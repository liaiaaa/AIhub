// 探针2：完整桌面端身份头 + JWT，无验证码头 → 判定 3012 是身份问题还是 IP 问题
import { createHash, createDecipheriv } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { homedir, userInfo } from "node:os";

const desk = JSON.parse(readFileSync(join(homedir(), ".zcode", "v2", "credentials.json"), "utf-8"));
let user = "unknown";
try { user = userInfo().username; } catch {}
if (user.includes("\\")) user = user.slice(user.lastIndexOf("\\") + 1);
const key = createHash("sha256").update(
  process.env.ZCODE_CREDENTIAL_SECRET || `zcode-credential-fallback:win32:${homedir()}:${user}`, "utf-8").digest();
function decrypt(v) {
  if (!v.startsWith("enc:v1:")) return v;
  const [iv, tag, ct] = v.slice(7).split(".").map(s => Buffer.from(s, "base64url"));
  const d = createDecipheriv("aes-256-gcm", key, iv);
  d.setAuthTag(tag);
  return Buffer.concat([d.update(ct), d.final()]).toString("utf-8");
}
let jwt = "";
for (const [k, v] of Object.entries(desk)) if (k === "zcodejwttoken") { try { jwt = decrypt(v); } catch {} }

const tele = JSON.parse(readFileSync(join(homedir(), ".zcode", "v2", "telemetry-state.json"), "utf-8"));
const headers = {
  "Authorization": "Bearer " + jwt,
  "anthropic-version": "2023-06-01",
  "Content-Type": "application/json",
  "User-Agent": "ZCode/3.14.3",
  "HTTP-Referer": "https://zcode.z.ai",
  "X-Title": "Z Code@cli",
  "X-ZCode-App-Version": "3.14.3",
  "X-ZCode-Agent": "glm",
  "X-Platform": "win32-x64",
  "X-Os-Category": "windows",
  "X-Os-Version": "10.0.26200",
  "X-Client-Language": "zh-CN",
  "X-Client-Timezone": "Asia/Shanghai",
  "X-Device-Mid": tele.deviceMid || "",
};
delete headers["X-Device-Mid"] === undefined && headers["X-Device-Mid"] === "" && delete headers["X-Device-Mid"];

const t0 = Date.now();
const resp = await fetch("https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages", {
  method: "POST", headers,
  body: JSON.stringify({ model: "glm-5.3-flash", max_tokens: 16,
    messages: [{ role: "user", content: "reply OK" }] }),
});
const body = await resp.text();
console.log(`完整身份头直打: HTTP=${resp.status} ${Date.now() - t0}ms`);
console.log("响应体:", body.slice(0, 400));
