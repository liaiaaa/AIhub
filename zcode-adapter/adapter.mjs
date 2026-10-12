#!/usr/bin/env node
// zcode-adapter — 把 ZCode 官方 app-server(ZCode Protocol / stdio)桥接为
// 本地 OpenAI 兼容接口,供 DSH 等 OpenAI 客户端接入。
//
//   DSH ──HTTP──> 127.0.0.1:8795/v1/chat/completions
//                     │  (本适配器,零第三方依赖,Node ≥ 20)
//                     ▼
//            node zcode.cjs app-server --stdio   ← 官方 CLI,认证/签名全部由它完成
//
// 设计要点:
//   - 只通过官方 stdio 协议驱动官方 CLI;不修改 ZCode 安装文件。
//   - 一个 app-server 常驻子进程;每个 HTTP 请求一个独立会话
//     (create → setModel → subscribe → send → 收事件 → 读转录 → close),串行执行。
//   - 协议反向请求按官方桥接惯例应答:
//       session/requestRuntimePreferences → 桥接配置
//       interaction/requestProviderRuntimeHeaders → { headersApplied:false }
//         (验证码头由桌面端 UI 能力提供,见 RESEARCH.md「边界」一节)
//   - 启动参数(环境变量,均有默认值):
//       ZCODE_ADAPTER_PORT      监听端口(默认 8795)
//       ZCODE_HARNESS           zcode.cjs 路径(默认 E:\software\Zcode\resources\glm\zcode.cjs)
//       ZCODE_BUILTIN_CONFIG    官方内置 provider 配置(默认 E:\software\Zcode\resources\config\provider\zcode-builtin.json)
//       ZCODE_WORKSPACE         会话工作目录(默认本目录下 workspace\)
//       ZCODE_ADAPTER_KEY       可选;设置后要求客户端 Authorization/Bearer 匹配

import { spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const PORT = Number(process.env.ZCODE_ADAPTER_PORT ?? 8795);
const HARNESS = process.env.ZCODE_HARNESS
  ?? "E:\\software\\Zcode\\resources\\glm\\zcode.cjs";
const BUILTIN_CONFIG = process.env.ZCODE_BUILTIN_CONFIG
  ?? "E:\\software\\Zcode\\resources\\config\\provider\\zcode-builtin.json";
const WORKSPACE = process.env.ZCODE_WORKSPACE ?? path.join(__dirname, "workspace");
const ADAPTER_KEY = process.env.ZCODE_ADAPTER_KEY ?? "";
const REQUEST_TIMEOUT_MS = Number(process.env.ZCODE_REQUEST_TIMEOUT_MS ?? 300000);

fs.mkdirSync(WORKSPACE, { recursive: true });

// ---------------------------------------------------------------- 协议连接

let child = null;
let reqId = 1000;
let buffer = "";
const pending = new Map();
let eventListeners = new Set();
let sawTraffic = false;
let sawModelMaterialized = false;

function isReadableFile(f) {
  try { return fs.statSync(f).isFile(); } catch { return false; }
}

function spawnServer() {
  const env = { ...process.env };
  if (!isReadableFile(BUILTIN_CONFIG)) {
    throw new Error(`内置 provider 配置不可读: ${BUILTIN_CONFIG}`);
  }
  env.ZCODE_BUILTIN_PROVIDER_CONFIG_FILE = BUILTIN_CONFIG;
  const c = spawn("node", [HARNESS, "app-server", "--stdio"], {
    cwd: WORKSPACE, env, shell: false,
    stdio: ["pipe", "pipe", "pipe"], windowsHide: true,
  });
  c.stdout.setEncoding("utf8");
  c.stderr.setEncoding("utf8");
  c.stderr.on("data", (chunk) => {
    const tail = chunk.trim().split("\n").pop() ?? "";
    if (tail) console.error(`[harness] ${tail.slice(0, 200)}`);
  });
  c.on("exit", (code, signal) => {
    console.error(`[harness] exited code=${code} signal=${signal}`);
    child = null;
    for (const [, p] of pending) { clearTimeout(p.timer); p.reject(new Error("harness exited")); }
    pending.clear();
  });
  c.stdout.on("data", (chunk) => {
    sawTraffic = true;
    buffer += chunk;
    let i;
    while ((i = buffer.indexOf("\n")) >= 0) {
      const line = buffer.slice(0, i).trim();
      buffer = buffer.slice(i + 1);
      if (line) handleLine(line);
    }
    if (buffer.length > 64 * 1024 * 1024) buffer = "";
  });
  return c;
}

function handleLine(line) {
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.method !== undefined && msg.id !== undefined) {
    // 服务端反向请求
    if (msg.method === "session/requestRuntimePreferences") {
      send({ id: msg.id, result: {
        nativeSearchEnhancementsEnabled: false,
        memoryEnabled: false,
        askUserQuestionAutoResolutionEnabled: true,
        modelContextBudgetStrategy: "preflight-v1",
      }});
    } else if (msg.method === "interaction/requestProviderRuntimeHeaders") {
      // 验证码/风控头只能由桌面端 UI 提供(官方桥接同样不提供);如实应答。
      send({ id: msg.id, result: { headersApplied: false,
        errorMessage: "bridge does not provide provider runtime headers" } });
    } else if (msg.method === "interaction/requestOfficialMcpAuthHeaders") {
      send({ id: msg.id, result: { ok: false, reason: "unsupported" } });
    } else if (msg.method === "interaction/requestPermission") {
      send({ id: msg.id, result: { decision: "deny", reason: "denied by adapter policy" } });
    } else if (msg.method === "interaction/requestUserInput") {
      send({ id: msg.id, result: { answers: [], cancelled: true, reason: "headless adapter" } });
    } else {
      send({ id: msg.id, error: { code: -32601, message: `adapter does not support ${msg.method}` } });
    }
    return;
  }
  if (msg.method !== undefined) {
    if (process.env.ZCODE_ADAPTER_DEBUG === "1") {
      console.error(`[event] ${msg.method} ${JSON.stringify(msg.params ?? {}).slice(0, 300)}`);
    }
    for (const fn of eventListeners) fn(msg);
    return;
  }
  if (msg.id !== undefined) {
    const p = pending.get(String(msg.id));
    if (p) {
      pending.delete(String(msg.id));
      clearTimeout(p.timer);
      if (msg.error) p.reject(new Error(`${p.method} error ${msg.error.code}: ${msg.error.message}`));
      else p.resolve(msg.result);
    }
  }
}

function send(obj) { child?.stdin.write(JSON.stringify(obj) + "\n"); }

function call(method, params = {}, timeoutMs = 120000) {
  if (!child || child.exitCode !== null) throw new Error("harness not running");
  const id = `client-${reqId++}`;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`timeout: ${method} (${timeoutMs}ms)`));
    }, timeoutMs);
    pending.set(id, { resolve, reject, timer, method });
    send({ id, method, params });
  });
}

function ensureServer() {
  if (child && child.exitCode === null) return;
  buffer = "";
  sawTraffic = false;
  child = spawnServer();
  // app-server 无问候语;存活即就绪
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => resolve(), 500);
    child.once("exit", (code) => { clearTimeout(t); reject(new Error(`harness 启动即退出 (code=${code})`)); });
  });
}

// ---------------------------------------------------------------- 会话回合

const workspaceRef = (p) => {
  const s = p.replace(/[\\/]+$/, "");
  return { workspaceKey: s, workspacePath: s };
};

function waitForTurnEnd(sessionId, timeoutMs) {
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      eventListeners.delete(listener);
      resolve({ timedOut: true });
    }, timeoutMs);
    const listener = (msg) => {
      const p = msg.params ?? {};
      if (p.sessionId && p.sessionId !== sessionId) return;
      // session/event 通知:kind 在 params.kind,payload 在 params.payload
      const kind = String(p.kind ?? p.event?.kind ?? p.type ?? "");
      const payload = p.payload ?? p.event ?? p;
      if (/turn\.failed|turn-failed/i.test(kind)) {
        clearTimeout(timer); eventListeners.delete(listener);
        resolve({ failed: true, error: payload?.error ?? payload });
      } else if (/turn\.end|turn-end|turn\.completed|turn\.terminal.*success|completed|finished/i.test(kind)) {
        clearTimeout(timer); eventListeners.delete(listener);
        resolve({ end: payload });
      }
    };
    eventListeners.add(listener);
  });
}

