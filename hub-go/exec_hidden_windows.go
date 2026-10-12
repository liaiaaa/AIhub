//go:build windows

package main

// GUI 进程（-H=windowsgui 编译，无自身控制台）spawn 控制台程序
// （reg / netstat / tasklist / taskkill / runtime-info.exe）时，Windows 会给
// 子进程新建一个可见控制台——用户看到「黑色命令行框一闪而过」。
// CREATE_NO_WINDOW 压掉它。与 zcode_relay_spawn_windows.go 的
// hideWindowProcAttr 同一招；独立成对文件（_windows/_other）是为了让
// 跨平台文件（proxy_util.go / proxy_qoder.go）也能安全引用。

import "syscall"

func sysProcAttrHideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
