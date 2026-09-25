package main

// 平台相关：端口探测/清扫、进程查杀、浏览器打开、控制台日志
// 由 main.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------- 端口工具 ----------

func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 150*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func pidsOnPort(port int) []int {
	var pids []int
	out, err := exec.Command("netstat", "-ano").Output()
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
	exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// isOwnProcessName 判断 pid 是否就是本程序（同名的另一个 hub 实例）。
func isOwnProcessName(pid int) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	want := strings.ToLower(filepath.Base(self))
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), `"`+want+`"`)
}

func stopPort(port int) bool {
	if portListening(port) {
		for _, pid := range pidsOnPort(port) {
			killPid(pid)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return !portListening(port)
}

// sweepPorts 启动时连续清扫，默认全部停止。
func sweepPorts() {
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		a := stopPort(DomPort)
		b := stopPort(IntlPort)
		c := stopPort(MimoPort)
		d := stopPort(QoderPort)
		if a && b && c && d {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func openBrowser(port int) {
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func logHub(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	if f, err := os.OpenFile(dataPath("hub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		f.WriteString(line)
		f.Close()
	}
}