function textFromContent(content) {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content.filter((b) => b?.type === "text").map((b) => b.text ?? "").join("\n");
  }
  return "";
}

async function runTurn(openaiBody) {
  await ensureServer();
  // 内置 provider 配置的服务端刷新是异步的;首连给物化留一点时间
  if (!sawModelMaterialized) {
    await new Promise((r) => setTimeout(r, 3000));
    sawModelMaterialized = true;
  }
  const messages = Array.isArray(openaiBody.messages) ? openaiBody.messages : [];
  const lastUser = [...messages].reverse().find((m) => m?.role === "user");
  if (!lastUser) throw { status: 400, message: "messages 中需要至少一条 user 消息" };
  const prompt = textFromContent(lastUser.content);
  if (!prompt.trim()) throw { status: 400, message: "最后一条 user 消息为空" };

  const created = await call("session/create", {
    workspace: workspaceRef(WORKSPACE),
    titleGenerationEnabled: false,
  }, 120000);
  const sessionId = created?.session?.sessionId;
  if (!sessionId) throw { status: 502, message: "session/create 未返回 sessionId" };

  try {
    // 模型选择:客户端 model 名(如 glm-5.3-flash)→ 目录里 providerId/modelId;
    // 目录为空(物化延迟/个人配置缺失)时回退显式 start-plan 供应商 + 规范 modelId
    const avail = created?.settings?.model?.available ?? [];
    const wanted = String(openaiBody.model ?? "").toLowerCase().replace(/[^a-z0-9.]/g, "");
    const pick = avail.find((m) => String(m.modelId ?? "").toLowerCase().replace(/[^a-z0-9.]/g, "") === wanted)
      ?? avail.find((m) => String(m.modelId ?? "").toLowerCase().includes("flash"))
      ?? avail[0];
    if (pick) {
      await call("session/setModel", {
        sessionId, model: `${pick.providerId}/${pick.modelId}`,
      }, 60000);
    } else {
      const table = { "glm-5.3": "GLM-5.3", "glm-5.3-flash": "GLM-5.3-Flash", "glm-5.2": "GLM-5.2", "glm-5-turbo": "GLM-5-Turbo" };
      const modelId = table[wanted] ?? "GLM-5.3-Flash";
      await call("session/setModel", {
        sessionId,
        model: { providerId: process.env.ZCODE_PROVIDER_ID ?? "account:zai-individual-coding-plan", modelId },
      }, 60000);
    }

    await call("session/subscribe", { sessionId, deliveryKind: "desktop-continuous" }, 60000);
    await call("session/send", { sessionId, content: prompt }, 60000);

    const turn = await waitForTurnEnd(sessionId, REQUEST_TIMEOUT_MS);

    // 转录取最后一条 assistant 文本(事件 schema 细节以转录为准)
    let reply = "";
    try {
      const transcript = await call("session/messages", { sessionId }, 60000);
      const msgs = transcript?.messages ?? transcript?.items ?? [];
      const last = [...msgs].reverse().find((m) => (m.role ?? m?.message?.role) === "assistant");
      const am = last?.message ?? last ?? {};
      reply = textFromContent(am.content);
    } catch { /* 转录失败时退回事件文本 */ }

    if (!reply && turn.failed) {
      const em = turn.error?.message ?? "turn failed";
      throw { status: 502, message: upstreamHint(em) };
    }
    if (!reply && turn.timedOut) {
      await call("session/stop", { sessionId }, 30000).catch(() => {});
      throw { status: 504, message: "ZCode 回合超时" };
    }
    return { reply, model: pick ? `${pick.providerId}/${pick.modelId}` : String(openaiBody.model ?? "") };
  } finally {
    call("session/close", { sessionId }, 30000).catch(() => {});
  }
}

