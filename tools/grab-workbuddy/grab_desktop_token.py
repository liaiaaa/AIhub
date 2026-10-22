#!/usr/bin/env python3
"""
WorkBuddy 国际站（www.workbuddy.ai）取 hub-go 凭证 —— 官方设备授权流。

链路（逆向自社区开源实现，见文末参考）：
  1) POST /v2/plugin/auth/state?platform=CLI   → {state, authUrl}
  2) 用【已登录 workbuddy 的浏览器】打开 authUrl → 服务端把该会话绑定到 state
  3) GET  /v2/plugin/auth/token?state=<state>  → code:0 时返回
        accessToken / refreshToken / expiresIn / refreshExpiresIn / tokenType / sessionState / scope / domain
     （未完成时 code:11217 "login ing..."，继续轮询）
  4) GET  /v2/plugin/login/account?state=<state> + Authorization: Bearer <accessToken>
        → uid / nickname / uin / type
  5) 组装成 hub-go 的 .info（auth / account / accounts / allAccounts）

要点：无 PKCE、无需桌面端、无需从 Network 面板复制 Bearer。
      网页 API 本身走 Cookie 会话，不发 Authorization —— 所以 DevTools 里找不到 Bearer 是正常的。

用法：
  python3 grab_desktop_token.py --out <数据根>/cred-d/workbuddy-desktop-ai.info
  # 浏览器已登录哪个账号，就签发哪个账号的票。换账号先在浏览器里切换登录。
  python3 grab_desktop_token.py --no-open      # 只打印授权链接，自己点
  python3 grab_desktop_token.py --expect user@example.com   # 校验账号，不符则不落盘

参考：linguo2625469/workbuddy2api-panel  internal/panel/login.go
      Sliverkiss/workbuddy2api           admin/browser_login.py
"""
import argparse, base64, glob, json, os, subprocess, sys, time, urllib.error, urllib.parse, urllib.request

BASE = "https://www.workbuddy.ai"
UA = "CLI/2.63.2 CodeBuddy/2.63.2"
HEADERS = {
    "Content-Type": "application/json",
    "Accept": "application/json, text/plain, */*",
    "X-Requested-With": "XMLHttpRequest",
    "Origin": BASE,
    "Referer": BASE + "/",
    "User-Agent": UA,
}



CHROME_CANDIDATES = ["/opt/google/chrome/chrome", "/usr/bin/google-chrome",
                     "/usr/bin/google-chrome-stable"]


