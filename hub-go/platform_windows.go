package main

// 平台相关（Windows）：端口占用者查询、进程查杀、浏览器打开。
// 跨平台部分（portListening/stopPort/sweepPorts/logHub）在 platform.go。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func pidsOnPort(port int) []int {
	var pids []int
	out, err := execCmd("netstat", "-ano").Output()
	if err != nil {
		return pids
	}
	needle := fmt.Sprintf(":%d ", port)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "LISTENING") || !strings.Contains(line, needle) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			if pid, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	seen := map[int]bool{}
	uniq := pids[:0]
	for _, p := range pids {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}

// killPid 只用于清理【启动前】残留在代理端口上的外部进程。
// 两道保险：
//  1. 绝不杀自己——代理跑在本进程内，端口上的 PID 往往就是 hub 自己，
//     早期版本在 Stop() 里调用它，等于「点停止 -> 自杀」。
//  2. 绝不杀同类 hub 进程——清扫的目标是残留（崩溃的旧进程 / Python 版转换器），
//     不是正在正常服务的另一个控制台实例。
func killPid(pid int) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	if isOwnProcessName(pid) {
		return
	}
	// 目标进程正持有默认控制台端口（8801）：它是另一个活跃控制台实例，
	// 即使 exe 改名（isOwnProcessName 漏判）也绝不杀。
	for _, cpid := range pidsOnPort(HubPort) {
		if cpid == pid {
			return
		}
	}
	execCmd("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// isOwnProcessName 判断 pid 是否就是本程序（同名的另一个 hub 实例）。
func isOwnProcessName(pid int) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	want := strings.ToLower(filepath.Base(self))
	out, err := execCmd("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), `"`+want+`"`)
}

func openBrowser(port int) {
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	execCmd("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
