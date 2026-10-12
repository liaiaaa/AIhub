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
<title>AIHub · 本地模型网关</title>
<style>
  :root {
    --ok:#0f9d58; --warn:#c77700; --err:#d93025; --off:#9aa3ad;
    --bg:#f5f6f8; --card:#ffffff; --line:#e5e7eb; --line2:#eef0f3;
    --txt:#16181d; --muted:#69707a; --faint:#9aa3ad;
    --brand:#3b5bfd; --brand2:#7c3aed;
    --radius:14px;
    --shadow:0 1px 2px rgba(16,24,40,.04), 0 4px 16px rgba(16,24,40,.05);
    --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  }
  * { box-sizing:border-box; }
  html, body { margin:0; }
  body {
    font-family:system-ui,-apple-system,"Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif;
    background:var(--bg);
    background-image:radial-gradient(1100px 460px at 50% -220px, #eaefff 0%, rgba(234,239,255,0) 72%);
    color:var(--txt); font-size:14px; line-height:1.5;
    padding:0 18px 34px;
    -webkit-font-smoothing:antialiased;
  }
  .wrap { max-width:1080px; margin:0 auto; }

  /* ---------- 顶栏（吸顶 + 毛玻璃） ---------- */
  .topbar {
    position:sticky; top:0; z-index:20;
    display:flex; align-items:center; gap:14px; flex-wrap:wrap;
    padding:15px 0 12px; margin-bottom:14px;
    background:linear-gradient(180deg, rgba(245,246,248,.97) 62%, rgba(245,246,248,0));
    backdrop-filter:saturate(1.6) blur(8px);
  }
  .brand { display:flex; align-items:center; gap:10px; font-size:17px;
           font-weight:700; letter-spacing:.2px; white-space:nowrap; }
  .brand::before {
    content:""; width:26px; height:26px; border-radius:8px; flex:none;
    background:linear-gradient(135deg,var(--brand),var(--brand2));
    box-shadow:0 2px 8px rgba(59,91,253,.35);
  }
  .brand .tag { font-size:11.5px; font-weight:500; color:var(--muted);
                padding-left:10px; border-left:1px solid var(--line); }
  #msg { flex:1; min-width:160px; font-size:12.5px; color:var(--muted);
         min-height:18px; word-break:break-all; }
  #msg.err { color:var(--err); font-weight:500; }
  .top-actions { display:flex; gap:7px; }

  button {
    font:inherit; font-size:12.5px; line-height:1;
    border:1px solid var(--line); background:var(--card); color:var(--txt);
    border-radius:9px; padding:7px 13px; cursor:pointer; white-space:nowrap;
    transition:background .15s, border-color .15s, box-shadow .15s, transform .06s;
  }
  button:hover { background:#f7f8fa; border-color:#d8dce2; }
  button:active { transform:translateY(1px); }
  button.primary {
    background:linear-gradient(180deg,#4a67ff,#3b5bfd); border-color:#3450e8;
    color:#fff; font-weight:600; box-shadow:0 1px 2px rgba(59,91,253,.3);
  }
  button.primary:hover { background:linear-gradient(180deg,#3f5cf5,#3450e8); }
  button.danger { color:var(--err); border-color:#f2d3d0; }
  button.danger:hover { background:#fdf3f2; border-color:#e8b4b0; }

  /* ---------- 签到条 ---------- */
  .checkin-bar {
    display:flex; align-items:center; gap:11px; flex-wrap:wrap;
    background:var(--card); border:1px solid var(--line); border-radius:var(--radius);
    padding:11px 14px; margin-bottom:14px; box-shadow:var(--shadow);
  }
  .checkin-bar .sum { flex:1; font-size:12.5px; color:var(--muted); min-width:200px; }
  .chip { font-size:11.5px; font-weight:600; padding:3px 11px; border-radius:999px;
          white-space:nowrap; border:1px solid transparent; }
  .chip.ok { background:#e8f7ee; color:#0b7a43; border-color:#c7ebd6; }
  .chip.no { background:#f1f3f6; color:#6b7280; border-color:#e5e8ed; }
  .chip.warn { background:#fdf4e3; color:#a35a00; border-color:#f6e2bd; }

  /* ---------- 瓦片网格 ---------- */
  .grid { display:grid; grid-template-columns:repeat(3,1fr); gap:12px; }
  @media (max-width:880px) { .grid { grid-template-columns:repeat(2,1fr); } }
  @media (max-width:580px) { .grid { grid-template-columns:1fr; } }

  .tile {
    position:relative; overflow:hidden;
    background:var(--card); border:1px solid var(--line); border-radius:var(--radius);
    padding:13px 14px 12px; display:flex; flex-direction:column; gap:7px;
    box-shadow:var(--shadow);
    transition:border-color .18s, box-shadow .18s;
  }
  /* 左侧状态色条：一眼看出哪条线路在跑 */
  .tile::before {
    content:""; position:absolute; left:0; top:0; bottom:0; width:3px;
    background:var(--line2); transition:background .18s;
  }
  .tile.on::before { background:linear-gradient(180deg,#22c55e,#0f9d58); }
  .tile.warn::before { background:linear-gradient(180deg,#fbbf24,#d97706); }
  .tile.on { border-color:#d9ecdf; }
  .tile.warn { border-color:#f3e3c8; }

  .tile-head { display:flex; align-items:center; gap:8px; }
  .tile-name { font-weight:650; font-size:13.5px; flex:1;
               white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  .tile-name .tag { font-size:11px; font-weight:500; color:var(--faint); margin-left:6px; }
  .tile-port { font-size:11px; color:var(--faint); font-family:var(--mono); flex:none; }
  .dot { width:8px; height:8px; border-radius:50%; background:var(--off); flex:none;
         transition:background .18s, box-shadow .18s; }
  .dot.on { background:var(--ok); box-shadow:0 0 0 3px rgba(15,157,88,.16); }
  .dot.warn { background:var(--warn); box-shadow:0 0 0 3px rgba(199,119,0,.16); }
  .tile-acct { font-size:12px; color:var(--muted); min-height:17px;
               white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  .tile-acct.off { color:var(--faint); }
  .tile-credit { font-size:12px; color:var(--brand); min-height:17px;
                 display:flex; align-items:center; gap:7px; }
  .tile-credit span { flex:1; min-width:0; white-space:nowrap;
                      overflow:hidden; text-overflow:ellipsis; }
  .tile-credit span.stale { color:var(--faint); }
  .tile-credit span.bad { color:var(--err); }
  .credit-ref { padding:3px 9px; font-size:11px; flex:none; border-radius:7px; }
  .tile-btns { display:flex; gap:6px; margin-top:2px; flex-wrap:wrap; align-items:center; }
  .tile-btns .spacer { flex:1; }
  .tile-btns button { padding:6px 12px; }
  .cred-radios { font-size:11.5px; color:var(--muted); display:flex; gap:9px;
                 flex-wrap:wrap; align-items:center; width:100%; }
  .cred-radios label { cursor:pointer; display:flex; align-items:center; gap:4px; }
  .cred-radios input { accent-color:var(--brand); margin:0; }
  .zkey { flex:1; min-width:120px; padding:6px 10px; border:1px solid var(--line);
          border-radius:9px; font:inherit; font-size:12px; background:#fbfbfc; color:var(--txt); }
  .zkey:focus { outline:none; border-color:var(--brand); background:var(--card);
                box-shadow:0 0 0 3px rgba(59,91,253,.1); }

  /* ---------- 聚合线路 / Token 用量 ---------- */
  .agg-card, .usage-card { margin-top:12px; }
  .agg-row { display:flex; gap:7px; flex-wrap:wrap; align-items:center; margin-top:5px; }
  .agg-row select {
    padding:6px 10px; border:1px solid var(--line); border-radius:9px;
    font:inherit; font-size:12px; background:#fbfbfc; color:var(--txt);
    max-width:230px; cursor:pointer;
  }
  .agg-row select:focus { outline:none; border-color:var(--brand);
                          box-shadow:0 0 0 3px rgba(59,91,253,.1); }
  .agg-item { display:flex; align-items:center; gap:9px; font-size:12px;
              color:var(--muted); padding:7px 10px; border-radius:9px;
              background:#fafbfc; border:1px solid var(--line2); margin-top:6px; }
  .agg-item .slug { font-family:var(--mono); color:var(--txt); font-size:11.5px; }
  .agg-item button { padding:4px 10px; font-size:11.5px; border-radius:7px; }
  .ratio { font-size:11px; font-weight:600; padding:2px 8px; border-radius:999px;
           background:#eef0f4; color:var(--muted); flex:none; }
  .ratio.free { background:#e8f7ee; color:#0b7a43; }
  /* 依赖线路没启动的白名单行：整行弱化 + 明确标注，避免选到必失败的模型 */
  .agg-item.down { background:#f7f8fa; border-style:dashed; }
  .agg-item.down .slug, .agg-item.down > span { color:var(--faint); }
  .agg-item.down .ratio { opacity:.55; }
  .off-tag { font-size:10.5px; font-weight:600; padding:2px 7px; border-radius:999px;
             background:#fdf4e3; color:#a35a00; border:1px solid #f6e2bd; flex:none; }

  .usage-cols { display:grid; grid-template-columns:1fr 1fr; gap:18px; margin-top:10px; }
  .usage-cols > div { min-width:0; }
  @media (max-width:900px) { .usage-cols { grid-template-columns:1fr; } }
  .usage-h { font-size:11px; font-weight:600; color:var(--faint);
             margin-bottom:5px; letter-spacing:.5px; }
  .usage-item { display:flex; align-items:center; gap:8px; font-size:12px;
                color:var(--muted); padding:5px 2px; border-bottom:1px solid var(--line2); }
  .usage-item:last-child { border-bottom:none; }
  .usage-item .k { flex:none; width:74px; font-family:var(--mono); color:var(--txt); }
  .usage-item .k.wide { width:auto; flex:1; min-width:0; white-space:nowrap;
                        overflow:hidden; text-overflow:ellipsis; }
  .usage-item .n { flex:none; width:58px; text-align:right; font-family:var(--mono);
                   font-variant-numeric:tabular-nums; }
  .usage-item .n.sum { width:70px; color:var(--txt); font-weight:600; }
  .usage-item.head { color:var(--faint); font-size:11px; font-weight:600;
                     border-bottom:1px solid var(--line); padding-bottom:6px; }
  .usage-note { font-size:11.5px; color:var(--faint); }
  .usage-sum { font-size:12.5px; font-weight:600; color:var(--brand); }
  .foot-hint { margin-top:20px; font-size:11.5px; color:var(--faint); text-align:center; }

  /* ---------- 深色模式 ---------- */
  @media (prefers-color-scheme:dark) {
    :root {
      --bg:#0f1216; --card:#171b21; --line:#272d36; --line2:#20252c;
      --txt:#e8eaed; --muted:#9aa3ad; --faint:#6b7280;
      --brand:#7c96ff; --brand2:#a78bfa;
      --shadow:0 1px 2px rgba(0,0,0,.3), 0 4px 16px rgba(0,0,0,.25);
    }
    body { background-image:radial-gradient(1100px 460px at 50% -220px, #1a2033 0%, rgba(26,32,51,0) 72%); }
    .topbar { background:linear-gradient(180deg, rgba(15,18,22,.97) 62%, rgba(15,18,22,0)); }
    button:hover { background:#1e242c; border-color:#333a44; }
    .zkey, .agg-row select { background:#12161b; }
    .agg-item { background:#141820; }
    .chip.ok { background:#12301f; color:#5fd493; border-color:#1d4a30; }
    .chip.no { background:#1c2027; color:#9aa3ad; border-color:#2a3038; }
    .chip.warn { background:#33260f; color:#e0a54a; border-color:#4a3818; }
    .ratio { background:#222831; color:#9aa3ad; }
    .ratio.free { background:#12301f; color:#5fd493; }
    .agg-item.down { background:#13161b; }
    .off-tag { background:#33260f; color:#e0a54a; border-color:#4a3818; }
    .tile.on { border-color:#1f3a2a; }
    .tile.warn { border-color:#3a2f18; }
    button.danger { border-color:#4a2622; }
    button.danger:hover { background:#2a1714; }
  }
</style>
</head>
<body>
<div class="wrap">
  <div class="topbar">
    <div class="brand">AIHub<span class="tag">本地模型网关 · Go</span></div>
    <div id="msg"></div>
    <div class="top-actions">
      <button onclick="refreshAll()">刷新</button>
      <button onclick="stopAll()">全部停止</button>
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
    const tileEl = dot.closest('.tile');
    if (d.up && d.ok) {
      dot.className = 'dot on';
      if (tileEl) { tileEl.classList.add('on'); tileEl.classList.remove('warn'); }
      acct.className = 'tile-acct';
      const nick = nicks[s.key];
      acct.textContent = '账号 ' + (nick || d.detail || '—');
    } else if (d.up) {
      dot.className = 'dot warn';
      if (tileEl) { tileEl.classList.add('warn'); tileEl.classList.remove('on'); }
      acct.className = 'tile-acct';
      acct.textContent = d.detail || '端口占用 /health 异常';
    } else {
      dot.className = 'dot';
      if (tileEl) { tileEl.classList.remove('on'); tileEl.classList.remove('warn'); }
      acct.className = 'tile-acct off';
      acct.textContent = '已停止';
    }
    const el = document.getElementById('credit-' + s.key);
    if (!el) return;
    const c = credits && credits[s.key];
    if (!c || !c.text) {
      el.textContent = '额度未查询';
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
let aggUpSig = null;

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
  const aggTile = dot.closest('.tile');
  if (aggTile) aggTile.classList.toggle('on', !!a.up);
  document.getElementById('agg-count').textContent = a.up ? (a.models + ' 个模型') : '';

  const sig = aggSig();
  if (sig !== aggLineSig) {
    aggLineSig = sig;
    aggLoadLineModels();
  }

  // 任一条线路启停都要更新白名单的灰显（纯本地重渲染，不打接口）
  const upSig = ['dom', 'intl', 'qoder', 'trae'].map(function (k) {
    return k + ((state[k] && state[k].up) ? '1' : '0');
  }).join(',');
  if (upSig !== aggUpSig) {
    aggUpSig = upSig;
    renderAggList();
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
// 白名单数据（模型 + 倍率）只在增删/加载时变；而「依赖线路在不在跑」随状态变。
// 所以把数据缓存下来，渲染拆成两步：拉数据后存缓存，再按当前 state 本地重渲染。
// 这样状态轮询发现线路启停时，能立刻更新灰显而不必重新打接口。
let aggModelsCache = null;

// 白名单行：依赖线路没启动的置灰并标注——否则用户会选到必失败的模型，
// 直到请求报 line_not_running 才发现（2026-10-12 实测 4 个里 3 个是这种）。
function renderAggList() {
  const box = document.getElementById('agg-list');
  if (!box) return;
  if (!aggModelsCache) {
    box.innerHTML = '<div class="agg-item">加载中…</div>';
    return;
  }
  const models = aggModelsCache.models || [];
  const ratios = aggModelsCache.ratios || {};
  box.innerHTML = models.map(function (m) {
    const up = !!(state && state[m.line] && state[m.line].up);
    return '<div class="agg-item' + (up ? '' : ' down') + '"' +
      (up ? '' : ' title="该模型所在线路（' + (aggLineLabel(m.line)) + '）未启动，现在用它会被拒绝。先在上方启动那条线路。"') + '>' +
      '<span class="slug">' + m.slug + '</span>' +
      '<span>' + (m.display || '') + '</span>' + ratioBadge(ratios[m.slug]) +
      (up ? '' : '<span class="off-tag">线路未启动</span>') +
      '<span style="flex:1"></span>' +
      '<button onclick="aggRemove(\'' + m.slug + '\')">删除</button></div>';
  }).join('') || '<div class="agg-item">还没有模型：选好线路点「添加到 Codex」（Codex 端重开会话生效）</div>';
}

function aggLineLabel(line) {
  const sel = document.getElementById('agg-line');
  if (sel) {
    for (const o of sel.options) {
      if (o.value === line) return o.textContent;
    }
  }
  return line;
}

async function aggRefresh() {
  try {
    const r = await fetch('/api/agg/models', { signal: AbortSignal.timeout(5000) });
    aggModelsCache = await r.json();
    renderAggList();
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

// 全部停止会连 Codex 聚合口一起停掉（聚合口正给 Codex 供流时会当场断流），
// 破坏力与「退出」相当，所以和退出一样加二次确认。
function stopAll() {
  if (!confirm('确定停止全部代理？\n\n会同时停掉 Codex 聚合口（:8790）——如果 Codex 正在通过它对话，会立刻断流。')) return;
  post('stop_all', {});
}
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

// 顶部「刷新」= 全刷：状态 + 昵称 + 额度 + 用量 + 白名单。
// 额度仍不自动轮询（风控，2026-10-08），只有这里和各瓦片按钮会主动查。
function refreshAll() {
  refresh();
  refreshNick();
  loadCredits(true);
  usageRefresh();
  aggRefresh();
  aggLoadLineModels();
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
refresh().then(function () { renderAggList(); });   // state 到位后再渲染一次白名单
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
