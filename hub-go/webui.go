package main

// 控制台网页（嵌入二进制，无外部依赖）。

const webHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>WorkBuddy 控制台 (Go)</title>
<style>
  :root { --ok:#0a7a32; --off:#888; --warn:#b8860b; --err:#b00020; --line:#e2e5ea; }
  * { box-sizing: border-box; }
  body { font-family: "Microsoft YaHei", system-ui, sans-serif; background:#f5f6f8; margin:0; padding:16px; }
  .wrap { max-width:640px; margin:0 auto; }
  h1 { font-size:20px; margin:0 0 12px; color:#1f2329; }
  .tag { font-size:12px; color:#6b7280; font-weight:normal; margin-left:8px; }
  #msg { min-height:22px; font-size:13px; color:#333; margin:0 0 10px; word-break:break-all; }
  #msg.err { color:var(--err); }
  .card { background:#fff; border:1px solid var(--line); border-radius:8px; padding:14px 16px; margin-bottom:12px; }
  .card-head { font-size:14px; font-weight:600; color:#1f2329; display:flex; justify-content:space-between; align-items:center; }
  .status { font-size:13px; font-weight:normal; }
  .st-off { color:var(--off); }
  .st-ok { color:var(--ok); }
  .st-warn { color:var(--warn); }
  .detail { font-size:12px; color:#6b7280; margin:6px 0 10px; min-height:16px; }
  .btns { display:flex; align-items:center; gap:8px; flex-wrap:wrap; }
  .btns .spacer { flex:1; }
  .hint { font-size:11px; color:#9aa0a8; }
  button { border:1px solid var(--line); background:#fff; color:#1f2329; border-radius:6px; padding:6px 14px; font-size:13px; cursor:pointer; }
  button:hover { background:#f0f1f3; }
  button.primary { background:#1664ff; border-color:#1664ff; color:#fff; }
  button.primary:hover { background:#0e56e0; }
  button.danger { color:var(--err); }
  .cred label { font-size:13px; margin-right:10px; cursor:pointer; }
  .foot { display:flex; gap:8px; flex-wrap:wrap; }
  .foot button:last-child { margin-left:auto; }
</style>
</head>
<body>
<div class="wrap">
  <h1>WorkBuddy 控制台<span class="tag">Go 版 · 默认全部停止</span></h1>
  <div id="msg"></div>

  <div class="card">
    <div class="card-head"><span>国内 WorkBuddy · 127.0.0.1:8787</span><span class="status st-off" id="dom-status">检测中…</span></div>
    <div class="detail" id="dom-detail"></div>
    <div class="btns">
      <button class="primary" onclick="act('dom/start')">启动</button>
      <button onclick="act('dom/stop')">停止</button>
      <span class="spacer"></span><span class="hint">converter_cn</span>
    </div>
  </div>

  <div class="card">
    <div class="card-head"><span>国际 WorkBuddy AI · 127.0.0.1:8799</span><span class="status st-off" id="intl-status">检测中…</span></div>
    <div class="detail" id="intl-detail"></div>
    <div class="btns">
      <span class="cred" id="cred-row"></span>
      <button class="primary" onclick="act('intl/start')">启动</button>
      <button onclick="act('intl/stop')">停止</button>
      <button onclick="act('intl/switch')">换凭证并重启</button>
      <span class="spacer"></span><span class="hint">converter</span>
    </div>
  </div>

  <div class="card">
    <div class="card-head"><span>MiMo（小米）· 127.0.0.1:8798</span><span class="status st-off" id="mimo-status">检测中…</span></div>
    <div class="detail" id="mimo-detail"></div>
    <div class="btns">
      <button class="primary" onclick="act('mimo/start')">启动</button>
      <button onclick="act('mimo/stop')">停止</button>
      <span class="spacer"></span><span class="hint">MiMo V2.6 Flash / Pro / Pro Ultraspeed · 小米账号 SSO</span>
    </div>
  </div>

  <div class="card">
    <div class="card-head"><span>Qoder CN（阿里）· 127.0.0.1:8797</span><span class="status st-off" id="qoder-status">检测中…</span></div>
    <div class="detail" id="qoder-detail"></div>
    <div class="btns">
      <button class="primary" onclick="act('qoder/start')">启动</button>
      <button onclick="act('qoder/stop')">停止</button>
      <span class="spacer"></span><span class="hint">14 个模型：Qwen3.8 / GLM-5.3 / DeepSeek-V4 / Kimi-K3 / MiniMax · 桌面端凭据自动解密</span>
    </div>
  </div>

  <div class="card">
    <div class="card-head"><span>Zcode（Z.ai）· 127.0.0.1:8796</span><span class="status st-off" id="zcode-status">检测中…</span></div>
    <div class="detail" id="zcode-detail"></div>
    <div class="btns">
      <input type="password" id="zcode-apikey" placeholder="可选：留空自动读取桌面端登录凭据" style="flex:1;min-width:200px;padding:6px;border:1px solid #e2e5ea;border-radius:6px;font-size:13px;">
      <button class="primary" onclick="startZcode()">启动</button>
      <button onclick="act('zcode/stop')">停止</button>
      <span class="spacer"></span><span class="hint">GLM-5.3 / GLM-5.3-Flash · 桌面端凭据自动解密 · Coding Plan 额度</span>
    </div>
  </div>

  <div class="foot">
    <button onclick="act('stop_all')">全部停止</button>
    <button onclick="refresh()">立即刷新</button>
    <button class="danger" onclick="exitHub()">退出控制台</button>
  </div>
</div>

<script>
let state = null;
const msgEl = document.getElementById('msg');

function showMsg(text, err) {
  msgEl.textContent = text || '';
  msgEl.className = err ? 'err' : '';
}

function render(s) {
  state = s;
  renderSide('dom', s.dom);
  renderSide('intl', s.intl);
  renderSide('mimo', s.mimo);
  renderSide('qoder', s.qoder);
  renderSide('zcode', s.zcode);
  // 凭证单选
  const row = document.getElementById('cred-row');
  if (row.dataset.cred !== s.intl_cred) {
    row.dataset.cred = s.intl_cred;
    row.innerHTML = '';
    const span = document.createElement('span');
    span.textContent = '凭证：';
    row.appendChild(span);
    (s.creds || []).forEach(function (name) {
      const lab = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'cred';
      inp.value = name;
      inp.checked = (name === s.intl_cred);
      inp.onchange = function () { setCred(name); };
      lab.appendChild(inp);
      lab.appendChild(document.createTextNode(' ' + name));
      row.appendChild(lab);
    });
  }
}

function renderSide(key, d) {
  const st = document.getElementById(key + '-status');
  const dt = document.getElementById(key + '-detail');
  if (!d || !d.up) {
    st.textContent = '○ 已停止';
    st.className = 'status st-off';
    dt.textContent = '';
  } else if (d.ok) {
    st.textContent = '● 运行中';
    st.className = 'status st-ok';
    dt.textContent = '账号 ' + (d.nick || '?') + (d.detail ? '  ·  ' + d.detail : '');
  } else {
    st.textContent = '● 端口占用 /health 异常';
    st.className = 'status st-warn';
    dt.textContent = '';
  }
}

// Zcode 专用启动函数（带 API Key）
async function startZcode() {
  const apiKey = document.getElementById('zcode-apikey').value.trim();
  if (!apiKey) {
    showMsg('请输入 Zcode API Key', true);
    return;
  }
  
  try {
    const r = await fetch('/api/zcode/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ apiKey: apiKey }),
      signal: AbortSignal.timeout(20000)
    });
    const d = await r.json();
    showMsg(d.msg, !d.ok);
    
    // 保存 API Key
    if (d.ok) {
      await saveZcodeKey(apiKey);
    }
  } catch (e) {
    showMsg('请求失败：' + e, true);
  }
  refresh();
}

async function saveZcodeKey(apiKey) {
  try {
    await fetch('/api/zcode/key', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ apiKey: apiKey })
    });
  } catch (e) {
    // 忽略保存失败
  }
}

// Zcode 专用启动函数（带 API Key）
async function startZcode() {
  const apiKey = document.getElementById('zcode-apikey').value.trim();
  if (!apiKey) {
    showMsg('请输入 Zcode API Key', true);
    return;
  }
  
  try {
    const r = await fetch('/api/zcode/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ apiKey: apiKey }),
      signal: AbortSignal.timeout(20000)
    });
    const d = await r.json();
    showMsg(d.msg, !d.ok);
    
    // 保存 API Key
    if (d.ok) {
      await saveZcodeKey(apiKey);
    }
  } catch (e) {
    showMsg('请求失败：' + e, true);
  }
  refresh();
}

async function saveZcodeKey(apiKey) {
  try {
    await fetch('/api/zcode/key', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ apiKey: apiKey })
    });
  } catch (e) {
    // 忽略保存失败
  }
}

async function post(path, body) {
  try {
    const r = await fetch('/api/' + path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
      signal: AbortSignal.timeout(20000)
    });
    const d = await r.json();
    showMsg(d.msg, !d.ok);
  } catch (e) {
    showMsg('请求失败：' + e, true);
  }
  refresh();
}

function act(path) { post(path, { cred: state ? state.intl_cred : '' }); }

function setCred(name) { post('intl/cred', { cred: name }); }

function exitHub() {
  if (!confirm('确定退出控制台？（会同时停止全部代理）')) return;
  showMsg('正在退出…');
  // 退出后服务端就没了，别等响应体，发完即视为成功
  fetch('/api/exit', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: '{}',
    signal: AbortSignal.timeout(5000)
  }).catch(function () {});
  setTimeout(function () {
    document.body.innerHTML =
      '<div style="font-family:Microsoft YaHei,sans-serif;padding:40px;color:#333">' +
      '<h2>控制台已退出</h2><p>全部代理已停止，此页面可以关闭。</p></div>';
  }, 1500);
}

async function refresh() {
  try {
    const r = await fetch('/api/status', { signal: AbortSignal.timeout(5000) });
    const s = await r.json();
    render(s);
  } catch (e) {
    showMsg('控制台连接中断：' + e, true);
  }
}

// 昵称单独拉取：读凭据可能较慢，不能拖住状态轮询和启停按钮
async function refreshNick() {
  try {
    const r = await fetch('/api/nick', { signal: AbortSignal.timeout(10000) });
    const n = await r.json();
    ['dom', 'intl', 'mimo', 'qoder'].forEach(function (k) {
      const el = document.getElementById(k + '-detail');
      if (!el || !state || !state[k] || !state[k].up) return;
      const base = state[k].detail ? state[k].detail : '';
      el.textContent = '账号 ' + (n[k] || '?') + (base ? '  ·  ' + base : '');
    });
    // Zcode 不需要昵称，但需要刷新状态
    if (state && state.zcode && state.zcode.up) {
      const el = document.getElementById('zcode-detail');
      if (el) {
        el.textContent = 'Zcode · GLM-5.3';
      }
    }
  } catch (e) { /* 忽略：下一次轮询再试 */ }
}

refresh();
setInterval(refresh, 2000);
setInterval(refreshNick, 6000);
refreshNick();
</script>
</body>
</html>
`
