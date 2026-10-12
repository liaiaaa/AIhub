package main

// execCmd / execCmdContext：本程序所有「顺手调用的系统命令」子进程统一走这里，
// 保证 Windows 下不弹黑框（见 exec_hidden_windows.go 的说明）。
// 新增 exec.Command 调用点时不要直接用标准库，改用这两个封装。

import (
	"context"
	"os/exec"
)

func execCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = sysProcAttrHideWindow()
	return cmd
}

func execCmdContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = sysProcAttrHideWindow()
	return cmd
}
