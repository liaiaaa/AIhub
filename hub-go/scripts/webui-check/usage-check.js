#!/usr/bin/env node
// ============================================================
//  用量表 + 签到条 的渲染断言（2026-10-12 用户反馈后新增）
//
//  防回归的三件事：
//    #1 用量行必须显示「缓存」列——只报「输入」会让人以为烧了上亿 token，
//       实测输入 148.9M 里 146.6M 是缓存命中（折扣计价）。
//    #2 每行要有构成条（新输入/缓存/输出），段宽按「所有行最大合计」归一，
//       所以既能量级对比又能看构成。
//    #3 签到文案要压缩（原来占满整行），且完整原文挂 title 不丢信息。
//
//  用法：python3 scripts/webui-check/stub.py 8899 && node scripts/webui-check/usage-check.js
//  注意：桩服务的 /api/usage 返回空数据，本脚本因此直接注入假数据渲染，
//  不依赖桩的用量内容（但页面与 JS 都取自真实的 webui.go）。
// ============================================================
const path = require('path'), fs = require('fs');
function loadPlaywright() {
  const cands = [process.env.PLAYWRIGHT_CORE,
    path.join(__dirname, '..', '..', '..', 'tools', 'zcode-relay', 'mint', 'node_modules', 'playwright-core')].filter(Boolean);
  for (const c of cands) if (fs.existsSync(c)) return require(c);
  throw new Error('找不到 playwright-core：设 PLAYWRIGHT_CORE 指向它的目录');
}
const { chromium } = loadPlaywright();
const CHROME = process.env.CHROME_PATH || '/usr/bin/google-chrome';
const B = 'http://127.0.0.1:' + (process.env.STUB_PORT || '8899');

let pass = 0, fail = 0;
const chk = (n, c, g) => { if (c) { pass++; console.log('  [PASS] ' + n); } else { fail++; console.log('  [FAIL] ' + n + ' -> ' + JSON.stringify(g)); } };

// 与真实后端同形状的用量数据（取自 2026-10-12 实例的 /api/usage）
const FAKE = {
  ok: true, days: 14, from: '2026-10-11', to: '2026-10-12', recorded: 1292, file_mb: '0.27',
  summary: { reqs: 1292, in: 269311037, out: 1883039, cached: 266287616, reason: 0, total: 271194076, fail: 38 },
  days: [
    { date: '2026-10-12', agg: { reqs: 714, in: 148931633, out: 1370968, cached: 146608384, total: 150302601, fail: 7 } },
    { date: '2026-10-11', agg: { reqs: 578, in: 120379404, out: 512071, cached: 119679232, total: 120891475, fail: 31 } },
  ],
  models: [
    { line: 'intl', line_cn: '国际', slug: 'intl.deepseek-v4.1-flash', agg: { reqs: 1161, in: 248109461, out: 1421456, cached: 245923712, total: 249530917, fail: 8 } },
    { line: 'dom', line_cn: '国内', slug: 'dom.glm-5.3-flash', agg: { reqs: 30, in: 0, out: 0, cached: 0, total: 0, fail: 30 } },
  ],
};

(async () => {
  const b = await chromium.launch({ executablePath: CHROME, headless: true, args: ['--no-sandbox'] });
  const p = await b.newPage({ viewport: { width: 1180, height: 1000 } });

  // 拦掉 /api/usage 喂假数据，其余走桩
  await p.route('**/api/usage*', r => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(FAKE) }));
  await p.goto(B + '/', { waitUntil: 'networkidle' });
  await p.waitForTimeout(1500);

  console.log('【用量表：#1 缓存列 + #2 构成条】');
  const head = await p.evaluate(() => [...document.querySelectorAll('.usage-card .usage-item.head')].map(e => e.textContent.replace(/\s+/g, ' ').trim()));
  chk('表头含「缓存」列', head.every(h => h.includes('缓存')), head);

  const rows = await p.evaluate(() => [...document.querySelectorAll('#usage-day-list .usage-item')].map(e => ({
    text: e.textContent.replace(/\s+/g, ' ').trim(),
    bars: [...e.querySelectorAll('.usage-bar i')].map(i => i.style.width),
    barCount: e.querySelectorAll('.usage-bar i').length,
  })));
  chk('按天有 2 行', rows.length === 2, rows.length);
  chk('每行都有 3 段构成条（新输入/缓存/输出）', rows.every(r => r.barCount === 3), rows.map(r => r.barCount));
  chk('行的数字里出现缓存量（146.61M）', rows[0].text.includes('146.61M'), rows[0].text);
  chk('构成条宽度非全 0（真的按比例画了）', rows[0].bars.some(w => parseFloat(w) > 0), rows[0].bars);
  // 三段是「构成」关系（新输入 + 缓存 + 输出 = 合计），所以最大那行三段之和应为 100%，
  // 而不是某一段独占 100%。这同时验证了归一基准取的是「该列表最大合计」。
  const sum0 = rows[0].bars.reduce((a, w) => a + parseFloat(w), 0);
  chk('最大那行三段之和 ≈ 100%（构成关系正确）', Math.abs(sum0 - 100) < 0.5, sum0.toFixed(2));
  // 两个列表各自归一：按模型里最大的那行也应占满
  const mrows = await p.evaluate(() => [...document.querySelectorAll('#usage-model-list .usage-item')].map(e =>
    [...e.querySelectorAll('.usage-bar i')].map(i => i.style.width)));
  const msum = mrows[0].reduce((a, w) => a + parseFloat(w), 0);
  chk('按模型列表也各自归一（最大行三段和 ≈ 100%）', Math.abs(msum - 100) < 0.5, msum.toFixed(2));

  console.log('【用量汇总：命中缓存要点出来】');
  const sum = await p.evaluate(() => document.getElementById('usage-sum').textContent);
  chk('汇总里说明「命中缓存」', sum.includes('命中缓存'), sum);
  chk('汇总里给出命中占比', /命中缓存（\d+%）/.test(sum), sum);

  console.log('【失败次数：不能只藏在总数里】');
  const failTag = await p.evaluate(() => {
    const e = document.querySelector('#usage-day-list .usage-item .fail');
    return e ? e.textContent : null;
  });
  chk('有失败的行标出失败次数', failTag && /✗\d+/.test(failTag), failTag);

  console.log('【签到条：#3 文案压缩且不丢信息】');
  const ck = await p.evaluate(() => {
    const s = document.getElementById('checkin-sum');
    return { text: s.textContent, title: s.title };
  });
  chk('签到文案不超过 46 字符（原来 ~60）', ck.text.length <= 46, ck.text.length + ': ' + ck.text);
  chk('完整原文保留在 title 里', ck.title.length > ck.text.length, ck.title);
  chk('title 里有原始措辞「签到成功」', ck.title.includes('签到成功'), ck.title);

  console.log('\n结果: 通过 ' + pass + ' / 失败 ' + fail);
  await b.close();
  process.exit(fail ? 1 : 0);
})().catch(e => { console.error('ERR', e); process.exit(1); });
