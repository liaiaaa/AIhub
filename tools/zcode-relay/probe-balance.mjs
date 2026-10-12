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
let jwt = "", apiKey = "";
for (const [k, v] of Object.entries(desk)) {
  const p = decrypt(v);
  if (k === "zcodejwttoken") jwt = p;
  if (k.includes(":coding-plan:") && k.endsWith(":api-key")) apiKey = p;
}
const tele = JSON.parse(readFileSync(join(homedir(), ".zcode", "v2", "telemetry-state.json"), "utf-8"));
const resp = await fetch("https://zcode.z.ai/api/v1/zcode-plan/billing/balance?app_version=3.14.3&platform=win32-x64", {
  headers: {
    "Authorization": "Bearer " + jwt,
    "x-coding-plan-api-key": apiKey,
    "X-Device-Mid": tele.deviceMid || "",
    "User-Agent": "ZCode/3.14.3",
  },
});
const j = await resp.json();
const now = Date.now() / 1000;
for (const b of j?.data?.balances || []) {
  console.log(`${b.show_name}  剩 ${b.remaining_units}/${b.total_units}  过期 ${new Date(b.expires_at * 1000).toLocaleString("zh-CN")}`);
}
if (!j?.data?.balances?.length) console.log("原始:", JSON.stringify(j).slice(0, 400));
