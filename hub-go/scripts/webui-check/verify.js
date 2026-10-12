#!/usr/bin/env node
// ============================================================
//  控制台网页（webui.go）UI 回归：聚合卡片「模型下拉」随状态刷新
//
//  回归的 bug（2026-10-12）：点「启动聚合」后，模型下拉仍停在
//  「线路未启动，请先在上方启动它」，手动刷新页面才恢复。
//
//  用法：
//     python3 scripts/webui-check/stub.py 8899     # 一个终端
//     node    scripts/webui-check/verify.js        # 另一个终端
//
//  依赖 headless Chrome（本机 /usr/bin/google-chrome）。浏览器驱动用
//  playwright-core，路径优先取仓库内 zcode-relay 里那份，其次取环境变量
//  PLAYWRIGHT_CORE。
// ============================================================
const path = require('path');
const fs = require('fs');

function loadPlaywright() {
  const cands = [
    process.env.PLAYWRIGHT_CORE,
    path.join(__dirname, '..', '..', '..', 'tools', 'zcode-relay', 'mint', 'node_modules', 'playwright-core'),
  ].filter(Boolean);
  for (const c of cands) {
    if (fs.existsSync(c)) return require(c);
  }
  throw new Error('找不到 playwright-core：设 PLAYWRIGHT_CORE 指向它的目录');
}

const CHROME = process.env.CHROME_PATH || '/usr/bin/google-chrome';
const PORT = process.env.STUB_PORT || '8899';
const B = 'http://127.0.0.1:' + PORT;

const { chromium } = loadPlaywright();

const snap = p => p.evaluate(() => ({
  dot: document.getElementById('agg-dot').className,
  count: document.getElementById('agg-count').textContent,
  model: [...document.getElementById('agg-model').options].map(o => o.textContent),
}));

let pass = 0, fail = 0;
function chk(name, cond, got) {
  if (cond) { pass++; console.log('  [PASS] ' + name); }
  else { fail++; console.log('  [FAIL] ' + name + '  -> 实际: ' + JSON.stringify(got)); }
}

(async () => {
  const b = await chromium.launch({ executablePath: CHROME, headless: true, args: ['--no-sandbox'] });
  const p = await b.newPage();

  console.log('【场景1】原 bug：先开线路 -> 再点「启动聚合」，全程不刷新页面');
  await fetch(B + '/ctl?agg=0&intl=0&dom=0');
  await p.goto(B + '/', { waitUntil: 'networkidle' });
  await p.waitForTimeout(1200);
  chk('加载时下拉显示未启动提示', (await snap(p)).model.join('').includes('线路未启动'), await snap(p));

  await fetch(B + '/ctl?intl=1');   // 等价于在瓦片上点了「启动」
  await p.waitForTimeout(3000);
  let s = await snap(p);
  chk('线路起来后下拉自动补上模型（无需刷新）', s.model.join(',').includes('deepseek-v4.1-flash'), s);

  await p.click('text=启动聚合');
  await p.waitForTimeout(2000);
  s = await snap(p);
  chk('点「启动聚合」后圆点亮', s.dot.includes('on'), s);
  chk('点「启动聚合」后模型数显示', s.count.includes('3 个模型'), s);
  chk('模型下拉仍然正确', s.model.join(',').includes('deepseek-v4.1-flash'), s);

  console.log('【场景2】切线路：dom 未启动 -> 启动 dom -> 下拉应跟随');
  await fetch(B + '/ctl?dom=0');
  await p.selectOption('#agg-line', 'dom');
  await p.waitForTimeout(1500);
  s = await snap(p);
  chk('切到未启动的 dom，显示提示', s.model.join('').includes('线路未启动'), s);
  await fetch(B + '/ctl?dom=1');
  await p.waitForTimeout(3000);
  s = await snap(p);
  chk('dom 起来后下拉自动补上', s.model.join(',').includes('deepseek-v4.1-flash'), s);

  console.log('【场景3】不应每 2 秒重试（否则下拉会反复闪「加载中…」）');
  await fetch(B + '/ctl?intl=0');
  await p.selectOption('#agg-line', 'intl');
  await p.waitForTimeout(1500);
  let flips = 0, prev = null;
  for (let i = 0; i < 10; i++) {
    await p.waitForTimeout(500);
    const t = (await snap(p)).model.join(',');
    if (prev !== null && t !== prev) flips++;
    prev = t;
  }
  chk('线路未启动时下拉内容稳定（无反复闪动）', flips === 0, 'flips=' + flips);

  console.log('【场景4】选中模型在重取后保留');
  await fetch(B + '/ctl?intl=1');
  await p.waitForTimeout(3000);
  await p.selectOption('#agg-model', 'glm-5.2');
  await p.waitForTimeout(300);
  await fetch(B + '/ctl?intl=0');
  await p.waitForTimeout(300);
  await fetch(B + '/ctl?intl=1');
  await p.waitForTimeout(3000);
  const keep = await p.evaluate(() => document.getElementById('agg-model').value);
  chk('线路重启后仍保留用户选中的模型', keep === 'glm-5.2', keep);

  console.log('\n结果: 通过 ' + pass + ' / 失败 ' + fail);
  await b.close();
  process.exit(fail ? 1 : 0);
})().catch(e => { console.error('ERR', e); process.exit(1); });
