// 探针：官方关闭验证码校验的验证（2026-09-28 更新公告）
// ① client/configs 的 captcha 开关 ② 无验证码头直打 start-plan 模型网关
import { createHash, createDecipheriv } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { homedir, userInfo } from "node:os";

// 解密桌面端凭据拿 zcodejwttoken（与 inject-credential.mjs 同款逻辑）
const deskPath = join(homedir(), ".zcode", "v2", "credentials.json");
const desk = JSON.parse(readFileSync(deskPath, "utf-8"));
let user = "unknown";
try { user = userInfo().username; } catch {}
if (user.includes("\\")) user = user.slice(user.lastIndexOf("\\") + 1);
const secret = process.env.ZCODE_CREDENTIAL_SECRET
  || `zcode-credential-fallback:win32:${homedir()}:${user}`;
const key = createHash("sha256").update(secret, "utf-8").digest();
function decrypt(v) {
  if (!v.startsWith("enc:v1:")) return v;
  const [iv, tag, ct] = v.slice(7).split(".").map(s => Buffer.from(s, "base64url"));
  const d = createDecipheriv("aes-256-gcm", key, iv);
  d.setAuthTag(tag);
  return Buffer.concat([d.update(ct), d.final()]).toString("utf-8");
}
let jwt = "";
for (const [k, v] of Object.entries(desk)) {
  if (k === "zcodejwttoken") { try { jwt = decrypt(v); } catch {} }
}
if (!jwt) { console.log("无 JWT"); process.exit(1); }
console.log("JWT 长度:", jwt.length);

// ① 服务端验证码开关
const cfgResp = await fetch("https://zcode.z.ai/api/v1/client/configs?app_version=3.14.3&platform=win32-x64");
const cfg = await cfgResp.json();
console.log("① captcha 配置:", JSON.stringify(cfg?.data?.configs?.captcha ?? null));

// ② 无验证码头直打模型网关（anthropic 形态）
const t0 = Date.now();
const resp = await fetch("https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages", {
  method: "POST",
  headers: {
    "Authorization": "Bearer " + jwt,
    "anthropic-version": "2023-06-01",
    "Content-Type": "application/json",
    "User-Agent": "ZCode/3.14.3",
  },
  body: JSON.stringify({
    model: "glm-5.3-flash",
    max_tokens: 16,
    messages: [{ role: "user", content: "reply OK" }],
  }),
});
const body = await resp.text();
console.log(`② 直打网关(无验证码头): HTTP=${resp.status} ${Date.now() - t0}ms`);
console.log("响应头片段:", JSON.stringify({
  captchaParam: resp.headers.get("x-aliyun-captcha-verify-param") ? "有挑战" : "无",
}));
console.log("响应体:", body.slice(0, 300));
