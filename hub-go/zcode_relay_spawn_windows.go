//go:build windows

package main

// 中继栈自拉起：控制台点「启动 Zcode」时（ZcodeProxy.Start），若本地中继
// （8807）不在运行，自动把「真浏览器铸币服务（mint, 8809）+ 中继引擎
// （zcode-relay.exe）」拉起来；点「停止」时一并收掉本实例拉起的进程。
//
// 设计约束：
//   - 部署目录不存在（tools\zcode-relay 未部署）时静默跳过；
//   - 子进程隐藏窗口（CREATE_NO_WINDOW）；
//   - 只管理本实例拉起的进程（外部手动启动的不动）；孤儿无害：
//     下次启动探测到 8807 已在运行就直接复用，不会重复拉起；
//   - node.exe 缺失时 mint 拉起失败，引擎 auto 后端自动回退 happy-dom。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// hideWindowProcAttr Windows 子进程不弹窗口。
func hideWindowProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// relayStackLog 打开子进程日志文件（追加），让拉起的 mint/中继日志可诊断。
func relayStackLog(path string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

func (p *ZcodeProxy) spawnRelayStack() {
	relayDir := filepath.Join(hubRoot, "tools", "zcode-relay")
	exe := filepath.Join(relayDir, "zcode-relay.exe")
	mintScript := filepath.Join(relayDir, "mint", "mint.mjs")
	if _, err := os.Stat(exe); err != nil {
		return // 中继未部署，跳过
	}
	// 用户显式关闭中继（ZCODE_RELAY_URL=off/direct/none）时不自拉起
	if v := strings.TrimSpace(os.Getenv("ZCODE_RELAY_URL")); v != "" {
		lv := strings.EqualFold
		if lv(v, "off") || lv(v, "direct") || lv(v, "none") {
			return
		}
	}
	// 已启用中继（刚探测成功）则无事可做
	if url, _, ok := p.relayTarget(); ok {
		_ = url
		return
	}

	// 1) mint 服务（真浏览器铸币，8809）：2026-09-29 官方关闭模型请求验证码
	//    校验（configs.captcha.skip_model_request=true），引擎已打补丁尊重该
	//    标记——不再需要铸币，默认不拉起（省 400MB Edge 常驻）。
	//    若官方重新启用验证码：手动 `node mint\mint.mjs` 即可恢复兜底。
	if !portListening(8809) {
		if _, err := os.Stat(mintScript); err == nil {
			if node, err := exec.LookPath("node"); err == nil {
				_ = node // mint 保留为可选兜底，见上；默认不启动
			}
		}
	}

	// 2) 中继引擎（8807）
	if portListening(8807) {
		return // 已在运行（上次拉起的或手动起的），直接复用
	}
	cmd := exec.Command(exe, "serve", filepath.Join(relayDir, "config.yaml"))
	cmd.Dir = relayDir
	cmd.Env = append(os.Environ(),
		"ZCODE_CAPTCHA_BACKEND=auto",
		"ZCODE_CAPTCHA_MINT_URL=http://127.0.0.1:8809/mint",
		"CAPTCHA_POOL_MIN=3",
		"CAPTCHA_POOL_MAX=12",
		"CAPTCHA_SOLVE_CONCURRENCY=2",
	)
	cmd.SysProcAttr = hideWindowProcAttr()
	if f := relayStackLog(filepath.Join(relayDir, "relay.log")); f != nil {
		cmd.Stdout, cmd.Stderr = f, f
		defer f.Close()
	}
	if err := cmd.Start(); err != nil {
		p.logf("中继引擎拉起失败：%v", err)
		return
	}
	p.relayProbeMutex.Lock()
	p.spawnedRelayCmd = cmd
	p.relayProbeMutex.Unlock()
	p.logf("已拉起本地 start-plan 中继（zcode-relay, 8807），等待就绪…")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if portListening(8807) {
			time.Sleep(time.Second) // 引擎初始化缓冲
			return
		}
	}
	p.logf("中继引擎拉起后 15s 内未就绪，交由后台循环兜底重探")
}

// stopSpawnedRelayStack 点「停止」时收掉本实例拉起的 mint/中继进程。
// 外部手动启动（bat 等）的不在本实例 spawned 记录里，不受影响。
func (p *ZcodeProxy) stopSpawnedRelayStack() {
	p.relayProbeMutex.Lock()
	mint, relay := p.spawnedMintCmd, p.spawnedRelayCmd
	p.spawnedMintCmd, p.spawnedRelayCmd = nil, nil
	p.relayProbeMutex.Unlock()
	for _, c := range []*exec.Cmd{mint, relay} {
		if c != nil && c.Process != nil {
			_ = c.Process.Kill()
		}
	}
	if mint != nil || relay != nil {
		time.Sleep(300 * time.Millisecond) // 端口释放缓冲
	}
}
