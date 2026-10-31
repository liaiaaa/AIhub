package main

// 平台无关的端口与日志工具：两端（Windows / POSIX）共用。
// 平台专属部分（占用者查询、查杀、打开浏览器）见 platform_windows.go / platform_other.go。

import (
	"fmt"
	"net"
	"os"
	"time"
)

func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 150*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
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
// 端口清单来自 lineSpecs（见 lines.go），不再逐个列常量——
// 加一条线路只改那张表，这里自动覆盖。
func sweepPorts() {
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		allFree := true
		for _, port := range allLinePorts() {
			if !stopPort(port) {
				allFree = false
			}
		}
		if allFree {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func logHub(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	if f, err := os.OpenFile(dataPath("hub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		f.WriteString(line)
		f.Close()
	}
}
