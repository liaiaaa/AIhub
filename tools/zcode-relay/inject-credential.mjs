// inject-credential.mjs — 把 Zcode 桌面端已有凭据注入 zcode-api 的凭据库，
// 跳过它的浏览器 OAuth 登录（桌面端 JWT 就是 auth login 会拿到的同一枚 token）。
//
// 用法: node inject-credential.mjs
//   读取 %USERPROFILE%\.zcode\v2\credentials.json（enc:v1 AES-256-GCM，与
//   hub-go/auth_zcode.go 同款解密），取 zcodejwttoken + coding-plan api-key +
//   user_info，按 zcode-api src/auth/store.ts 的格式加密写入
//   %USERPROFILE%\.zcode-proxy\credentials.json。
//   种子默认 {homedir}-{platform}-{arch}；可用 ZCODE_PROXY_CREDENTIAL_SECRET 覆盖。

import { createHash, createDecipheriv, webcrypto } from "node:crypto";
import { existsSync, readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { homedir, platform, arch, userInfo } from "node:os";

function die(msg) { console.error("[inject] " + msg); process.exit(1); }

// ---------- 第一步：解密桌面端凭据 ----------
const deskPath = join(homedir(), ".zcode", "v2", "credentials.json");
let desk;
try { desk = JSON.parse(readFileSync(deskPath, "utf-8")); }
catch (e) { die("读不到桌面端凭据 " + deskPath + ": " + e.message); }

let secret = process.env.ZCODE_CREDENTIAL_SECRET || "";
if (!secret) {
  let user = "unknown";
  try { user = userInfo().username; } catch {}
  if (user.includes("\\")) user = user.slice(user.lastIndexOf("\\") + 1);
  // 与桌面端实际的 os.platform() 对齐（Windows=win32 / Linux=linux / macOS=darwin）
  secret = `zcode-credential-fallback:${platform()}:${homedir()}:${user}`;
}
const deskKey = createHash("sha256").update(secret, "utf-8").digest();

function deskDecrypt(v) {
  if (!v.startsWith("enc:v1:")) return v;
  const [ivB64, tagB64, ctB64] = v.slice(7).split(".");
  const iv = Buffer.from(ivB64, "base64url");
  const tag = Buffer.from(tagB64, "base64url");
  const ct = Buffer.from(ctB64, "base64url");
  const decipher = createDecipheriv("aes-256-gcm", Buffer.from(deskKey), iv);
  decipher.setAuthTag(tag);
  return Buffer.concat([decipher.update(ct), decipher.final()]).toString("utf-8");
}

const plain = {};
for (const [k, v] of Object.entries(desk)) {
  try { plain[k] = deskDecrypt(v); } catch { /* 单条失败跳过 */ }
}

const jwt = plain["zcodejwttoken"] || "";
if (!jwt) die("桌面端凭据中无 zcodejwttoken");
let apiKey = "";
for (const [k, v] of Object.entries(plain)) {
  if (k.includes(":coding-plan:") && k.endsWith(":api-key") && v.trim()) {
    if (k.includes("individual-coding-plan")) { apiKey = v.trim(); break; }
    if (!apiKey) apiKey = v.trim();
  }
}
if (!apiKey) die("桌面端凭据中无 coding-plan api-key");

let userId;
try { userId = JSON.parse(plain["oauth:zai:user_info"] || "{}").user_id; } catch {}

// JWT 过期时间（exp 秒），写进凭据供 zcode-api 判断过期
let expMs;
try {
  const payload = JSON.parse(Buffer.from(jwt.split(".")[1], "base64url").toString("utf-8"));
  if (typeof payload.exp === "number") expMs = payload.exp * 1000;
  console.log("[inject] JWT exp =", expMs ? new Date(expMs).toISOString() : "无 exp 声明");
} catch {}
if (expMs && expMs < Date.now()) {
  console.error("[inject] 警告：桌面端 JWT 已过期，请先启动 Zcode 桌面端让它刷新后再注入");
}

// ---------- 第二步：按 zcode-api store.ts 格式加密写入 ----------
const cred = {
  apiKey,
  provider: "zai",
  ...(expMs ? { expiresAt: expMs } : {}),
  ...(userId ? { userId } : {}),
  jwt,
};

const relaySeed = process.env.ZCODE_PROXY_CREDENTIAL_SECRET
  || `${homedir()}-${platform()}-${arch()}`;
const relayKey = createHash("sha256").update(relaySeed, "utf-8").digest();

const iv = webcrypto.getRandomValues(new Uint8Array(12));
const key = await webcrypto.subtle.importKey("raw", Buffer.from(relayKey), { name: "AES-GCM" }, false, ["encrypt"]);
const ct = new Uint8Array(await webcrypto.subtle.encrypt(
  { name: "AES-GCM", iv }, key, new TextEncoder().encode(JSON.stringify(cred))));
const combined = new Uint8Array(iv.length + ct.length);
combined.set(iv, 0); combined.set(ct, iv.length);

const storePath = join(homedir(), ".zcode-proxy", "credentials.json");
mkdirSync(join(homedir(), ".zcode-proxy"), { recursive: true });
writeFileSync(storePath, JSON.stringify({ encrypted: Buffer.from(combined).toString("base64") }), { mode: 0o600 });
console.log("[inject] 已写入 " + storePath);
console.log("[inject] provider=zai jwt_len=" + jwt.length + " api_key_head=" + apiKey.slice(0, 6) + "...");

// ---------- 第三步：中继本地密钥（防浏览器驱动式滥用） ----------
// 中继设置了 proxyApiKey 时会对浏览器发宽松 CORS，密钥若用可猜测常量，
// 恶意网页可猜中并消耗免费额度。这里保证密钥随机、且 config.yaml 与
// hub-go 读取的 relay-key 文件始终一致。
const keyPath = join(homedir(), ".zcode-proxy", "relay-key");
const cfgPath = join(dirname(fileURLToPath(import.meta.url)), "config.yaml");
let relayLocalKey = "";
if (existsSync(keyPath)) relayLocalKey = readFileSync(keyPath, "utf-8").trim();
if (!relayLocalKey) {
  relayLocalKey = Buffer.from(crypto.getRandomValues(new Uint8Array(24))).toString("hex");
  writeFileSync(keyPath, relayLocalKey);
  console.log("[inject] 已生成随机中继密钥 -> " + keyPath);
}
try {
  let cfg = readFileSync(cfgPath, "utf-8");
  const updated = cfg.replace(/proxyApiKey:\s*"[^"]*"/, `proxyApiKey: "${relayLocalKey}"`);
  if (updated !== cfg) {
    writeFileSync(cfgPath, updated);
    console.log("[inject] config.yaml proxyApiKey 已与 relay-key 同步");
  }
} catch (e) {
  console.log("[inject] 提示：未能自动同步 config.yaml（" + e.message + "），请手动把 relay-key 文件内容填入 proxyApiKey");
}
