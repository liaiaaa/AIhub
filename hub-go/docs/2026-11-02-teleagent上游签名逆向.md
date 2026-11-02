# TeleAgent 上游签名逆向记录（2026-11-02）

> 对象：`~/.local/share/TeleAgent/runtimes/super-agent-code/bin/TeleAgent`（Go 运行时，stripped）
> 手段：只读反汇编（Go pclntab 取函数名 + objdump），未安装任何东西、未向任何接口发请求。
> 交叉验证：① 开源实现 `hj01857655/TeleAgent2api`；② 本机真实调用返回 200。
> 落地产物：`hub-go/teleagent_proxy.go`、`hub-go/teleagent_vault.go`。
>
> 注意：这是**非公开 API**，上游改版即失效；失效时表现为 40101（缺头）/ 40104（版本或签名不对）。

## 一页速查

| 项 | 值 |
|---|---|
| 上游 | `POST https://agent.teleai.com.cn/superCowork/sapi/api/v1/chat/completions` |
| 版本头 | `X-SuperAgent-Sign-Version: v1`（二进制内只有这一个常量） |
| 时间戳 | 二进制用**毫秒**；开源实现用**秒**，实测秒可用 |
| nonce | UUIDv4 |
| k1 | `hexHMAC(JWT第三段, "superagent-auth-v1/<token>/<installId>/<ts>/<nonce>")` |
| 签名 | `hexHMAC(k1 的 64 字节 hex 串, join("\n", ["superagent-auth-v1","POST",path?query,ts,nonce,appVersion,sha256Hex(body)]))` |
| deviceId | 只作头发，**不参与**签名材料（已用测试钉住） |

---

**结论：能纯离线复刻。** key 完全从 token 自己派生，不依赖服务端密钥；有有效 JWT + installId/deviceId 即可本地算出全部签名头。

**Q1 canonical 7 段，`\n` 连接**（`(*GatewaySigner).Sign` @0x117c380，切片 rsp+0x78，Join @0x117c60f，分隔符 0x1e8c910=`\n`）：
①`superagent-auth-v1` @0x117c7ec ②`strings.ToUpper(method)` @0x117c535 ③`req.URL.RequestURI()`＝path?query，无 scheme/host，@0x117d981 ④ts=`fmt.Sprintf("%d",time.Now().UnixMilli())` **毫秒**（格式串 0x1c939a0=`%d`，@0x117d932） ⑤nonce=`randomUUID()` UUIDv4（@0x117d947） ⑥appVersion（transport+0x10，env `SUPER_AGENT_APP_VERSION`，缺省 `unknown`） ⑦`sha256Hex(io.ReadAll(body))` 小写 hex（@0x117d9ae，body 读于 0x117e38e）

**Q2 key＝两级 HMAC，不是 sha256Hex(jwt3)：**
k1 = hex(HMAC(jwt3(token), join(`/`, [prefix, token, installId, ts, nonce])))——这一段分隔符是 `/`(0x1e8c998)，@0x117c516；
sig = hex(HMAC(**k1 的 64 字节 ASCII 十六进制串**, canonical7)) @0x117c635。
`jwtThirdSegment` @0x117e680 = `Split(s,".")` 恰 3 段取 [2]，否则原串。
token 链：RoundTrip → `tool.CurrentAuthState` @0x117d80e（缓存 0x3139078，AuthState{Token@0x0,DeviceID@0x10,InstallID@0x20} 三个 string）← `tool.LoadAuthStateFromEnv` @0x1162de0 读 env `SUPER_AGENT_AUTH_STATE` 的 JSON、用 `SUPER_AGENT_LOCAL_SESSION_KEY` 经 `DecryptAuthState` 解密（@0x1162f40）。

**Q3 只接受 v1。** 二进制内唯一常量 `v1`(0x1c9399e,len2) 硬编码写入 @0x117ca88，无版本枚举、无分派分支；`unsupported sign version`/`missing super agent signature` 字符串都不存在——40104 的两种语义纯服务端产生，客户端不区分。

**Q4 本地 4397 不是同一套。** `validateLocalAuth` @0x16836e0：头为 `x-sa-sign-version/timestamp/nonce/signature`；版本必须 `local-v1`（0x1683861，常量 0x31762d6c61636f6c）；canonical=join(`\n`,[local-v1,UPPER(method),path?query,ts,nonce])（拼接 @0x1683d19）；签名=**base64.Std**(HMAC-SHA256)（@0x1683dd5）；key=闭包捕获的本地会话密钥（源自 `SUPER_AGENT_LOCAL_SESSION_KEY`，config.Load @0x14a85af），非 JWT；另有 nonce 重放表+Mutex 与时间戳窗口。

**Go 伪代码**
```go
func tok3(t string) string { if p := strings.Split(t, "."); len(p) == 3 { return p[2] }; return t }

func hmacHex(key, msg string) string {           // 小写 hex
	m := hmac.New(sha256.New, []byte(key)); m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// auth = {Token, DeviceID, InstallID}
func signHeaders(auth AuthState, method, requestURI, appVersion string, body []byte) http.Header {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10) // 毫秒
	nonce := uuidV4()                                   // rand 16B; b[6]&0x0f|0x40; b[8]&0x3f|0x80; 36 字符带 '-'
	bodyHash := hex.EncodeToString(func() []byte { s := sha256.Sum256(body); return s[:] }())

	k1 := hmacHex(tok3(auth.Token), strings.Join([]string{
		"superagent-auth-v1", auth.Token, auth.InstallID, ts, nonce,
	}, "/"))
	sig := hmacHex(k1, strings.Join([]string{
		"superagent-auth-v1", strings.ToUpper(method), requestURI,
		ts, nonce, appVersion, bodyHash,
	}, "\n"))

	h := http.Header{}
	h.Set("X-Token", auth.Token)
	h.Set("X-SuperAgent-Timestamp", ts)
	h.Set("X-SuperAgent-Nonce", nonce)
	if auth.DeviceID != "" { h.Set("X-SuperAgent-Device-Id", auth.DeviceID) }
	if auth.InstallID != "" { h.Set("X-SuperAgent-Install-Id", auth.InstallID) }
	h.Set("X-SuperAgent-Sign-Version", "v1")
	h.Set("X-App-Version", appVersion)                  // 空则 "unknown"
	h.Set("X-SuperAgent-Signature", sig)
	return h
}
// requestURI = req.URL.RequestURI()；RoundTrip 按"已存在则不覆盖"并入请求头
// 附带 X-TeleAI-Upstream-Request-ID = 第二个 UUIDv4（与签名无关）
```

**未确定**：①appVersion 运行时实际值；②AuthState 三字段顺序按 offset 推定，未与解密后 JSON 键逐字对表；③`(*AKSKSigner).Sign` @0x117cd40 是另一套 AK/SK 头（X-AK/X-Timestamp/X-Nonce/X-Signature/X-OS-Type/X-App-Version/X-Token，供 PostWithAKSK），未展开；④服务端 ts 窗口/重放窗口客户端不可见，本地中间件窗口常数未逐条确认；⑤全程只读，未发任何请求，故无在线验证。
