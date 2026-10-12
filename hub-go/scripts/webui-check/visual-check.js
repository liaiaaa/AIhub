// 依赖 playwright-core（默认取仓库内 zcode-relay 那份）与 headless Chrome。
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

  console.log('【视觉状态断言】左侧色条 / 瓦片类名随状态变化');
  await fetch(B+'/ctl?agg=1&intl=1&dom=0&qoder=0&trae=0');
  await p.goto(B+'/',{waitUntil:'networkidle'}); await p.waitForTimeout(2000);

  const cls = await p.evaluate(()=>{
    const g=k=>{ const d=document.getElementById('dot-'+k); return d? d.closest('.tile').className : null; };
    return {intl:g('intl'), dom:g('dom'), agg:document.querySelector('.agg-card').className};
  });
  chk('运行中的线路瓦片带 on 类（绿色条）', /(^|\s)on(\s|$)/.test(cls.intl||''), cls.intl);
  chk('已停止的线路瓦片不带 on/warn 类（灰色条）', !/(^|\s)(on|warn)(\s|$)/.test(cls.dom||''), cls.dom);
  chk('运行中的聚合卡片带 on 类', /(^|\s)on(\s|$)/.test(cls.agg||''), cls.agg);

  // 关掉国际线路，色条应立刻变灰
  await fetch(B+'/ctl?intl=0'); await p.waitForTimeout(3000);
  const after = await p.evaluate(()=>document.getElementById('dot-intl').closest('.tile').className);
  chk('线路停止后 on 类被移除（色条变灰）', !/(^|\s)on(\s|$)/.test(after), after);

  // 已停止的账号行应该是灰色弱化样式
  const acctCls = await p.evaluate(()=>document.getElementById('acct-intl').className);
  chk('已停止的账号行用弱化样式(.off)', /(^|\s)off(\s|$)/.test(acctCls), acctCls);

  console.log('\n结果: 通过 '+pass+' / 失败 '+fail);
  await b.close(); process.exit(fail?1:0);
})().catch(e=>{console.error('ERR',e);process.exit(1)});
