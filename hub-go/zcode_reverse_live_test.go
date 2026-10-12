package main

// Zcode 逆向接入真机 smoke（只读链路，不消耗额度、不碰验证码）：
//   1) 实时模型目录 client/configs（公开）
//   2) 套餐额度 billing/balance（JWT + 设备号 + API Key 三头）
//   3) 端点路由 agent/configs（当期 mapping 允许为空，只验链路 200）
//   4) 身份头值与本机探测（版本/设备号/OS 构建号）
// 默认跳过；启用：$env:ZCODE_LIVE_TEST="1"; go test -run TestZcodeLiveReverse -v

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestZcodeLiveReverse(t *testing.T) {
	if os.Getenv("ZCODE_LIVE_TEST") == "" {
		t.Skip("跳过真机 smoke；设置 ZCODE_LIVE_TEST=1 以启用")
	}

	// ① 身份探测
	id := zcodeLoadIdentity()
	t.Logf("appVersion=%q platform=%q osVersion=%q timezone=%q deviceMid=%.8s...",
		id.appVersion, id.platformParam(), id.osVersion, id.clientTimezone, id.deviceMid)
	if id.appVersion == "" {
		t.Error("未能探测桌面端版本（runtime/provider 目录扫描失败）")
	}
	if id.deviceMid == "" {
		t.Error("未能读取 deviceMid（telemetry-state.json）")
	}

	p := NewZcodeProxy()
	p.identity = id
	p.client = directClient()
	a, err := NewZcodeAuth()
	if err != nil {
		t.Fatalf("解密桌面端凭据失败: %v", err)
	}
	p.auth = a
	p.apiKey = a.Key()
	t.Logf("凭据: individual 优先选取 key_head=%.6s... jwt_len=%d", a.Key(), len(a.JWT()))

	// ② 实时目录
	n, err := p.refreshCatalog()
	if err != nil {
		t.Errorf("refreshCatalog: %v", err)
	} else {
		t.Logf("实时目录: %d 个模型（含 %v）", n, catalogSampleKeys(p.catalogSnapshot(), 4))
		if n <= 0 {
			t.Error("实时目录为空")
		}
		if e := p.entryFor("glm-5.3"); e == nil {
			t.Error("实时目录缺 glm-5.3")
		}
	}

	// ③ 套餐额度
	buckets, qerr := p.quotaSnapshot()
	if qerr != "" {
		t.Errorf("额度查询: %s", qerr)
	}
	for _, b := range buckets {
		t.Logf("额度: %s %s 剩余 %d/%d（能力 %v）", b.PlanName, b.ShowName, b.RemainingUnits, b.TotalUnits, b.Capabilities)
	}
	if len(buckets) == 0 {
		t.Error("额度分桶为空")
	}

	// ④ 端点路由（fail-open：mapping 为空也是合法结果，链路必须不报错）
	target := p.Endpoint + "/chat/completions"
	got := p.routing.resolve(target)
	t.Logf("端点路由: %s → %s（快照 %d 条）", target, got, p.routing.SnapshotSize())
	if got == "" {
		t.Error("路由 resolve 返回空 URL")
	}

	// ⑤ 中继探测 + 免费额度端到端（需 tools\zcode-relay\start-zcode-relay.bat 已启动）
	p.detectRelay(true)
	url, _, relayOn := p.relayTarget()
	t.Logf("中继探测: %q (on=%v)", url, relayOn)
	if !relayOn {
		t.Skip("本地中继未启动（8807），跳过端到端；运行 tools\\zcode-relay\\start-zcode-relay.bat 后重试")
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      "glm-5.3",
		"stream":     false,
		"max_tokens": 20,
		"messages":   []map[string]any{{"role": "user", "content": "reply with exactly: OK"}},
	})
	resp, err := p.zcodeUpstream(context.Background(), payload)
	if err != nil {
		t.Fatalf("中继端到端 zcodeUpstream: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		t.Fatalf("中继端到端 HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &out) != nil || len(out.Choices) == 0 {
		t.Fatalf("中继端到端响应解析失败: %s", truncate(string(body), 300))
	}
	t.Logf("免费额度端到端: model 回复 %q", strings.TrimSpace(out.Choices[0].Message.Content))
}

// 模拟控制台「留空 API Key 点启动」的完整路径：Start() 自动解密桌面端凭据、
// 自动探测中继、健康/状态行透出免费额度通道与剩余额度摘要。
func TestZcodeLiveConsoleStart(t *testing.T) {
	if os.Getenv("ZCODE_LIVE_TEST") == "" {
		t.Skip("跳过真机 smoke；设置 ZCODE_LIVE_TEST=1 以启用")
	}
	p := NewZcodeProxy()
	p.Port = 18796 // 不占用 8796，避免与正在运行的 hub 冲突
	ok, msg := p.Start()
	if !ok {
		t.Fatalf("空 API Key 启动失败: %s", msg)
	}
	defer p.Stop()

	// /health 应显示中继模式（自拉起有界等待 12s + 探测，轮询最多 30s）
	deadline := time.Now().Add(30 * time.Second)
	var h struct {
		Relay map[string]any  `json:"relay"`
		Quota json.RawMessage `json:"quota"`
	}
	mode := ""
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", p.Port))
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if json.Unmarshal(body, &h) == nil {
			mode, _ = h.Relay["mode"].(string)
			if mode == "start_plan_free_quota" {
				break
			}
		}
		time.Sleep(time.Second)
	}
	t.Logf("health relay.mode = %q", mode)
	if mode != "start_plan_free_quota" {
		t.Errorf("relay.mode = %q, want start_plan_free_quota（中继栈自拉起未就绪）", mode)
	}

	// 控制台状态行（等首轮额度后台拉取完成）
	time.Sleep(4 * time.Second)
	detail := zcodeStatusDetail(p)
	t.Logf("控制台状态行: %s", detail)
	if !strings.Contains(detail, "免费额度中继") {
		t.Errorf("状态行应含「免费额度中继」: %s", detail)
	}
}

// 生命周期：Stop 应收掉本实例拉起的 mint（8809）与中继（8807）。
func TestZcodeLiveRelayStackStop(t *testing.T) {
	if os.Getenv("ZCODE_LIVE_TEST") == "" {
		t.Skip("跳过真机 smoke；设置 ZCODE_LIVE_TEST=1 以启用")
	}
	p := NewZcodeProxy()
	p.Port = 18797
	if ok, msg := p.Start(); !ok {
		t.Fatalf("启动失败: %s", msg)
	}
	// 等自拉起完成（有界等待 12s + 余量）
	time.Sleep(14 * time.Second)
	if _, _, on := p.relayTarget(); !on {
		t.Skip("中继栈未就绪，跳过停止验证")
	}
	p.Stop()
	for _, port := range []int{8807, 8809} {
		deadline := time.Now().Add(5 * time.Second)
		released := false
		for time.Now().Before(deadline) {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				ln.Close()
				released = true
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Logf("端口 %d 释放: %v", port, released)
		if !released {
			t.Errorf("Stop 后端口 %d 仍被占用（本实例拉起的进程未收干净）", port)
		}
	}
}

func catalogSampleKeys(entries []zcodeModelEntry, n int) []string {
	keys := make([]string, 0, n)
	for i, e := range entries {
		if i >= n {
			break
		}
		keys = append(keys, e.Key)
	}
	return keys
}
