package main

// 控制台网页（嵌入二进制，无外部依赖）。
// 2026-10-07 重设计：顶栏 + 签到条 + 三列网格瓦片（替代原来的单列大卡片），
// 每个瓦片：状态点/账号/额度/启停按钮。额度走 /api/credits（5 分钟缓存，
// 过期后台补拉，见 credits.go），控制台轮询永远不碰上游。

const webHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>AIHub</title>
<style>
  :root { --ok:#0a9c46; --off:#a6adb6; --warn:#c07f10; --err:#b00020;
          --line:#e4e7ec; --bg:#f3f5f8; --blue:#1664ff; --txt:#1f2329; }
  * { box-sizing:border-box; }
  body { font-family:"Microsoft YaHei",system-ui,sans-serif; background:var(--bg);
         margin:0; padding:14px 16px 22px; color:var(--txt); }
  .wrap { max-width:1020px; margin:0 auto; }
  .topbar { display:flex; align-items:center; gap:12px; margin-bottom:10px; flex-wrap:wrap; }
  .brand { font-size:18px; font-weight:700; white-space:nowrap; }
  .brand .tag { font-size:12px; color:#8a919c; font-weight:normal; margin-left:6px; }
  #msg { flex:1; font-size:12.5px; color:#333; min-height:18px; word-break:break-all; }
  #msg.err { color:var(--err); }
  .top-actions { display:flex; gap:6px; }
  .checkin-bar { display:flex; align-items:center; gap:10px; background:#fff;
                 border:1px solid var(--line); border-radius:10px; padding:8px 12px;
                 margin-bottom:10px; flex-wrap:wrap; }
  .checkin-bar .sum { flex:1; font-size:12.5px; color:#57606a; min-width:220px; }
  .chip { font-size:12px; padding:2px 10px; border-radius:20px; white-space:nowrap; }
  .chip.ok { background:#e6f6ec; color:var(--ok); }
  .chip.no { background:#eef0f3; color:#666; }
  .chip.warn { background:#fdf3e0; color:var(--warn); }
  .grid { display:grid; grid-template-columns:repeat(3,1fr); gap:10px; }
  @media (max-width:880px){ .grid{ grid-template-columns:repeat(2,1fr);} }
  @media (max-width:580px){ .grid{ grid-template-columns:1fr;} }
  .tile { background:#fff; border:1px solid var(--line); border-radius:10px;
          padding:10px 12px 9px; display:flex; flex-direction:column; gap:5px; }
  .tile-head { display:flex; align-items:center; gap:7px; }
  .tile-name { font-weight:600; font-size:13.5px; flex:1;
               white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  .tile-port { font-size:11px; color:#9aa0a8; font-family:Consolas,monospace; }
  .dot { width:9px; height:9px; border-radius:50%; background:var(--off); flex:none; }
  .dot.on { background:var(--ok); box-shadow:0 0 0 3px rgba(10,156,70,.13); }
  .dot.warn { background:var(--warn); box-shadow:0 0 0 3px rgba(192,127,16,.13); }
  .tile-acct { font-size:12px; color:#57606a; min-height:17px;
               white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  .tile-credit { font-size:12px; color:var(--blue); min-height:17px;
                 display:flex; align-items:center; gap:6px; }
  .tile-credit span { flex:1; min-width:0; white-space:nowrap;
                      overflow:hidden; text-overflow:ellipsis; }
  .tile-credit span.stale { color:#9aa0a8; }
  .tile-credit span.bad { color:var(--err); }
  .credit-ref { padding:2px 8px; font-size:11px; flex:none; }
  .tile-btns { display:flex; gap:6px; margin-top:1px; flex-wrap:wrap; align-items:center; }
  .tile-btns .spacer { flex:1; }
  button { border:1px solid var(--line); background:#fff; color:var(--txt);
           border-radius:6px; padding:4px 12px; font-size:12px; cursor:pointer; }
  button:hover { background:#f0f2f5; }
  button.primary { background:var(--blue); border-color:var(--blue); color:#fff; }
  button.primary:hover { background:#0e56e0; }
  button.danger { color:var(--err); }
  .cred-radios { font-size:11px; color:#57606a; display:flex; gap:8px;
                 flex-wrap:wrap; align-items:center; }
  .cred-radios label { cursor:pointer; display:flex; align-items:center; gap:3px; }
  .zkey { flex:1; min-width:110px; padding:3px 8px; border:1px solid var(--line);
          border-radius:6px; font-size:12px; }
  .foot-hint { margin-top:12px; font-size:11px; color:#9aa0a8; text-align:center; }
  .agg-card { margin-top:10px; }
  .agg-row { display:flex; gap:6px; flex-wrap:wrap; align-items:center; margin-top:4px; }
  .agg-row select { padding:3px 8px; border:1px solid var(--line); border-radius:6px;
                    font-size:12px; max-width:220px; background:#fff; }
  .agg-item { display:flex; align-items:center; gap:8px; font-size:12px; color:#57606a; padding:2px 0; }
  .agg-item .slug { font-family:Consolas,monospace; color:var(--txt); }
  .ratio { font-size:11px; padding:1px 7px; border-radius:20px; background:#eef0f3; color:#57606a; flex:none; }
  .ratio.free { background:#e6f6ec; color:var(--ok); }
  .usage-card { margin-top:10px; }
  .usage-cols { display:grid; grid-template-columns:1fr 1fr; gap:14px; margin-top:8px; }
  .usage-cols > div { min-width:0; }
  @media (max-width:900px) { .usage-cols { grid-template-columns:1fr; } }
  .usage-h { font-size:11px; color:#9aa0a8; margin-bottom:3px; }
  .usage-item { display:flex; align-items:center; gap:6px; font-size:12px; color:#57606a; padding:2px 0; }
  .usage-item .k { flex:none; width:70px; font-family:Consolas,monospace; color:var(--txt); }
  .usage-item .k.wide { width:auto; flex:1; min-width:0; white-space:nowrap;
                        overflow:hidden; text-overflow:ellipsis; }
  .usage-item .n { flex:none; width:54px; text-align:right; font-family:Consolas,monospace; }
  .usage-item .n.sum { flex:none; width:66px; }
  .usage-item.head { color:#9aa0a8; border-bottom:1px solid var(--line); margin-bottom:2px; }
  .usage-note { font-size:11px; color:#9aa0a8; }
  .usage-sum { font-size:12px; color:var(--blue); }
</style>
</head>
<body>
<div class="wrap">
  <div class="topbar">
    <div class="brand">AIHub<span class="tag">本地模型网关 · Go</span></div>
    <div id="msg"></div>
    <div class="top-actions">
      <button onclick="refreshAll(true)">刷新</button>
      <button onclick="act('stop_all')">全部停止</button>
      <button class="danger" onclick="exitHub()">退出</button>
    </div>
  </div>

  <div class="checkin-bar">
    <button class="primary" onclick="doCheckin()">一键签到</button>
    <span class="chip no" id="checkin-chip">…</span>
    <span class="sum" id="checkin-sum"></span>
  </div>

  <div class="grid" id="grid"></div>

  <div class="tile agg-card">
    <div class="tile-head"><span class="dot" id="agg-dot"></span>
      <span class="tile-name">Codex 聚合线路<span class="tag">:8790 · 用 Codex 时才启动</span></span>
      <span class="tile-port" id="agg-count"></span></div>
    <div class="tile-btns">
      <button class="primary" onclick="act('agg/start')">启动聚合</button>
      <button onclick="act('agg/stop')">停止聚合</button>
    </div>
    <div class="agg-row">
      <select id="agg-line" onchange="aggLoadLineModels()">
        <option value="intl">国际 WorkBuddy</option>
        <option value="dom">国内 WorkBuddy</option>
        <option value="qoder">Qoder</option>
        <option value="trae">Trae</option>
      </select>
      <select id="agg-model"><option value="">—</option></select>
      <button onclick="aggAdd()">添加到 Codex</button>
    </div>
    <div id="agg-list"></div>
  </div>

  <div class="tile usage-card">
    <div class="tile-head">
      <span class="tile-name">Token 用量<span class="tag"> · Codex 聚合口 · 每次请求实测</span></span>
      <span class="usage-sum" id="usage-sum"></span></div>
    <div class="agg-row">
      <select id="usage-days" onchange="usageRefresh()">
        <option value="7">近 7 天</option>
        <option value="14" selected>近 14 天</option>
        <option value="30">近 30 天</option>
        <option value="0">全部</option>
      </select>
      <button onclick="usageRefresh()">刷新用量</button>
      <span class="usage-note" id="usage-note">统计从聚合口(:8790)取样，直连各线路端口的请求不计入</span>
    </div>
    <div class="usage-cols">
      <div>
        <div class="usage-h">按天</div>
        <div class="usage-item head"><span class="k">日期</span><span class="n">请求</span>
          <span class="n">输入</span><span class="n">输出</span><span class="n sum">合计</span></div>
        <div id="usage-day-list"></div>
      </div>
      <div>
        <div class="usage-h">按模型</div>
        <div class="usage-item head"><span class="k wide">线路 · 模型</span><span class="n">请求</span>
          <span class="n">输入</span><span class="n">输出</span><span class="n sum">合计</span></div>
        <div id="usage-model-list"></div>
      </div>
    </div>
  </div>

  <div class="foot-hint">各代理均为 OpenAI 兼容接口 · 额度不自动查询，每个平台单独点该瓦片上的「刷新额度」</div>
</div>

<script>
const SERVICES = [
  { key:'dom',   name:'国内 WorkBuddy', port:8787 },
  { key:'intl',  name:'国际 WorkBuddy AI', port:8799, cred:true },
  { key:'qoder', name:'Qoder CN（阿里）', port:8797 },
  { key:'trae',  name:'Trae CN（字节）', port:8795 },
  { key:'mimo',  name:'MiMo（小米）', port:8798 },
  { key:'zcode', name:'Zcode（Z.ai）', port:8796, zkey:true },
];
let state = null, credits = null, nicks = {};
const CREDIT_KEYS = ['dom','intl','qoder','trae'];
const msgEl = document.getElementById('msg');

function showMsg(text, err) {
  msgEl.textContent = text || '';
  msgEl.className = err ? 'err' : '';
}

function creditRow(s) {
  const span = '<span id="credit-' + s.key + '">额度 —</span>';
  if (CREDIT_KEYS.indexOf(s.key) < 0) return '<div class="tile-credit">' + span + '</div>';
  return '<div class="tile-credit">' + span +
    '<button class="credit-ref" onclick="loadCredits(true,\'' + s.key + '\')">刷新额度</button></div>';
}
function ensureTiles() {
  const grid = document.getElementById('grid');
  if (grid.children.length) return;
  grid.innerHTML = SERVICES.map(function (s) {
    let extra = '';
    if (s.cred) extra += '<span class="cred-radios" id="cred-row"></span>';
    if (s.zkey) extra += '<input type="password" class="zkey" id="zcode-apikey" placeholder="可选 API Key，留空自动读桌面端凭据">';
    return '<div class="tile">' +
      '<div class="tile-head"><span class="dot" id="dot-' + s.key + '"></span>' +
      '<span class="tile-name">' + s.name + '</span>' +
      '<span class="tile-port">:' + s.port + '</span></div>' +
      '<div class="tile-acct" id="acct-' + s.key + '"></div>' +
      creditRow(s) +
      '<div class="tile-btns">' + extra +
      '<button class="primary" onclick="act(\'' + s.key + '/start\')">启动</button>' +
      '<button onclick="act(\'' + s.key + '/stop\')">停止</button>' +
      (s.cred ? '<span class="spacer"></span><button onclick="act(\'intl/switch\')">换凭证并重启</button>' : '') +
      '</div></div>';
  }).join('');
}

function updateTiles() {
  if (!state) return;
  SERVICES.forEach(function (s) {
    const d = state[s.key] || { up:false, ok:false, detail:'' };
    const dot = document.getElementById('dot-' + s.key);
    const acct = document.getElementById('acct-' + s.key);
    if (!dot) return;
    if (d.up && d.ok) {
      dot.className = 'dot on';
      const nick = nicks[s.key];
      acct.textContent = '账号 ' + (nick || d.detail || '—');
    } else if (d.up) {
      dot.className = 'dot warn';
      acct.textContent = d.detail || '端口占用 /health 异常';
    } else {
      dot.className = 'dot';
      acct.textContent = '已停止';
    }
    const el = document.getElementById('credit-' + s.key);
    if (!el) return;
    const c = credits && credits[s.key];
    if (!c || !c.text) {
      el.textContent = '额度 —';
      el.className = 'stale';
    } else if (!c.ok) {
      el.textContent = c.text;
      el.className = c.text === '查询中…' ? 'stale' : 'bad';
    } else {
      el.textContent = c.text + (c.fetched_at ? '（' + c.fetched_at + '）' : '');
      el.className = '';
    }
  });
  // 国际版凭证单选（仅列表/选中变化时重建，避免打断交互）
  const row = document.getElementById('cred-row');
  if (row && state && row.dataset.cred !== state.intl_cred) {
    row.dataset.cred = state.intl_cred;
    row.innerHTML = '<span>凭证：</span>';
    (state.creds || []).forEach(function (name) {
      const lab = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'cred';
      inp.value = name;
      inp.checked = (name === state.intl_cred);
      inp.onchange = function () { setCred(name); };
      lab.appendChild(inp);
      lab.appendChild(document.createTextNode(name));
      row.appendChild(lab);
    });
  }
}

function renderCheckin() {
  const chip = document.getElementById('checkin-chip');
  const sum = document.getElementById('checkin-sum');
  const c = state && state.checkin;
  if (!c || !c.date) {
    chip.textContent = '今日未签';
    chip.className = 'chip no';
    sum.textContent = '尚未签到过';
    return;
  }
  const today = new Date();
  const todayStr = today.getFullYear() + '-' +
    String(today.getMonth() + 1).padStart(2, '0') + '-' +
    String(today.getDate()).padStart(2, '0');
  const parts = [];
  if (c.dom) parts.push('国内 ' + c.dom);
  if (c.qoder) parts.push('Qoder ' + c.qoder);
  if (c.trae) parts.push('Trae ' + c.trae);
  sum.textContent = c.date + ' ' + (c.time || '') + '：' + parts.join('；');
  if (c.date === todayStr && c.ok) {
    chip.textContent = '今日已签';
    chip.className = 'chip ok';
  } else if (c.date === todayStr) {
    chip.textContent = '今日部分成功';
    chip.className = 'chip warn';
  } else {
    chip.textContent = '今日未签';
    chip.className = 'chip no';
  }
}

function render() {
  ensureTiles();
  updateTiles();
  renderCheckin();
  renderAgg();
}

// ---------- Codex 聚合线路（:8790 启停 + 模型白名单管理） ----------
function ratioBadge(txt) {
  if (!txt) return '';
  return '<span class="ratio' + (txt === '免费' ? ' free' : '') + '">' + txt + '</span>';
}
// 模型下拉（#agg-model）只在「页面加载」和「切换线路」时取过一次，而
// /api/agg/line_models 的答案会随线路启停改变（线路没起来时只回一句提示）。
// 于是启动线路后下拉仍停在旧提示上，必须手动刷新页面才恢复——2026-10-12 用户报的
// 就是这个。这里记住上次取数时的「线路 + 该线路是否在跑」，轮询发现翻转就补拉。
// 只在翻转时补拉（而不是每轮都拉）：否则线路没起来时会每 2 秒重试一次，
// 下拉反复闪「加载中…」。
let aggLineSig = null;

function aggSig() {
  const line = document.getElementById('agg-line').value;
  const up = !!(state && state[line] && state[line].up);
  return line + '|' + (up ? '1' : '0');
}

function renderAgg() {
  if (!state) return;
  const a = state.agg || { up: false, models: 0 };
  const dot = document.getElementById('agg-dot');
  dot.className = a.up ? 'dot on' : 'dot';
  document.getElementById('agg-count').textContent = a.up ? (a.models + ' 个模型') : '';

  const sig = aggSig();
  if (sig !== aggLineSig) {
    aggLineSig = sig;
    aggLoadLineModels();
  }
}
let aggLoadSeq = 0;

async function aggLoadLineModels() {
  const line = document.getElementById('agg-line').value;
  const sel = document.getElementById('agg-model');
  const keep = sel.value;   // 重取时保住用户当前选中的模型
  const seq = ++aggLoadSeq;
  sel.innerHTML = '<option value="">加载中…</option>';
  try {
    const r = await fetch('/api/agg/line_models?line=' + line, { signal: AbortSignal.timeout(8000) });
    const d = await r.json();
    if (seq !== aggLoadSeq) return;   // 期间又切了线路/又补拉了一轮，丢弃过期结果
    if (!d.ok) { sel.innerHTML = '<option value="">' + (d.msg || '暂无模型') + '</option>'; return; }
    const ratios = d.ratios || {};
    sel.innerHTML = (d.models || []).map(function (m) {
      return '<option value="' + m + '">' + m + (ratios[m] ? ' · ' + ratios[m] : '') + '</option>';
    }).join('') || '<option value="">该线路无模型</option>';
    if (keep) sel.value = keep;
  } catch (e) { if (seq === aggLoadSeq) sel.innerHTML = '<option value="">加载失败</option>'; }
}
async function aggRefresh() {
  try {
    const r = await fetch('/api/agg/models', { signal: AbortSignal.timeout(5000) });
    const d = await r.json();
    const box = document.getElementById('agg-list');
    const ratios = d.ratios || {};
    box.innerHTML = (d.models || []).map(function (m) {
      return '<div class="agg-item"><span class="slug">' + m.slug + '</span>' +
        '<span>' + (m.display || '') + '</span>' + ratioBadge(ratios[m.slug]) +
        '<span style="flex:1"></span>' +
        '<button onclick="aggRemove(\'' + m.slug + '\')">删除</button></div>';
    }).join('') || '<div class="agg-item">还没有模型：选好线路点「添加到 Codex」（Codex 端重开会话生效）</div>';
  } catch (e) { /* 忽略 */ }
}
async function aggAdd() {
  const line = document.getElementById('agg-line').value;
  const model = document.getElementById('agg-model').value;
  if (!model) { showMsg('先选线路并确保该线路已启动，才有模型可选', true); return; }
  await post('agg/add', { line: line, model: model });
  aggRefresh();
}
async function aggRemove(slug) {
  await post('agg/remove', { slug: slug });
  aggRefresh();
}

async function post(path, body) {
  try {
    const r = await fetch('/api/' + path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
      signal: AbortSignal.timeout(45000)
    });
    const d = await r.json();
    showMsg(d.msg, !d.ok);
  } catch (e) {
    showMsg('请求失败：' + e, true);
  }
  refresh();
}

function act(path) { post(path, {}); }
function setCred(name) { post('intl/cred', { cred: name }); }

async function doCheckin() {
  showMsg('正在签到…');
  try {
    const r = await fetch('/api/checkin', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
      signal: AbortSignal.timeout(60000)
    });
    const d = await r.json();
    showMsg(d.msg, !d.ok);
  } catch (e) {
    showMsg('签到请求失败：' + e, true);
  }
  refresh();
}

async function startZcode() {
  const apiKey = document.getElementById('zcode-apikey').value.trim();
  try {
    const r = await fetch('/api/zcode/start', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ apiKey: apiKey }),
      signal: AbortSignal.timeout(30000)
    });
    const d = await r.json();
    showMsg(d.msg, !d.ok);
  } catch (e) {
    showMsg('请求失败：' + e, true);
  }
  refresh();
}

function exitHub() {
  if (!confirm('确定退出控制台？（会同时停止全部代理）')) return;
  showMsg('正在退出…');
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
    state = await r.json();
    render();
  } catch (e) {
    showMsg('控制台连接中断：' + e, true);
  }
}

// 昵称较慢（可能读凭据/刷新 token），与高频状态轮询分离；结果并入瓦片文案。
async function refreshNick() {
  try {
    const r = await fetch('/api/nick', { signal: AbortSignal.timeout(10000) });
    const n = await r.json();
    let changed = false;
    for (const k in n) {
      if (n[k] && n[k] !== nicks[k]) { nicks[k] = n[k]; changed = true; }
    }
    if (changed) updateTiles();
  } catch (e) { /* 下次轮询再试 */ }
}

async function loadCredits(force, key) {
  try {
    let url = '/api/credits';
    if (key) {
      url += '?key=' + encodeURIComponent(key) + (force ? '&force=1' : '');
    } else if (force) {
      url += '?force=1';
    }
    const r = await fetch(url, { signal: AbortSignal.timeout(8000) });
    const data = await r.json();
    if (!credits) credits = {};
    if (key) {
      if (data[key]) credits[key] = data[key];
    } else {
      credits = data;
    }
    updateTiles();
    // 强刷后等后台补拉结果回填（只回填该平台）
    if (force) {
      if (key) setTimeout(function () { loadCredits(false, key); }, 5000);
      else setTimeout(loadCredits, 5000);
    }
  } catch (e) { /* 忽略 */ }
}

function refreshAll(force) {
  refresh(); // 额度不随顶部刷新全量拉取，改由各瓦片独立刷新
}

function fmtTok(n) {
  n = Number(n) || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e4) return Math.round(n / 1e3) + 'k';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

async function usageRefresh() {
  const days = document.getElementById('usage-days').value;
  const note = document.getElementById('usage-note');
  try {
    const r = await fetch('/api/usage?days=' + days, { signal: AbortSignal.timeout(15000) });
    const j = await r.json();
    if (!j.ok) { note.textContent = j.msg || '读取用量失败'; return; }
    const s = j.summary || {};
    document.getElementById('usage-sum').textContent =
      (s.reqs || 0) + ' 次 · 合计 ' + fmtTok(s.total) + ' tok';
    const rows = Number(j.recorded) || 0;
    note.textContent = j.from
      ? (j.from + ' → ' + j.to + ' · 累计 ' + rows + ' 条 / ' + j.file_mb + ' MB')
      : ('还没有记录（用一次 Codex 再刷新）· 累计 ' + rows + ' 条');
    const line = function (k, a, wide) {
      return '<div class="usage-item"><span class="k' + (wide ? ' wide' : '') + '">' + k + '</span>' +
        '<span class="n">' + (a.reqs || 0) + '</span>' +
        '<span class="n">' + fmtTok(a.in) + '</span>' +
        '<span class="n">' + fmtTok(a.out) + '</span>' +
        '<span class="n sum">' + fmtTok(a.total) + '</span></div>';
    };
    document.getElementById('usage-day-list').innerHTML =
      (j.days || []).map(function (d) { return line(d.date.slice(5), d.agg, false); }).join('') ||
      '<div class="usage-item"><span class="k">—</span></div>';
    document.getElementById('usage-model-list').innerHTML =
      (j.models || []).map(function (m) { return line(m.line_cn + ' · ' + m.slug, m.agg, true); }).join('') ||
      '<div class="usage-item"><span class="k">—</span></div>';
  } catch (e) { note.textContent = '用量接口请求失败：' + e; }
}

ensureTiles();
refresh();
refreshNick();
aggRefresh();
aggLoadLineModels();
usageRefresh();
setInterval(refresh, 2000);
setInterval(refreshNick, 6000);
// 额度不自动轮询、不自动查询：仅手动点「刷新额度」/右上「刷新」（风控考虑，2026-10-08）
</script>
</body>
</html>
`
