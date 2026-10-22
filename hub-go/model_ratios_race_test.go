package main

import (
	"os"
	"sync"
	"testing"
)

// TestUserRatiosConcurrentFirstLoad 回归测试：userRatios 是懒加载的全局表，
// 而读它的全在 HTTP 协程里（/v1/models、/api/gateway、codex 目录刷新）——
// 2026-10-21 开源审查前完全没有同步，`go test -race` 实报
// loadUserRatios 的读与写数据竞争，进一步并发进入 map 就是运行时 fatal。
// 修法见 model_ratios.go 顶部注释。
func TestUserRatiosConcurrentFirstLoad(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() {
		hubRoot = old
		userRatios = nil
	})
	// 造一个用户倍率表文件，确保首载走 Unmarshal 分支
	body := `{"dom":{"glm-5.3":0.79},"intl":{"deepseek-v4.1-flash":0.1}}`
	if err := os.WriteFile(userRatiosPath(), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	// 8 个并发读者（模拟 /v1/models、/api/gateway、codex 目录刷新同时首拉）
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = (&Hub{}).modelRatio("dom", "glm-5.3")
		}()
	}
	wg.Wait()
}