// 上游业务错误的可读化(1113 = 订阅计划只能由桌面端消费,直连需余额/资源包)
function upstreamHint(msg) {
  if (/1113|Insufficient balance/i.test(msg)) {
    return "Z.ai 返回 1113(余额不足):当前订阅计划(Start Plan)的额度只能由 Zcode 桌面端自身消费;"
      + "API Key 直连路径需要账户有充值余额或资源包(或 Individual Coding Plan)。详见 RESEARCH.md。原始错误:" + msg;
  }
  return msg;
}

// ---------------------------------------------------------------- HTTP 层

let queue = Promise.resolve();
function serialized(fn) {
  const next = queue.then(fn, fn);
  queue = next.catch(() => {});
  return next;
}

function readBody(req, limit = 32 * 1024 * 1024) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on("data", (c) => {
      size += c.length;
      if (size > limit) { reject(new Error("body too large")); req.destroy(); return; }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

function sendJSON(res, status, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) });
  res.end(body);
}

function openaiError(res, status, message, type = "upstream_error") {
  sendJSON(res, status, { error: { message, type, code: String(status) } });
}

const server = http.createServer((req, res) => {
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Access-Control-Allow-Headers", "*");
  res.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
  if (req.method === "OPTIONS") { res.writeHead(204); res.end(); return; }

  if (ADAPTER_KEY) {
    const auth = req.headers.authorization ?? "";
    if (auth !== `Bearer ${ADAPTER_KEY}`) {
      openaiError(res, 401, "invalid adapter key", "auth_error");
      return;
    }
  }

  if (req.method === "GET" && req.url === "/health") {
    sendJSON(res, 200, {
      status: "ok", harness: HARNESS, running: child?.exitCode === null ?? false,
      sawTraffic, workspace: WORKSPACE,
    });
    return;
  }

  if (req.method === "GET" && req.url === "/v1/models") {
    const models = ["GLM-5.3", "GLM-5.3-Flash", "GLM-5.2", "GLM-5-Turbo"];
    sendJSON(res, 200, {
      object: "list",
      data: models.map((id) => ({ id: id.toLowerCase(), object: "model", created: 1700000000, owned_by: "zcode" })),
    });
    return;
  }

  if (req.method === "POST" && (req.url === "/v1/chat/completions" || req.url === "/chat/completions")) {
    readBody(req).then((raw) => {
      let body;
      try { body = JSON.parse(raw); } catch { openaiError(res, 400, "bad json", "invalid_request_error"); return; }
      serialized(() => runTurn(body)).then((r) => {
        if (body?.stream === true) {
          // 流式:单帧聚合输出(适配器不做逐 token 转发)
          const id = `chatcmpl-${randomUUID()}`;
          res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
          const frame = (delta, finish) => res.write(`data: ${JSON.stringify({
            id, object: "chat.completion.chunk", created: Math.floor(Date.now() / 1000),
            model: r.model, choices: [{ index: 0, delta, finish_reason: finish }]})}\n\n`);
          frame({ role: "assistant", content: r.reply }, null);
          frame({}, "stop");
          res.write("data: [DONE]\n\n");
          res.end();
        } else {
          sendJSON(res, 200, {
            id: `chatcmpl-${randomUUID()}`, object: "chat.completion",
            created: Math.floor(Date.now() / 1000), model: r.model,
            choices: [{ index: 0, message: { role: "assistant", content: r.reply }, finish_reason: "stop" }],
            usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
          });
        }
      }).catch((e) => {
        const status = e?.status ?? 502;
        openaiError(res, status, e?.message ?? String(e));
      });
    }).catch((e) => openaiError(res, 400, e?.message ?? "read body error"));
    return;
  }

  openaiError(res, 404, "not found", "invalid_request_error");
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`[zcode-adapter] listening on http://127.0.0.1:${PORT}`);
  console.log(`[zcode-adapter] harness: ${HARNESS}`);
  console.log(`[zcode-adapter] workspace: ${WORKSPACE}`);
});
