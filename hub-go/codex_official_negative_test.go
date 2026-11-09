package main

import (
	"os"
	"testing"
	"time"
)

// codexBinPath 必须尊重 AIHUB_CODEX_BIN 覆盖（规格 5.1 第 1 条的可配置点）。
func TestCodexBinPathHonoursEnv(t *testing.T) {
	t.Setenv("AIHUB_CODEX_BIN", "/tmp/fake-codex")
	if got := codexBinPath(); got != "/tmp/fake-codex" {
		t.Errorf("codexBinPath() = %q，应为 /tmp/fake-codex", got)
	}
	t.Setenv("AIHUB_CODEX_BIN", "   ")
	if got := codexBinPath(); got != "/usr/lib/chatgpt/resources/codex" {
		t.Errorf("空白 env 应回落默认路径，实际 %q", got)
	}
}

// 失败要负缓存：Codex 未安装时连续调用只许 fork/exec 一次。
// 该函数在 /backend-api/codex/responses 的热路径上，不做负缓存会每次请求都 exec。
func TestCodexOfficialSlugSetNegativeCache(t *testing.T) {
	isolateOfficialDataRoot(t)
	officialSlugMu.Lock()
	officialSlugCache = nil
	officialSlugFailedAt = time.Time{}
	officialSlugMu.Unlock()
	t.Cleanup(func() {
		officialSlugMu.Lock()
		officialSlugCache = nil
		officialSlugFailedAt = time.Time{}
		officialSlugMu.Unlock()
	})

	calls := 0
	old := codexBundledJSON
	codexBundledJSON = func() ([]byte, error) {
		calls++
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { codexBundledJSON = old })

	for i := 0; i < 5; i++ {
		if got := codexOfficialSlugSet(); len(got) != 0 {
			t.Fatalf("第 %d 次应返回空集合，实际 %v", i+1, got)
		}
	}
	if calls != 1 {
		t.Errorf("5 次调用触发了 %d 次 exec，应为 1（负缓存未生效）", calls)
	}
}
