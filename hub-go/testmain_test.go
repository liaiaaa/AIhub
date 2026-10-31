package main

// 测试统一隔离（2026-10-31 A5.5）。
//
// 以前测试直接跑在仓库根（hubRoot 默认 = /home/<user>/opt/AI_test/hub-go）上，带来三个问题：
//  1. 测试写 state.json / rate_limit.json / usage.jsonl / hub.log 会污染真实数据根；
//  2. 测试之间共享同一份限流台账与用量账本，导致顺序相关的偶发失败
//     （TestHandleResponsesAbsorbs429EndToEnd 单独跑 3/3 通过，混跑偶尔红）；
//  3. 涉及 homeJoin / codex 目录的测试一旦走真实路径就会写用户目录
//     （2026-10-27 把 ~/.codex/aihub-model-catalog.json 写空那次就是这么来的）。
//
// 现在 TestMain 在跑任何测试之前把 hubRoot 与 userHomeDir 都指到临时目录：
//  - 想保留现场排查：AIHUB_TEST_KEEP_ROOT=1 go test ...
//  - 想指定目录：      AIHUB_TEST_ROOT=/tmp/xxx go test ...

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	root := os.Getenv("AIHUB_TEST_ROOT")
	cleanup := false
	if root == "" {
		dir, err := os.MkdirTemp("", "aihub-test-root-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建测试数据根失败：%v\n", err)
			os.Exit(1)
		}
		root, cleanup = dir, true
	} else if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "准备 AIHUB_TEST_ROOT=%s 失败：%v\n", root, err)
		os.Exit(1)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建测试 home 失败：%v\n", err)
		os.Exit(1)
	}

	hubRoot = root
	userHomeDir = func() (string, error) { return home, nil }

	code := m.Run()

	if cleanup && os.Getenv("AIHUB_TEST_KEEP_ROOT") == "" {
		_ = os.RemoveAll(root)
	} else {
		fmt.Fprintf(os.Stderr, "测试数据根保留在：%s\n", root)
	}
	os.Exit(code)
}
