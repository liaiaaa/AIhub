//go:build !windows

package main

// 中继栈自拉起（非 Windows / Linux 版，对照 zcode_relay_spawn_windows.go）：
// 控制台点「启动 Zcode」时若本地中继（8807）不在运行，自动拉起
// zcode-relay-linux（tools/zcode-relay/，bun build --compile 产物）。
//
// 与 Windows 版的差异：
//   - 不拉 mint 铸币服务（官方 2026-09-29 起模型请求不再强制验证码，
//     且 mint 依赖 Playwright + 系统 Edge，Linux 部署形态没有）；
//   - 凭据库缺失时先跑 node inject-credential.mjs 从桌面端凭据注入；
//   - 引擎二进制按 zcode-relay-linux → zcode-relay 顺序找。
//
// 其余语义一致：部署缺失静默跳过；只管理本实例拉起的进程（外部
// 手动 start-zcode-relay.sh 起的不动）；孤儿无害，下次启动直接复用。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// relayStackLog 打开子进程日志文件（追加），让拉起的注入脚本/中继日志可诊断。
func relayStackLog(path string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return f
}

func (p *ZcodeProxy) spawnRelayStack() {
	relayDir := filepath.Join(hubRoot, "tools", "zcode-relay")
	exe := ""
	for _, name := range []string{"zcode-relay-linux", "zcode-relay"} {
		if cand := filepath.Join(relayDir, name); fileExists(cand) {
			exe = cand
			break
		}
	}
	if exe == "" {
		return // 中继未部署（未编译），跳过
	}
	// 用户显式关闭中继（ZCODE_RELAY_URL=off/direct/none）时不自拉起
	if v := strings.TrimSpace(os.Getenv("ZCODE_RELAY_URL")); v != "" {
		lv := strings.EqualFold
		if lv(v, "off") || lv(v, "direct") || lv(v, "none") {
			return
		}
	}
	// 已启用中继（刚探测成功）则无事可做
	if _, _, ok := p.relayTarget(); ok {
		return
	}
	if portListening(8807) {
		return // 已在运行（上次拉起的或手动起的），直接复用
	}

	// 凭据库缺失：先从桌面端凭据注入（node 脚本，与 start-zcode-relay.sh 同一步）
	home, _ := os.UserHomeDir()
	credPath := filepath.Join(home, ".zcode-proxy", "credentials.json")
	if !fileExists(credPath) {
		if node, err := exec.LookPath("node"); err == nil {
			cmd := execCmd(node, "inject-credential.mjs")
			cmd.Dir = relayDir
			if f := relayStackLog(filepath.Join(relayDir, "relay.log")); f != nil {
				cmd.Stdout, cmd.Stderr = f, f
				_ = cmd.Run()
				f.Close()
			} else {
				_ = cmd.Run()
			}
		}
	}

	cmd := execCmd(exe, "serve", filepath.Join(relayDir, "config.yaml"))
	cmd.Dir = relayDir
	cmd.Env = append(os.Environ(),
		"ZCODE_CAPTCHA_BACKEND=auto",
		"CAPTCHA_POOL_MIN=3",
		"CAPTCHA_POOL_MAX=12",
		"CAPTCHA_SOLVE_CONCURRENCY=2",
	)
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
	p.logf("已拉起本地 start-plan 中继（%s, 8807），等待就绪…", filepath.Base(exe))
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

// stopSpawnedRelayStack 点「停止」时收掉本实例拉起的中继进程。
// 外部手动启动（start-zcode-relay.sh 等）的不在本实例 spawned 记录里，不受影响。
func (p *ZcodeProxy) stopSpawnedRelayStack() {
	p.relayProbeMutex.Lock()
	relay := p.spawnedRelayCmd
	p.spawnedRelayCmd = nil
	p.relayProbeMutex.Unlock()
	if relay != nil && relay.Process != nil {
		_ = relay.Process.Kill()
		time.Sleep(300 * time.Millisecond) // 端口释放缓冲
	}
}
