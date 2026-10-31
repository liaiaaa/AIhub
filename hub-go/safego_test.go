package main

// 后台 goroutine 兜底的回归测试（2026-10-31 A6）。
//
// 断言方式很直接：在 goroutine 里 panic，如果 recoverGoroutine 没兜住，
// 整个测试进程会被带走（Go 的 goroutine panic 不可恢复，test binary 直接死），
// 测试自然失败。所以「这条测试能过」就等于「兜底真的生效」。

import (
	"testing"
	"time"
)

func TestRecoverGoroutineSwallowsPanic(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverGoroutine("safego_test.go:TestRecoverGoroutineSwallowsPanic")
		panic("这条 panic 应该被兜住，进程继续活着")
	}()

	select {
	case <-done:
		// 正常：panic 被兜住，defer 链继续跑完
	case <-time.After(3 * time.Second):
		t.Fatal("goroutine 在 3 秒内没有结束：兜底可能没生效")
	}
}

// TestRecoverGoroutineNoPanic 没有 panic 时不产生副作用。
func TestRecoverGoroutineNoPanic(t *testing.T) {
	func() {
		defer recoverGoroutine("safego_test.go:TestRecoverGoroutineNoPanic")
		// 正常返回
	}()
}