def _x_env():
    env = dict(os.environ)
    if not env.get("DBUS_SESSION_BUS_ADDRESS") and os.path.exists("/run/user/%d/bus" % os.getuid()):
        env["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=/run/user/%d/bus" % os.getuid()
    if not env.get("DISPLAY"):
        env["DISPLAY"] = ":0"
        for cand in sorted(glob.glob("/run/user/%d/.mutter-Xwaylandauth.*" % os.getuid())):
            env["XAUTHORITY"] = cand
            break
    return env


def incognito_windows():
    """Chrome 窗口标题列表（读 AT-SPI 属性，不读像素）；不可用时返回 None。

    Atspi.init() 连不上无障碍总线会直接 abort 进程，所以放到子进程里跑。
    """
    probe = os.path.join(os.path.dirname(os.path.abspath(__file__)), "_atspi_titles.py")
    if not os.path.exists(probe):
        return None
    try:
        r = subprocess.run([sys.executable, probe], env=_x_env(), timeout=15,
                           stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            return None
        return [ln for ln in r.stdout.decode("utf-8", "ignore").splitlines() if ln]
    except Exception:
        return None


def open_in_browser(url, incognito=True):
    """向已运行的 Chrome 实例传 --incognito 会新开一个无痕窗口；实例没开时该参数同样生效。"""
    env = _x_env()
    exe = next((c for c in CHROME_CANDIDATES if os.path.exists(c)), None)
    before = incognito_windows()
    cmds = []
    if exe:
        cmds.append([exe, "--incognito", url] if incognito else [exe, url])
    cmds.append(["xdg-open", url])
    for cmd in cmds:
        try:
            r = subprocess.run(cmd, env=env, timeout=25,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if r.returncode != 0:
                continue
        except Exception:
            continue
        if not incognito:
            return True
        # 确认无痕窗口真的起来了：标题应带「无痕」，且出现了 workbuddy 页
        for _ in range(12):
            time.sleep(0.5)
            titles = incognito_windows()
            if titles is None:
                return True          # 读不到 AT-SPI，就别卡住流程
            if any(("无痕" in t or "Incognito" in t) for t in titles):
                return True
        return False
    return False


def api(path, method="GET", body=None, bearer=None, timeout=30):
    h = dict(HEADERS)
    if bearer:
        h["Authorization"] = "Bearer " + bearer
    data = json.dumps(body).encode() if body is not None else (b"{}" if method == "POST" else None)
    req = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as f:
            return json.loads(f.read()), f.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return json.loads(raw), e.code
        except Exception:
            sys.exit(f"[FATAL] {path} → HTTP {e.code}: {raw[:200].decode('utf-8','ignore')}")


def b64json(seg):
    return json.loads(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, help="要写出的 .info 路径")
    ap.add_argument("--timeout", type=int, default=300, help="等待浏览器完成授权的秒数")
    ap.add_argument("--no-open", action="store_true", help="不自动开浏览器，只打印链接")
    ap.add_argument("--no-incognito", dest="incognito", action="store_false",
                    help="用普通窗口打开（默认：无痕窗口，避免沿用普通 profile 的旧账号）")
    ap.add_argument("--expect", default="", help="期望的账号（邮箱/昵称），不符则拒绝落盘")
    ap.add_argument("--type", default="personal")
    args = ap.parse_args()

    obj, _ = api("/v2/plugin/auth/state?platform=CLI", "POST", {})
    if obj.get("code") != 0:
        sys.exit(f"[FATAL] 申请 state 失败: {obj}")
    state = obj["data"]["state"]
    auth_url = obj["data"]["authUrl"]
    print(f"[1/5] state={state}")
    print(f"      authUrl={auth_url}")
    if args.no_open:
        print("      请手动在浏览器里打开上面的链接（建议无痕窗口 + 目标账号）")
    else:
        ok = open_in_browser(auth_url, incognito=args.incognito)
        print("      已在无痕窗口打开授权页" if ok else
              "      自动打开失败，请把上面的 authUrl 手动贴进无痕窗口")

    print(f"[2/5] 轮询取票（最多 {args.timeout}s）…")
    deadline = time.time() + args.timeout
    tok = None
    while time.time() < deadline:
        o, _ = api("/v2/plugin/auth/token?" + urllib.parse.urlencode({"state": state}))
        code = o.get("code")
        if code == 0:
            tok = o["data"]
            print("      授权完成，已拿到令牌")
            break
        if code != 11217:
            sys.exit(f"[FATAL] 取票异常: {o}")
        print(f"      等待授权中… ({int(args.timeout-(deadline-time.time()))}s 已过)", flush=True)
        time.sleep(3)
    if not tok:
        sys.exit("\n[FATAL] 超时：浏览器里没有已登录的 workbuddy 会话，或授权未完成")
    print()

    at = tok["accessToken"]
    pay = b64json(at.split(".")[1])
    hdr = b64json(at.split(".")[0])
    if hdr.get("alg") != "RS256" or pay.get("azp") != "console":
        sys.exit(f"[FATAL] 令牌形态异常 alg={hdr.get('alg')} azp={pay.get('azp')}")

    # 账号信息：优先取 /v2/plugin/login/account，失败则退回 JWT claims
    acct = {"uid": pay.get("sub", ""), "nickname": pay.get("preferred_username", ""),
            "uin": "", "type": args.type}
    try:
        o, _ = api("/v2/plugin/login/account?" + urllib.parse.urlencode({"state": state}), bearer=at)
        if o.get("code") == 0 and isinstance(o.get("data"), dict):
            d = o["data"]
            acct.update({k: d[k] for k in ("uid", "nickname", "uin", "type") if d.get(k) not in (None, "")})
            acct["type"] = d.get("type") or args.type
    except SystemExit:
        pass

    who = acct["nickname"] or acct["uid"]
    print(f"[3/5] 账号: {who}  uid={acct['uid']}")
    print(f"      email={pay.get('email')}  scope={pay.get('scope')}")

    if args.expect and args.expect not in (who, acct["uid"], pay.get("email")):
        sys.exit(f"[FATAL] 账号不符：期望 {args.expect}，实际 {who}。\n"
                 "        请在浏览器里切换登录到目标账号后重试。")

    # 官方 userinfo 复核
    try:
        r = urllib.request.Request(BASE + "/auth/realms/copilot/protocol/openid-connect/userinfo",
                                   headers={"Authorization": "Bearer " + at,
                                            "User-Agent": "codebuddy-guoji/1.0"})
        with urllib.request.urlopen(r, timeout=25) as f:
            ui = json.loads(f.read())
        print(f"[4/5] userinfo 200 → {ui.get('preferred_username')} / {ui.get('email')}")
    except Exception as e:
        sys.exit(f"[FATAL] userinfo 校验失败，令牌无效：{e}")

    now_ms = int(time.time() * 1000)
    exp = int(pay["exp"])
    expires_in = int(tok.get("expiresIn") or (exp - time.time()))
    refresh_in = int(tok.get("refreshExpiresIn") or expires_in)
    auth = {
        "accessToken": at,
        "refreshToken": tok.get("refreshToken", ""),
        "tokenType": tok.get("tokenType", "Bearer"),
        "domain": tok.get("domain", "www.workbuddy.ai"),
        "expiresAt": exp * 1000,
        "expiresIn": expires_in,
        "refreshExpiresAt": (exp + refresh_in) * 1000 if refresh_in else 0,
        "refreshExpiresIn": refresh_in,
        "lastRefreshTime": now_ms,
        "scope": tok.get("scope") or pay.get("scope") or "openid profile offline_access email",
    }
    if tok.get("sessionState"):
        auth["sessionState"] = tok["sessionState"]
    doc = {"auth": auth, "account": acct, "accounts": [dict(acct)], "allAccounts": [dict(acct)]}

    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    tmp = args.out + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False)
    os.replace(tmp, args.out)
    try:
        os.chmod(args.out, 0o600)
    except OSError:
        pass
    print(f"[5/5] 已写出 {args.out}  ({os.path.getsize(args.out)} bytes)")
    print(f"      有效期至 {time.strftime('%Y-%m-%d', time.localtime(exp))}"
          f"（{expires_in/86400:.0f} 天），含 refreshToken={bool(auth['refreshToken'])}")


if __name__ == "__main__":
    main()
