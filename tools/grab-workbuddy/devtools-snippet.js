// 在无痕窗口的 DevTools Console 里整段粘贴执行。
// 前提：Network 面板里能看到 API 请求带 Authorization: Bearer eyJhbGciOiJSUzI...
// （页面刷新后才会发；若 console 里拿不到，就从 Network 面板手动复制那条头）
(async () => {
  const ISSUER = 'https://www.workbuddy.ai/auth/realms/copilot';
  const tok = (() => {
    // 1) 常见存放点挨个找
    const keys = ['access_token','accessToken','token','auth','oauth_token','console_token','kc_access'];
    for (const store of [localStorage, sessionStorage]) {
      for (let i = 0; i < store.length; i++) {
        const k = store.key(i);
        let v; try { v = JSON.parse(store.getItem(k)); } catch (e) { v = store.getItem(k); }
        const cands = typeof v === 'string' ? [v]
          : (v && typeof v === 'object' ? Object.values(v).filter(x => typeof x === 'string') : []);
        for (const c of cands) {
          const t = c.replace(/^Bearer\s+/i, '').trim();
          if (/^eyJhbGciOiJSUzI/.test(t)) return t;   // 只要 Keycloak RS256
        }
      }
    }
    return null;
  })();
  if (!tok) {
    // 抓不到就直接抓一次真实请求：看 fetch/XHR 有没有被包一层带 token 的全局对象
    console.warn('localStorage/sessionStorage 里没有 RS256 的 Keycloak token。'
      + '请去 Network 面板点一个 /console/ 或 /v2/ 的 XHR，复制 Request Headers 里的 Authorization。');
    return;
  }
  const r = await fetch(ISSUER + '/protocol/openid-connect/userinfo',
    { headers: { Authorization: 'Bearer ' + tok } });
  const j = await r.json().catch(() => null);
  const [h, p] = tok.split('.');
  const dec = s => JSON.parse(decodeURIComponent(escape(atob(s.replace(/-/g, '+').replace(/_/g, '/')))));
  const payload = dec(p);
  console.log('HTTP', r.status, j);
  console.log('sub    =', payload.sub);
  console.log('nick   =', payload.preferred_username);
  console.log('email  =', payload.email);
  console.log('exp    =', payload.exp, new Date(payload.exp * 1000).toLocaleString());
  console.log('scope  =', payload.scope);
  console.log('\n----- accessToken (复制这一整行) -----\n' + tok);
  window.__wb = { tok, payload };   // 方便再取
})();
