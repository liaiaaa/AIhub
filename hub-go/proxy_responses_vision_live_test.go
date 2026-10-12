package main

// 端到端验证（真实上游、独立端口）：图片在**工具结果**里时，模型到底看不看得见。
//
// 这是 2026-10-12 用户报障的复现/回归：
//   「同一张图、同一个模型（deepseek-v4.1-flash 国际），DSH 能找到投递邮箱，
//     Codex 找不到还胡扯。」
// 根因是 Codex 的看图路径是 view_image 工具 → 图片落在 function_call_output 里，
// 而 Responses→chat 翻译层那条路只取文本，把图片静默丢了。
//
// 默认跳过；启用：INTL_LIVE_TEST=1 go test -run TestLiveToolResultImage -v
// 使用临时端口 + 真实凭证，不影响正在运行的 hub（8790/8799）。

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)


// ---------- 测试用的小工具（造图 / 拼 PNG 分块） ----------

// solidPNGDataURL 生成一张纯色 PNG 的 data URL，避免依赖外部图片文件。
func solidPNGDataURL(w, h int, rgb [3]byte) string {
	raw := make([]byte, 0, (w*3+1)*h)
	for y := 0; y < h; y++ {
		raw = append(raw, 0) // filter type 0
		for x := 0; x < w; x++ {
			raw = append(raw, rgb[0], rgb[1], rgb[2])
		}
	}
	var idat bytes.Buffer
	zw := zlib.NewWriter(&idat)
	zw.Write(raw)
	zw.Close()
	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	writePNGChunk(&out, "IHDR", []byte{
		byte(w >> 24), byte(w >> 16), byte(w >> 8), byte(w),
		byte(h >> 24), byte(h >> 16), byte(h >> 8), byte(h),
		8, 2, 0, 0, 0,
	})
	writePNGChunk(&out, "IDAT", idat.Bytes())
	writePNGChunk(&out, "IEND", nil)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(out.Bytes())
}

// writePNGChunk 写一个 PNG 分块（长度 + 类型 + 数据 + CRC32）。
func writePNGChunk(w io.Writer, typ string, data []byte) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(data)))
	copy(hdr[4:8], typ)
	w.Write(hdr[:])
	w.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], crc.Sum32())
	w.Write(c[:])
}

// probeCred 用一句纯文本确认该凭证没被风控。
func probeCred(t *testing.T, p *Proxy) bool {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash", "stream": false,
		"input": []any{map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "说一个字：好"}}}},
	})
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Post(
		fmt.Sprintf("http://127.0.0.1:%d/v1/responses", p.Port), "application/json", bytes.NewReader(body))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func TestLiveToolResultImage(t *testing.T) {
	if os.Getenv("INTL_LIVE_TEST") == "" {
		t.Skip("跳过真实上游测试；设置 INTL_LIVE_TEST=1 启用")
	}
	hubRoot = repoRoot(t)
	// 逐个凭证试：11140 是账号级风控，跟请求内容无关，换一个凭据继续。
	want := os.Getenv("INTL_LIVE_CRED")
	cands := []string{}
	if want != "" {
		cands = append(cands, want)
	}
	cd := credDirs()
	for _, k := range []string{"<凭证目录D>", "<凭证目录B>", "<凭证目录C>", "<凭证目录A>"} {
		if v, ok := cd[k]; ok && dirHasCredential(v) {
			cands = append(cands, k)
		}
	}
	var p *Proxy
	var picked string
	for _, k := range cands {
		dir := cd[k]
		if dir == "" {
			dir = k
		}
		cand := NewIntlProxy(dir)
		l2, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		cand.Port = l2.Addr().(*net.TCPAddr).Port
		l2.Close()
		if ok, msg := cand.Start(); !ok {
			t.Logf("凭证 %s 启动失败：%s", k, msg)
			continue
		}
		if ok := probeCred(t, cand); ok {
			p, picked = cand, k
			break
		}
		cand.Stop()
		t.Logf("凭证 %s 被风控（11140），换下一个", k)
	}
	if p == nil {
		t.Skip("所有国际凭证当前都命中 11140 风控，跳过真实上游验证")
	}
	t.Logf("使用凭证：%s", picked)
	defer p.Stop()
	base := fmt.Sprintf("http://127.0.0.1:%d/v1/responses", p.Port)
	client := &http.Client{Timeout: 300 * time.Second}

	img := solidPNGDataURL(64, 64, [3]byte{220, 30, 30})
	call := func(name string, input []any) string {
		body, _ := json.Marshal(map[string]any{
			"model": "deepseek-v4.1-flash", "stream": false, "input": input,
		})
		resp, err := client.Post(base, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s 请求失败：%v", name, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		t.Logf("%s HTTP %d raw=%s", name, resp.StatusCode, truncate(string(b), 400))
		var d map[string]any
		json.Unmarshal(b, &d)
		var txt strings.Builder
		if outs, ok := d["output"].([]any); ok {
			for _, it := range outs {
				im, _ := it.(map[string]any)
				if cs, ok := im["content"].([]any); ok {
					for _, c := range cs {
						cm, _ := c.(map[string]any)
						if cm["type"] == "output_text" {
							txt.WriteString(fmt.Sprintf("%v", cm["text"]))
						}
					}
				}
			}
		}
		t.Logf("=== %s ===\n%s", name, txt.String())
		return txt.String()
	}

	// A：图片直接在用户消息里（第一次修复已覆盖）
	a := call("A 图片在用户消息里", []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "这张图是什么颜色？只答颜色。"},
			map[string]any{"type": "input_image", "image_url": img, "detail": "high"},
		}},
	})
	// B：图片在工具结果里（Codex view_image 的真实形态）
	b := call("B 图片在工具结果里", []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "用 view_image 看这张图，告诉我它是什么颜色。只答颜色。"},
		}},
		map[string]any{"type": "function_call", "name": "view_image", "call_id": "c1",
			"arguments": `{"path":"/tmp/probe.png"}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": []any{
			map[string]any{"type": "input_image", "image_url": img, "detail": "high"},
		}},
	})

	if !strings.Contains(a, "红") {
		t.Errorf("A（图片在用户消息）未答出红色：%q", a)
	}
	if !strings.Contains(b, "红") {
		t.Errorf("B（图片在工具结果）未答出红色 —— 工具结果里的图片仍被丢弃：%q", b)
	}
}
