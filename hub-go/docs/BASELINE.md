# 重构基线（重构开始前实测）

- 采集时间：2026-09-25 20:25:00
- Go 版本：go version go1.27.1 windows/amd64
- 工作目录：D:\C\Desktop\hub-go\hub-go

## go vet ./...  ->  退出码 0（通过）

```
(无输出 —— vet 通过)
```

## go test ./...  ->  退出码 1

```
--- FAIL: TestCredDiscovery (0.00s)
    proxy_intl_test.go:24: <凭证目录C>目录未被识别为有效凭证目录: D:\C\Desktop\workbuddy-hub\<凭证目录C>
FAIL
FAIL	workbuddy-hub	2.656s
FAIL

```

## 结论

- `go vet`：**通过**
- `go test`：**34 个用例，1 个失败**
  - 失败项：`TestCredDiscovery`
  - 根因：`proxy_intl_test.go:17` 硬编码了过期绝对路径
    `D:\C\Desktop\workbuddy-hub`（该目录已不存在）

重构验收标准：`go vet` 保持通过；`go test` 不得新增失败，
且 `TestCredDiscovery` 应在阶段 1 转为通过。

## 环境约束

- 采集时 `workbuddy-hub.exe` 正在运行（PID 14176）。
  重构全程**不得移动、覆盖或删除该 exe**。
- 用户实际使用的凭证来自 C 盘（`%LOCALAPPDATA%\CodeBuddyExtension`），
  与仓库内 `<凭证目录A>/<凭证目录B>/<凭证目录C>` 三个目录无依赖关系。
