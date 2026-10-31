# Codex 间歇性打不开：`~/.codex/aihub-model-catalog.json` 被 `go test` 写成空表

> 诊断方：dsh 会话（只诊断，未改代码）。交给 hub-go 会话修。
> 日期：2026-10-27　优先级：高 —— 用户在用的 Codex 每次跑测试都会被打坏。

---

## 一、现象

Codex 桌面版启动时弹窗，完全打不开：

```
无法加载 Codex 配置
model_catalog_json path `/home/<user>/.codex/aihub-model-catalog.json`
must contain at least one model
```

用户描述是「**时不时**」出现，不是每次都出现。

## 二、复现（已实测，100% 稳定）

```sh
# 前置：文件此时是好的
stat -c '%s' ~/.codex/aihub-model-catalog.json     # 654901
# 内容 14 个模型

# 只跑这一个测试（注意 go 不在 PATH 里，用绝对路径）
cd /home/<user>/opt/AI_test/hub-go/hub-go
/usr/local/go/bin/go test -run 'TestCodexCatalogFileLocation' -count=1 .

# 输出： ok  aihub  0.006s       ← 测试"通过"

# 再看真实文件
stat -c '%s' ~/.codex/aihub-model-catalog.json     # 17
cat ~/.codex/aihub-model-catalog.json
# {
#  "models": []
# }
```

**测试通过，但用户真实的 Codex 配置文件被写成了空表。**

## 三、根因（两个缺陷叠加）

### 缺陷 1：`homeJoin` 无视 `hubRoot`，恒写真实 `$HOME`

`hub-go/client_takeover.go:33`

```go
func homeJoin(elem ...string) (string, error) {
	home, err := os.UserHomeDir()      // ← 永远是真实 $HOME
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, elem...)...), nil
}
```

`codex_catalog.go:157` 的写入路径：

```go
path, err := homeJoin(".codex", codexCatalogPath)   // → /home/<user>/.codex/aihub-model-catalog.json
```

测试里设的 `hubRoot = t.TempDir()` **对这个路径完全无效**，因为 `homeJoin` 根本不看 `hubRoot`
（`hubRoot` 只被 `dataPath()` 用于 `agg_models.json` / 日志 / state.json）。

### 缺陷 2：那个测试是**故意**用空白名单，并断言「空表也要写出去」

`hub-go/codex_catalog_display_test.go:96` `TestCodexCatalogFileLocation`

```go
h := &Hub{}
aggMu.Lock()
saved := aggList
aggList = []aggModel{}            // ← 故意清空白名单
aggMu.Unlock()

path, err := writeCodexCatalogFile(h)
if err != nil {
	t.Fatalf("空白名单也应能写出目录：%v", err)   // ← 断言"即使 0 条也要写文件"
}
```

两个缺陷一叠加：**测试把 `aggList` 清空 → 调真实写盘路径 → 把用户真实文件覆盖成 `{"models": []}` → 且写完不恢复该文件**（`t.Cleanup` 只恢复 `aggList` 和 `hubRoot`）。

### 为什么是「时不时」

`codex_catalog_display_test.go` 里有 **两个** 测试都会调 `writeCodexCatalogFile` 并直写真实文件：

| 测试 | 位置 | aggList 状态 | 写出的内容 |
|---|---|---|---|
| `TestCodexCatalogFileDisplay` | `codex_catalog_display_test.go:35` | 非空（测试假数据） | 假模型的目录 |
| `TestCodexCatalogFileLocation` | `codex_catalog_display_test.go:109` | **空** | `{"models": []}` |

`go test` 按函数名字典序执行，`...Display` 先、`...Location` 后，**最后落盘的是空表**。

所以：**用户在别的会话里每跑一次 `go test`（哪怕只是为了验证无关改动），Codex 的模型目录就被打回空表。** 而 Codex 桌面版

```go
// proxy_agg.go:405 现有注释已写明
// Codex 桌面版只在启动时读一次模型目录，…且不会自愈——只能重启桌面版。
```

空表一直躺着，用户下次开 Codex 就报错 —— 这就是「时不时」。

时间线印证：`trae_stream_test.go` 修改于 `12:20:39`，用户真实 catalog 文件被写空于 `12:22`。

## 四、修复方案

### 必修 1：把 home 解析做成可注入（测试隔离）

`client_takeover.go:33` 改成：

```go
// userHomeDir 可被测试覆盖，避免测试写真实用户目录。
// 2026-10-27：TestCodexCatalogFileLocation 曾把用户真实的
// ~/.codex/aihub-model-catalog.json 写成 {"models": []}，导致 Codex 桌面版
// 报 "must contain at least one model" 无法启动。
var userHomeDir = os.UserHomeDir

func homeJoin(elem ...string) (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, elem...)...), nil
}
```

测试里 override 到临时目录（`codex_catalog_display_test.go` 两个测试都要加）：

```go
oldHome := userHomeDir
userHomeDir = func() (string, error) { return t.TempDir(), nil }
t.Cleanup(func() { userHomeDir = oldHome })
```

> 注意 `TestCodexCatalogFileLocation` 现在设的 `hubRoot = dir` 可以保留，但它管不着这条路径，
> 真正起作用的是 `userHomeDir`。

### 必修 2：`writeCodexCatalogFile` 拒绝用 0 条覆盖

`codex_catalog.go:89`，在 `entries` 构建完、`json.MarshalIndent` 之前插入：

