package main

// 控制台是**单文件内嵌**的：整页 HTML + 全部 JS 都在 webHTML 这一个反引号字符串里。
// 好处是没有构建步骤，代价是 Go 侧编译器和 go vet 完全不认识里面的 JS ——
// 我在 2026-10-17 就犯过一次：给 SERVICES 加注释时多写了一行 `const SERVICES = [`，
// Go 编译通过、go vet 干净、Go 测试全绿，**部署上去整个控制台直接白屏**（JS 语法错误）。
// 所以这里补一道：把页面里的 <script> 抽出来交给 node --check（没有 node 就跳过）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestConsoleJSParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机没有 node，跳过 JS 语法校验")
	}
	all := scriptRE.FindAllStringSubmatch(webHTML, -1)
	if len(all) == 0 {
		t.Fatal("webHTML 里没找到 <script> 块")
	}
	var js strings.Builder
	for i, m := range all {
		// 只校验内联脚本；外链 <script src> 的捕获组为空。
		if strings.TrimSpace(m[1]) == "" {
			continue
		}
		js.WriteString(m[1])
		js.WriteString("\n")
		_ = i
	}
	if js.Len() == 0 {
		t.Fatal("内联脚本为空")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "console.js")
	if err := os.WriteFile(f, []byte(js.String()), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", f).CombinedOutput()
	if err != nil {
		t.Fatalf("控制台 JS 语法错误（部署后整页会白屏）：%s\n%s", err, firstProblems(string(out), f))
	}
}

// 重复声明检查：node --check 其实已经会报，但报错信息含糊（"Unexpected token 'const'"
// 不告诉你哪个名字重复），所以单独查一遍，直接把名字列出来。
func TestNoDuplicateTopLevelDeclarations(t *testing.T) {
	seen := map[string]int{}
	for _, m := range declRE.FindAllStringSubmatch(webHTML, -1) {
		seen[m[1]+" "+m[2]]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("顶层声明 %q 出现 %d 次 —— 内嵌 JS 里这是语法错误，整页会白屏", k, n)
		}
	}
}

var (
	scriptRE = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	declRE   = regexp.MustCompile(`(?m)^const ([A-Z_][A-Z0-9_]*) = |^function ([A-Za-z][A-Za-z0-9_]*)\(`)
)

// firstProblems 把 node 的报错裁剪成前几行，并把临时路径去掉，免得测试输出刷屏。
func firstProblems(out, f string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 8 {
		lines = lines[:8]
	}
	for i, l := range lines {
		lines[i] = strings.ReplaceAll(l, f, "<console.js>")
	}
	return strings.Join(lines, "\n")
}

// 凭证单选框的 name 必须按线路分开。
//
// 浏览器里 radio 的分组是**整个文档**范围内按 name 算的，不按容器：两条线路
// 都用 name="cred" 时，后渲染的那条会把前一条的选中态顶掉，卡片上就看不出
// 「当前在用哪个凭证」（2026-10-17 用户报）。已用真实 Chrome 对照复现：
// 共用 name → A 组选中被清空；分线 name → 两组各自保留。
func TestCredentialRadioNamesAreLineScoped(t *testing.T) {
	if !strings.Contains(webHTML, `inp.name = 'cred-' + line`) {
		t.Error("凭证单选框的 name 没有按线路分开，两条线会互相顶掉选中态")
	}
	if strings.Contains(webHTML, `inp.name = 'cred';`) {
		t.Error("残留共用 name=\"cred\" 的写法")
	}
}
