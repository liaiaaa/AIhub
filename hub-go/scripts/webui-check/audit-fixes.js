#!/usr/bin/env node
// ============================================================
//  控制台可用性审计（2026-10-12 用户反馈后新增）
//
//  覆盖三项用户直接感知的问题，防止回归：
//    #1 聚合白名单里「依赖线路没启动」的模型必须灰显 + 标注
//       （否则用户会选到必失败的模型，直到报 line_not_running 才发现）
//    #2 顶部「刷新」必须真正全刷（状态/昵称/额度/用量/白名单）
//    #3 「全部停止」必须二次确认，且文案要点明会停掉 Codex 聚合口
//
//  用法（需先起桩服务，见 stub.py）：
//     python3 scripts/webui-check/stub.py 8899
//     node    scripts/webui-check/audit-fixes.js
// ============================================================
const path=require('path'), fs=require('fs');
function loadPlaywright(){
  const cands=[process.env.PLAYWRIGHT_CORE,
    path.join(__dirname,'..','..','..','tools','zcode-relay','mint','node_modules','playwright-core')].filter(Boolean);
  for(const c of cands) if(fs.existsSync(c)) return require(c);
  throw new Error('找不到 playwright-core：设 PLAYWRIGHT_CORE 指向它的目录');
}
const {chromium}=loadPlaywright();
const CHROME=process.env.CHROME_PATH||'/usr/bin/google-chrome';
const B='http://127.0.0.1:'+(process.env.STUB_PORT||'8899');
let pass=0,fail=0;
const chk=(n,c,g)=>{ if(c){pass++;console.log('  [PASS] '+n);} else {fail++;console.log('  [FAIL] '+n+' -> '+JSON.stringify(g));} };

(async()=>{
  const b=await chromium.launch({executablePath:CHROME,headless:true,args:['--no-sandbox']});
  const p=await b.newPage({viewport:{width:1180,height:900}});

  // ---------- #1 死模型灰显 ----------
  console.log('【#1 白名单：依赖线路没跑的模型必须灰显 + 标注】');
  await fetch(B+'/ctl?agg=1&intl=1&dom=0&qoder=0&trae=0');
  await p.goto(B+'/',{waitUntil:'networkidle'}); await p.waitForTimeout(2000);

  const rows = await p.evaluate(()=>[...document.querySelectorAll('#agg-list .agg-item')].map(e=>({
    text:e.textContent.replace(/\s+/g,' ').trim(), down:e.classList.contains('down'),
    tag:!!e.querySelector('.off-tag'), title:e.getAttribute('title')||''
  })));
  console.log('   行:', rows.map(r=>(r.down?'[灰]':'[亮]')+r.text.slice(0,34)));
  const intlRow = rows.find(r=>r.text.includes('intl.'));
  const domRow  = rows.find(r=>r.text.includes('dom.'));
  chk('国际线路在跑 -> intl 模型不灰显', intlRow && !intlRow.down, intlRow);
  chk('国内线路已停 -> dom 模型灰显', domRow && domRow.down, domRow);
  chk('灰显行带「线路未启动」标签', domRow && domRow.tag, domRow);
  chk('灰显行有 tooltip 说明', domRow && domRow.title.includes('未启动'), domRow && domRow.title);

  // 启动国内线路 -> 该行应立刻变亮，且不需重新打接口
  const calls=[];
  p.on('request',r=>{ const u=r.url(); if(u.includes('/api/agg/models')) calls.push(u); });
  await fetch(B+'/ctl?dom=1'); await p.waitForTimeout(3000);
  const after = await p.evaluate(()=>[...document.querySelectorAll('#agg-list .agg-item')].map(e=>({
    text:e.textContent.replace(/\s+/g,' ').trim(), down:e.classList.contains('down')})));
  const domAfter = after.find(r=>r.text.includes('dom.'));
  chk('启动国内线路后该行立刻变亮（无需刷新页面）', domAfter && !domAfter.down, domAfter);
  chk('且没有重复打 /api/agg/models（纯本地重渲染）', calls.length===0, 'calls='+calls.length);

  // ---------- #3 全部停止二次确认 ----------
  console.log('\n【#3 「全部停止」必须有二次确认】');
  let dialogMsg=null;
  p.on('dialog',async d=>{ dialogMsg=d.message(); await d.dismiss(); });
  await p.click('text=全部停止');
  await p.waitForTimeout(700);
  chk('弹出确认框', dialogMsg!==null, dialogMsg);
  chk('确认文案点明会停掉 Codex 聚合口', !!dialogMsg && dialogMsg.includes('聚合口'), dialogMsg);
  chk('点「取消」后没有发出 stop_all 请求', !calls.some(u=>u.includes('stop_all')), calls);

  // ---------- #2 顶部「刷新」全刷 ----------
  console.log('\n【#2 顶部「刷新」应真正全刷】');
  const r2=[];
  p.on('request',r=>{ const u=r.url(); if(u.includes(':8899')) r2.push(u.replace('http://127.0.0.1:8899','')); });
  await p.click('text=刷新');
  await p.waitForTimeout(3000);
  const hit = s => r2.some(u=>u.startsWith(s));
  chk('刷新了状态 /api/status', hit('/api/status'), r2);
  chk('刷新了额度 /api/credits', hit('/api/credits'), r2);
  chk('刷新了用量 /api/usage', hit('/api/usage'), r2);
  chk('刷新了白名单 /api/agg/models', hit('/api/agg/models'), r2);
  console.log('   实际请求:', [...new Set(r2)]);

  // ---------- #4 占位符 ----------
  console.log('\n【#4 未查询的额度占位】');
  const ph = await p.evaluate(()=>document.querySelector('#grid .tile .tile-credit span').textContent.trim());
  chk('占位不再是孤零零的「额度 —」', !/^额度\s*—$/.test(ph), ph);

  console.log('\n结果: 通过 '+pass+' / 失败 '+fail);
  await b.close(); process.exit(fail?1:0);
})().catch(e=>{console.error('ERR',e);process.exit(1)});
