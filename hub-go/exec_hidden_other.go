//go:build !windows

package main

// 非 Windows：没有「子进程弹控制台」问题，返回 nil 保持默认行为。

import "syscall"

func sysProcAttrHideWindow() *syscall.SysProcAttr { return nil }
