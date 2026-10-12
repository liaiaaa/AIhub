//go:build !windows

package main

// 平台相关（POSIX / Linux）：端口占用者查询、进程查杀、浏览器打开。
// 跨平台部分（portListening/stopPort/sweepPorts/logHub）在 platform.go。
//
// 查占用者一律读 /proc，不调 ss/lsof：少一个外部依赖，也和 Windows 侧
// 「所有系统命令都走 execCmd」的约定保持一致。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pidsOnPort 返回在 port 上处于 LISTEN 状态的进程 PID。
// 两步：/proc/net/tcp{,6} 找出该端口的 socket inode，再反查 /proc/<pid>/fd 的符号链接。
// 只能看到当前用户可读的进程——正是本程序要清扫的残留（自己启动的旧实例）。
func pidsOnPort(port int) []int {
	inodes := listenSocketLinks(port)
	if len(inodes) == 0 {
		return nil
	}
	var pids []int
	seen := map[int]bool{}
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", d.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // 不是本用户的进程，读不到直接跳过
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if inodes[link] {
				if !seen[pid] {
					seen[pid] = true
					pids = append(pids, pid)
				}
				break
			}
		}
	}
	return pids
}

// listenSocketLinks 收集 "socket:[inode]" 集合：监听 port 的所有 TCP socket。
func listenSocketLinks(port int) map[string]bool {
	out := map[string]bool{}
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue // 内核没暴露 IPv6 之类，能读到一个就够
		}
		for i, line := range strings.Split(string(b), "\n") {
			if i == 0 {
				continue // 表头
			}
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" { // 0A = TCP_LISTEN
				continue
			}
			colon := strings.LastIndex(f[1], ":")
			if colon < 0 {
				continue
			}
			pnum, err := strconv.ParseInt(f[1][colon+1:], 16, 32)
			if err != nil || int(pnum) != port {
				continue
			}
			out["socket:["+f[9]+"]"] = true
		}
	}
	return out
}

// killPid 只用于清理【启动前】残留在代理端口上的外部进程。
// 与 Windows 版同样的两道保险：不杀自己、不杀同类 hub 实例、
// 不杀正持有控制台端口（8801）的活跃实例。
func killPid(pid int) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	if isOwnProcessName(pid) {
		return
	}
	for _, cpid := range pidsOnPort(HubPort) {
		if cpid == pid {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// isOwnProcessName 判断 pid 是否就是本程序（同名的另一个 hub 实例）。
// exe 符号链接在二进制被重新编译覆盖后会带 " (deleted)" 后缀，需剥离。
func isOwnProcessName(pid int) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	target = strings.TrimSuffix(target, " (deleted)")
	return filepath.Clean(target) == filepath.Clean(self)
}

func openBrowser(port int) {
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if _, err := exec.LookPath("xdg-open"); err != nil {
		return // 无桌面环境（纯终端/SSH）时静默跳过，-no-browser 之外的兜底
	}
	execCmd("xdg-open", url).Start()
}
