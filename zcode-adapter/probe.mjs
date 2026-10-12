#!/usr/bin/env node
// ZCode app-server 协议探针:验证官方 stdio 接口的会话/模型/问答全链路。
// 用法:node probe.mjs [prompt]
import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import path from "node:path";
import fs from "node:fs";

const HARNESS = "E:\\software\\Zcode\\resources\\glm\\zcode.cjs";
const BUILTIN_CONFIG = "E:\\software\\Zcode\\resources\\config\\provider\\zcode-builtin.json";
const WORKSPACE = "D:\\C\\Desktop\\hub-go\\zcode-adapter\\workspace";
fs.mkdirSync(WORKSPACE, { recursive: true });

const PROMPT = process.argv[2] ?? "13*17=? 只回复数字结果";

const child = spawn("node", [HARNESS, "app-server", "--stdio"], {
  cwd: WORKSPACE,
  env: { ...process.env, ZCODE_BUILTIN_PROVIDER_CONFIG_FILE: BUILTIN_CONFIG },
  shell: false,
  stdio: ["pipe", "pipe", "pipe"],
  windowsHide: true,
});
child.stderr.setEncoding("utf8");
child.stderr.on("data", (c) => process.stderr.write("[harness-stderr] " + c));
child.on("exit", (code, sig) => console.error(`[harness exited code=${code} sig=${sig}]`));

let buf = "";
const pending = new Map();
let reqId = 1000;
const events = [];

child.stdout.setEncoding("utf8");
child.stdout.on("data", (chunk) => {
  buf += chunk;
  let i;
  while ((i = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, i).trim();
    buf = buf.slice(i + 1);
    if (!line) continue;
    let msg;
    try { msg = JSON.parse(line); } catch { console.error("[unparsed]", line.slice(0, 200)); continue; }
    if (msg.method !== undefined && msg.id !== undefined) {
      // 服务端反向请求
      if (msg.method === "session/requestRuntimePreferences") {
        send({ id: msg.id, result: {
          nativeSearchEnhancementsEnabled: false,
          memoryEnabled: false,
          askUserQuestionAutoResolutionEnabled: true,
          modelContextBudgetStrategy: "preflight-v1",
        }});
        console.error("[reverse] runtimePreferences answered");
      } else {
        console.error("[reverse]", msg.method);
        send({ id: msg.id, error: { code: -32601, message: "probe does not support " + msg.method } });
      }
      continue;
    }
    if (msg.method !== undefined) {
      events.push(msg);
      const p = msg.params ?? {};
      const ev = p.event ?? p;
      const kind = ev.kind ?? ev.type ?? msg.method;
      const txt = JSON.stringify(ev);
      if (String(kind).match(/turn|message|error|text|completion/i) || events.length <= 40) {
        console.error("[event]", msg.method, kind, txt.slice(0, 220));
      }
      continue;
    }
    if (msg.id !== undefined) {
      const key = String(msg.id);
      const p = pending.get(key);
      if (p) {
        pending.delete(key);
        p(msg);
      }
    }
  }
});

function send(obj) { child.stdin.write(JSON.stringify(obj) + "\n"); }
function call(method, params, timeoutMs = 120000) {
  return new Promise((resolve, reject) => {
    const id = `client-${reqId++}`;
    const t = setTimeout(() => { pending.delete(id); reject(new Error(`timeout ${method}`)); }, timeoutMs);
    pending.set(id, (resp) => {
      clearTimeout(t);
      if (resp.error) reject(new Error(`${method} error ${resp.error.code}: ${resp.error.message}`));
      else resolve(resp.result);
    });
    send({ id, method, params: params ?? {} });
  });
}

function extractText(ev) {
  // 从事件里尽力抽 assistant 文本
  const s = JSON.stringify(ev);
  const texts = [];
  const re = /"text"\s*:\s*"((?:[^"\\]|\\.)*)"/g;
  let m;
  while ((m = re.exec(s)) !== null) texts.push(m[1]);
  return texts.join("");
}

const result = {};
try {
  const created = await call("session/create", {
    workspace: { workspaceKey: WORKSPACE.replace(/[\\/]+$/, ""), workspacePath: WORKSPACE.replace(/[\\/]+$/, "") },
    titleGenerationEnabled: false,
  });
  const sessionId = created?.session?.sessionId;
  result.sessionId = sessionId;
  const avail = created?.settings?.model?.available ?? [];
  result.catalog = avail.map((m) => `${m.providerId}/${m.modelId}${m.supportsReasoning ? "*" : ""}`);
  console.error("[catalog]", JSON.stringify(result.catalog, null, 1).slice(0, 1500));
  console.error("[current]", JSON.stringify(created?.settings?.model?.current ?? created?.settings?.model?.default ?? null));

  if (!sessionId) throw new Error("no sessionId");

  // 选模型:优先 zai 供应商下的 glm-5.3-flash(与 DSH 的 DeepSeek-Flash 定位类似),否则第一个
  const pick = avail.find((m) => /zai/i.test(m.providerId ?? "") && /flash/i.test(m.modelId ?? ""))
    ?? avail.find((m) => /zai/i.test(m.providerId ?? ""))
    ?? avail[0];
  if (pick) {
    await call("session/setModel", { sessionId, model: { providerId: pick.providerId, modelId: pick.modelId } });
    result.model = `${pick.providerId}/${pick.modelId}`;
    console.error("[model set]", result.model);
  }

  await call("session/subscribe", { sessionId, deliveryKind: "desktop-continuous" });
  const sent = await call("session/send", { sessionId, content: PROMPT });
  console.error("[send ack]", JSON.stringify(sent).slice(0, 300));

  // 等待回合结束
  const done = await new Promise((resolve, reject) => {
    const t = setTimeout(() => resolve({ timeout: true, events: events.length }), 180000);
    const check = () => {
      const end = events.find((e) => {
        const ev = (e.params ?? {}).event ?? e.params ?? {};
        const k = String(ev.kind ?? ev.type ?? "");
        return /turn\/end|turn_end|completed|finished/i.test(k);
      });
      if (end) { clearTimeout(t); resolve({ end }); }
    };
    const iv = setInterval(check, 500);
    setTimeout(() => clearInterval(iv), 181000);
  });
  result.doneMarker = JSON.stringify(done).slice(0, 200);

  // 转录:直接读会话消息,拿最终 assistant 文本
  const transcript = await call("session/messages", { sessionId });
  const msgs = transcript?.messages ?? transcript?.items ?? [];
  const lastAssistant = [...msgs].reverse().find((m) => m.role === "assistant" || m?.message?.role === "assistant");
  const am = lastAssistant?.message ?? lastAssistant ?? {};
  let text = "";
  const c = am.content;
  if (typeof c === "string") text = c;
  else if (Array.isArray(c)) text = c.filter((b) => b.type === "text").map((b) => b.text).join("\n");
  result.reply = text.slice(0, 400);
  result.transcriptMsgs = msgs.length;
} catch (e) {
  result.error = String(e);
}
console.log("===RESULT===\n" + JSON.stringify(result, null, 1));
child.stdin.end();
setTimeout(() => process.exit(0), 500);
