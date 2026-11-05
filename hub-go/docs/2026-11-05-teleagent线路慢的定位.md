# teleagent 线路在 dsh 里为什么慢（2026-11-05）

> 用户问题：「为什么反代的 teleagent 感觉模型在 dsh 中运行速度那么慢呢？」
> 诊断由子代理实测（18:39–19:02，工具留在 `.tmp/bench/`），lead 用现网
> `usage.jsonl` 独立复核并**修正了一条会误导的结论**（见第三节）。

## 一、结论一句话

慢的不是反代链路，是**上游模型的解码吞吐**：dsh 在 teleagent 上只能用
「旗舰 chat-flagship」，实测 47.6 tok/s；对照 qoder 主力 DeepSeek-Flash
是 120.1 tok/s，差 2.5 倍。建连 + TTFT + hub 跳数合计只占一个回合的 1–5%。

## 二、数据（lead 独立复核，口径 = ms/token 的 p50）

筛选：`code=200`、`in≥1000`、`out≥50`（排除探测请求与截断样本）。

| slug | n | ms/token p50 | 等效 tok/s | out_p50 | 总耗时 p50 |
|---|---|---|---|---|---|
| qoder.DeepSeek-Flash | 26,867 | **8.33** | 120.1 | 605 | 5,647ms |
| dom.deepseek-v4-flash | 3,410 | 13.74 | 72.8 | 402 | 4,665ms |
| intl.deepseek-v4.1-flash | 13,287 | 15.75 | 63.5 | — | 7,580ms |
| trae.deepseek-v4.1-flash | 1,576 | 17.69 | 56.5 | — | 9,258ms |
| **teleagent.chat-flagship** | 291 | **21.02** | **47.6** | 636 | **12,414ms** |
| qoder.Qwen3.8-Flash | 7,392 | 21.79 | 45.9 | 623 | 14,070ms |
| **teleagent.chat-pro** | 46 | **8.98** | **111.3** | **1,978** | **14,616ms** |
| teleagent.chat-lite | 1 | — | 286.8 | 800 | 2,789ms |

链路侧实测（`.tmp/bench`，真实上游小请求）：

| 项 | 数值 |
|---|---|
| 纯握手 25 次 | dns 8.5 + tcp 27.7 + tls 52.9–65.4 = **97–101ms** |
| uTLS(chrome) 相对 Go TLS 的额外开销 | +5–13ms |
| TTFT teleagent 直连 | 883–1,246ms（1 次 5,037ms 尖峰） |
| TTFT qoder 对照（经 :8797） | 531–917ms |
| 600KB 上行写出 | 87–96ms（≈6.5MB/s） |
| 空闲 10s / 30s 后 | `reused=true`（同源端口 52990） |
| 空闲 95s 后 | **`reused=false`**，冷建连 37–123ms（`IdleConnTimeout=90s`，`proxy_util.go:273`） |
| hub 聚合跳 | `proxy_agg.go:600` `aggCall` 是**进程内直调**，不过 HTTP |

## 三、重要修正：不要把 flagship 换成 pro

子代理建议「dsh 首选从 chat-flagship 换成 chat-pro，实测 49 → 114 tok/s」。
**按每 token 速度这没错，但按回合墙钟时间是错的**：

- chat-pro 的 `out_p50 = 1,978`，flagship 是 `636` —— pro 话多 **3.1 倍**；
- 所以现网实测 pro 的总耗时 p50 是 **14,616ms > flagship 的 12,414ms**。

也就是说换过去，每回合反而慢约 2 秒。「解码快」不等于「回合快」，
模型爱写长答案把速度优势全吃掉了。这条要落地必须先固定输出长度做 A/B
（同一 prompt、`max_tokens` 一致），不能拿现网混合样本推。

## 四、真正能提速的做法

1. **交互场景别用 teleagent 当主力**：qoder.DeepSeek-Flash 8.33 ms/token、
   回合 p50 5.6s，是 flagship 的 2.2 倍快，且现网样本 2.7 万条，数据可信。
   （本会话模型就是 `qoder.*`，已经是这个状态。）
