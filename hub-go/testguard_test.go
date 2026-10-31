package main

// 测试环境守卫（2026-10-31 A5）。
//
// 背景：仓库里有 50+ 个「抓包探针」测试（zz_*_test.go）和 8 个「真机回归」
// 测试（*_live_test.go）。它们不是单元测试：
//   - zz_* 会读本机真实凭证目录（domCredDirs 指向 各国workbuddy凭证 等），
//     再向真实上游发请求 —— 跑一次就是真的花额度、真的增加风控信号；
//   - *_live_test.go 会打真实上游并依赖本机登录态。
// 默认跑 `go test ./...` 时这些测试会全部执行，既慢又危险（2026-10-27 就发生过
// 测试把 ~/.codex/aihub-model-catalog.json 写空的事故）。
//
// 口径（A5）：
//   - 纯逻辑测试（解析/状态机/序列化/路由）保持默认执行；
//   - 会读真实凭证、打真实上游的测试，必须在函数开头调用 requireLive(t)，
//     默认 Skip，只有显式 `AIHUB_LIVE_TEST=1 go test ./...` 才跑。
//
// 已有的 8 个 *_live_test.go 自带各自的厂商级门禁（QODER_LIVE_TEST 等），
// 保持原样；requireLive 用于此前完全没有门禁的抓包探针。

import (
	"os"
	"testing"
)

// liveTestEnv 打开真机/抓包测试的开关。
const liveTestEnv = "AIHUB_LIVE_TEST"

// requireLive 声明「本测试会读真实凭证或打真实上游」：默认跳过。
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv(liveTestEnv) != "1" {
		t.Skip("抓包/真机测试默认跳过（会读真实凭证并发真实请求）；设 " + liveTestEnv + "=1 才运行")
	}
}

// consoleSource 返回控制台页面全文。
//
// 2026-10-31 A3 之后页面正文在 assets/console.html（go:embed 进 webHTML），
// 不在 webui.go 源码里了。原先几个「拿源码文本断言前端公式」的测试是
// os.ReadFile("webui.go")，拆分后会读不到内容而误报，统一改走这里。
func consoleSource(t *testing.T) string {
	t.Helper()
	if webHTML == "" {
		t.Fatal("webHTML 为空：go:embed assets/console.html 没生效")
	}
	return webHTML
}