```go
if len(entries) == 0 {
	// 空表对桌面版毫无价值，但会让 Codex 直接拒绝启动（必须 >= 1 个 model），
	// 且它只在启动时读一次、不会自愈。保留现有文件永远比写空表好。
	logHub("⚠ 桌面版目录跳过：白名单 0 个模型，保留现有文件不覆盖")
	return "", fmt.Errorf("白名单为空，保留现有目录文件：%s", path)
}
```

（`path` 需提前算出来，或者把那两行 `homeJoin` 挪到 `entries` 判空之前。）

### 必修 3：同步改测试语义

`TestCodexCatalogFileLocation` 的本意是「验证落盘位置 + `.tmp` 原子替换」，
用空白名单只是图省事。改成：

- 用**非空** aggList（复用 `TestCodexCatalogFileDisplay` 里的假数据）
- 另外**新增**一个用例覆盖「0 条不落盘」：
  - `aggList = []aggModel{}`
  - `writeCodexCatalogFile` 应返回 `err != nil`
  - 目标文件应**未被创建**、或已存在时**内容/mtime 未变**

### 建议 4：成功路径补日志

`writeCodexCatalogFile` 现在**成功时完全静默**，失败才记日志。所以 12:22 那次写空在 `hub.log` 里查不到任何痕迹，只能靠反推。

```go
if _, err := os.Stat(path); err == nil {
	// 覆盖已有文件时记一笔，便于回溯
	logHub("桌面版目录已刷新：%d 个模型 → %s", len(entries), path)
}
```

## 五、附带修一个日志误报（会误导排查）

`hub-go/proxy_util.go:48`

```go
func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}
```

而 `aggOpenAIModels()` 返回的 `data` 类型是 `[]map[string]any`（Go 原生构造，不经 JSON 解析），
**类型断言必然失败**。所以 `proxy_agg.go:285`

```go
n := len(asSlice(out["data"]))   // ← 恒为 0
logHub("agg /v1/models 目录请求 | OpenAI 形态 | %d 个模型", n)
```

**这行日志永远是 `0 个模型`，不管实际返回几个。** 实测 `curl -s 127.0.0.1:8790/v1/models`
拿到 14 条，日志同时打「0 个模型」。

排查时极易被它带偏（本次诊断就被浪费了一轮）。

`codex 形态` 那行（`proxy_agg.go:276`）是对的，因为 `codexModelsResponseFrom` 返回的确实是 `[]any`。

最小修法（不动其它 12 处 `asSlice` 调用）：

```go
n := 0
if d, ok := out["data"].([]map[string]any); ok {
	n = len(d)
}
```

## 六、验收标准

```sh
# 1. 全量测试不再碰真实文件（最关键）
cp ~/.codex/aihub-model-catalog.json /tmp/catalog-before.json
cd /home/<user>/opt/AI_test/hub-go/hub-go
/usr/local/go/bin/go test ./...
diff /tmp/catalog-before.json ~/.codex/aihub-model-catalog.json \
  && echo "✅ 测试隔离生效，真实文件未被修改"

# 2. 空白名单不落盘（单元测试断言，见必修 3）

# 3. 日志不再误报
curl -s http://127.0.0.1:8790/v1/models | python3 -c "import json,sys;print(len(json.load(sys.stdin)['data']))"
# 该数字应与 hub.log 里 "OpenAI 形态 | N 个模型" 的 N 一致
```

## 七、环境提示（踩过的坑）

- **`go` 不在 PATH 里**，必须用绝对路径：`/usr/local/go/bin/go`
- `go.mod` 要求 `go 1.27`，本机是 `go1.27.2 linux/amd64`
- 当前生效的 aihub 由 transient systemd 单元 `aihub-console.service` 承载，
  `Restart=no`、`NRestarts=0`；`hub exiting` 来自 `hub_api.go:403`（`/api/exit`），
  即**手动/程序化退出**，不是 systemd 自动重启

## 八、现场处置记录（已由诊断方完成，勿重复）

| 项 | 状态 |
|---|---|
| `~/.codex/aihub-model-catalog.json` | 已用 hub 自己的接口重建：14 个模型 / 654901 字节 / 每条 33 键完整 / `context_window=600000`（与 `state.json` 的 `ctx_window` 一致） |
| 重建方式 | `POST http://127.0.0.1:8801/api/ctx/window {"tokens":600000}`（幂等传当前值，内部触发 `codexCatalogRefresh()`） |
| 坏文件备份 | `/home/<user>/opt/AI_test/hub-go/.tmp/aihub-model-catalog.bad-123148.json`（17 字节空表） |
| 好文件备份 | `/home/<user>/opt/AI_test/hub-go/.tmp/aihub-model-catalog.good-backup.json`（654901 字节） |
| 修完后再次被测试打坏 | 已再次用 good-backup 恢复（12:34:46），当前正常 |
| 代码 | **未改动任何代码**（用户指定由 hub-go 会话来改） |

### 修好代码后如果 catalog 又空了，快速恢复：

```sh
cp /home/<user>/opt/AI_test/hub-go/.tmp/aihub-model-catalog.good-backup.json \
   ~/.codex/aihub-model-catalog.json
```

或者让它按当前白名单重新生成（更准）：

```sh
curl -s -X POST http://127.0.0.1:8801/api/ctx/window \
  -H 'Content-Type: application/json' -d '{"tokens":600000}'
```

然后**重启 Codex 桌面版**（它只在启动时读一次这个文件）。
