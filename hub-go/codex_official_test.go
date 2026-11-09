package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// bundledSample 是本机 `codex debug models --bundled` 的真实形态（截取 3 条）。
const bundledSample = `{"models":[
 {"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","visibility":"list","priority":1,"description":"d"},
 {"slug":"codex-auto-review","display_name":"Codex Auto Review","visibility":"hide","priority":43,"description":"d"},
 {"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","priority":5,"description":"d"}
]}`

// isolateOfficialDataRoot 把数据根指到临时目录（官方 slug 集合落在这里）。
func isolateOfficialDataRoot(t *testing.T) string {
	t.Helper()
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })
	return dir
}

// withBundledJSON 替换内置目录来源，避免测试依赖本机是否装了 Codex。
func withBundledJSON(t *testing.T, raw string) {
	t.Helper()
	old := codexBundledJSON
	codexBundledJSON = func() ([]byte, error) { return []byte(raw), nil }
	t.Cleanup(func() { codexBundledJSON = old })
}

func TestCodexOfficialEntriesSortedListFirst(t *testing.T) {
	withBundledJSON(t, bundledSample)
	entries, err := codexOfficialEntries()
	if err != nil {
		t.Fatalf("取官方条目失败：%v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("条目数 = %d，应为 3", len(entries))
	}
	// visibility=list 必须排在 hide 之前
	if entries[len(entries)-1]["slug"] != "codex-auto-review" {
		t.Errorf("hide 条目应排最后，实际最后一条 = %v", entries[len(entries)-1]["slug"])
	}
	// 官方字段不许被改写
	if entries[0]["display_name"] == nil || entries[0]["description"] == nil {
		t.Error("官方条目的 display_name/description 丢了")
	}
}

func TestCodexOfficialEntriesRejectsEmpty(t *testing.T) {
	withBundledJSON(t, `{"models":[]}`)
	if _, err := codexOfficialEntries(); err == nil {
		t.Fatal("空目录必须报错，否则会写出空 catalog")
	}
}

func TestCodexOfficialSlugsRoundTrip(t *testing.T) {
	dir := isolateOfficialDataRoot(t)
	slugs := []string{"gpt-6.1-sol", "codex-auto-review"}
	if err := saveCodexOfficialSlugs(slugs); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	path := filepath.Join(dir, codexOfficialSlugsFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("文件不存在：%v", err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("权限 = %v，应为 0600（含官方模型名单，不该世界可读）", st.Mode().Perm())
	}
	got := loadCodexOfficialSlugs()
	if len(got) != 2 || got[0] != "gpt-6.1-sol" {
		t.Fatalf("读回 = %v", got)
	}
	// 内存缓存必须同步生效：直接查包级缓存，不经 codexOfficialSlugSet
	// （走后者会读盘重填，删掉 save 里的 setOfficialSlugCache 也照样过 —— 那是空断言）
	officialSlugMu.RLock()
	cached := officialSlugCache
	officialSlugMu.RUnlock()
	if !cached["codex-auto-review"] {
		t.Errorf("save 之后内存缓存没同步：%v", cached)
	}
}

func TestCodexOfficialSlugSetFallsBackToBundle(t *testing.T) {
	isolateOfficialDataRoot(t) // 磁盘上没有任何 slug 文件
	officialSlugMu.Lock()
	oldCache, oldFailed := officialSlugCache, officialSlugFailedAt
	officialSlugCache, officialSlugFailedAt = nil, time.Time{}
	officialSlugMu.Unlock()
	t.Cleanup(func() {
		officialSlugMu.Lock()
		officialSlugCache, officialSlugFailedAt = oldCache, oldFailed
		officialSlugMu.Unlock()
	})
	withBundledJSON(t, bundledSample)

	set := codexOfficialSlugSet()
	if !set["gpt-6.1-sol"] || !set["codex-auto-review"] {
		t.Fatalf("回退重算没拿到官方 slug：%v", set)
	}
	// 回退重算不应写盘（只读路径）
	if _, err := os.Stat(codexOfficialSlugsPath()); err == nil {
		t.Error("只读重算不该落盘")
	}
}

func TestCodexOfficialSlugListSkipsEmptySlug(t *testing.T) {
	got := codexOfficialSlugList([]map[string]any{{"slug": "a"}, {"slug": ""}, {}})
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("slug 列表 = %v", got)
	}
}