2. **teleagent 留作特定用途**（它的强项是 934k 上下文与 TeleAI 侧能力，不是速度）。
3. 想给 teleagent 提速，可以试 `chat-flash`（极速），但**现网只有 3 条 200 样本、
   且都是 in=17/15/32 的探测请求**（16–32 token 输出，固定开销占大头，
   算出来的 10.7–15.3 tok/s 不能采信）。要结论得先做一次等长 A/B。

## 五、顺手修掉的一个真 bug

`teleagent_vision.go:382` 之前调
`teleagentSignHeadersAt(..., ts="", nonce="")`，导致

- `X-SuperAgent-Timestamp` / `X-SuperAgent-Nonce` **以空串发出**；
- 签名派生键里也带这两个空值 → 上游按「时间戳 + nonce」校验直接判废。

证据：`hub.log` 里 242 次「识图没有返回内容」。后果是每张带图请求白烧
两次 teleagent 上游往返再回落别的线路。dsh 的图片在消息清洗阶段就被丢
（`hub.log` 每回合「丢弃无法转交的图片 3 个」），所以没暴露在 dsh 文本路径上。

修法：改走 `teleagentVisionHeaders()` → `teleagentSignHeaders()`（内部生成秒级
ts + UUIDv4 nonce）。回归测试 `TestVisionHeadersCarryTimestampAndNonce` 钉住
「ts 非空且 10 位、nonce 非空且 UUIDv4、两次调用 nonce 不同」。

**上游复验未做**（额度已用满 20 次），修复的机制正确、参数形态与正常签名一致，
但「上游是否接受」需要一次带图请求验证。

## 六、排除掉的假设（防止以后误修）

| 假设 | 结论 | 证据 |
|---|---|---|
| 强制 HTTP/1.1 拖慢了它 | **不成立** | 25 次握手 ALPN 全 `(none)`；`curl --http2` 实得 `http_version=1.1`；openssl `No ALPN negotiated`、TLSv1.2 → 上游根本不支持 h2，`client_profile.go:55` 的 `HTTP2=false` 零代价 |
| 流式被整体缓冲 | 对 dsh **不成立** | `teleagent_proxy.go:400-414` 逐 chunk flush；`proxy_agg.go:684-705` 的 `chatCapture`（无 Flusher）**只在 kind=responses 时**全缓冲，而 teleagent 7 天 604 条全是 kind=chat。⚠ 哪天 dsh 改走 responses，这条立刻变成真凶 |
| `min_tokens=1024` 抬高 | 从未触发 | `teleagent_proxy.go:281-284`；converter-teleagent.log 0 条「抬到」 |
| 限流/退避/backoff | 路径内不存在 | 代码 grep 无；hub.log 无 429/超时（3 条 "429" 命中是 rid 十六进制误匹配） |
| prompt 太大 / prefill 慢 | 不是 | teleagent 输入 p50 8 万，只有 qoder 的 1/3 却慢 2.3×；两边缓存命中都 98%+ |
| reasoning_effort medium→high 有思考税 | 无显著差（n=1） | low 1101/2074ms vs high 1145/1821ms |

## 七、两个待观察项

- **27% 断流**：teleagent 655 条里 175 条「⚠ 流中断…context canceled」，
  且是在已写出中位 45KB（数百 token）之后才被掐。发起方不明（dsh 超时？用户手动停止？）
  —— 其他线路不打这条日志（`proxy.go:727` 文案是「上游流中断」），无法横向比。
- **错误率**：teleagent 7d 非 200 = 48/604 = **7.9%**（400×29 / 503×13 / 502×5），
  qoder 是 2.5%。不是「慢」的原因，但说明这条线路本身不稳。

## 八、低优先级的链路优化（收益小、有风险，暂不做）

- `tlsfp.go:339` 按域名拨号，解析在 dial 内部：高频 20 次里 3 次解析耗时
  3,000/3,001ms（同轮 dial IP 恒 21–63ms）；低频（每 12s 一轮）不复现。
  可改成解析一次缓存 IP 或自定义 Resolver + 1s 超时。风险：绕过 IP 轮换/CDN 调度。
- `proxy_util.go:273` `IdleConnTimeout` 90s → 300s：dsh 回合间隔常 >90s，
  每回合一轮全新 DNS+TCP+TLS（0.1–0.2s）。风险：空闲连接被上游单边关闭后
  POST 不重试 → 偶发一次 502（客户端重发即恢复）。
