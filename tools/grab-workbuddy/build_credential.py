#!/usr/bin/env python3
"""
把从浏览器抓到的 WorkBuddy AI (国际站) OAuth token 组装成 hub-go 凭证文件。

用法：
  python3 build_credential.py --token '<accessToken>' [--refresh '<refreshToken>'] \
      --out /home/<user>/opt/AI_test/hub-go/cred-d/workbuddy-desktop-ai.info
  # 也支持环境变量：WB_ACCESS_TOKEN / WB_REFRESH_TOKEN

做的事：
  1) 解 JWT payload 拿 sub / preferred_username / email / exp / scope
  2) 用 /auth/realms/copilot/protocol/openid-connect/userinfo 验 token 有效性
  3) 只有确认为 Keycloak 的 console client Bearer token 才落盘
"""
import argparse, base64, json, os, sys, time, urllib.error, urllib.request

ISSUER = "https://www.workbuddy.ai/auth/realms/copilot"
USERINFO = ISSUER + "/protocol/openid-connect/userinfo"
DOMAIN = "www.workbuddy.ai"
UA = "codebuddy-guoji/1.0"
# 365 天 - 56 秒，和桌面端观测到的 expiresIn 对齐
DEFAULT_EXPIRES_IN = 31535944


def b64url_json(seg: str):
    pad = "=" * (-len(seg) % 4)
    return json.loads(base64.urlsafe_b64decode(seg + pad))


def decode_jwt(tok: str):
    parts = tok.split(".")
    if len(parts) != 3:
        sys.exit(f"[FATAL] token 不是三段式 JWT（拿到 {len(parts)} 段）")
    return b64url_json(parts[0]), b64url_json(parts[1])


def verify(tok: str):
    req = urllib.request.Request(
        USERINFO, headers={"Authorization": "Bearer " + tok, "User-Agent": UA})
    try:
        with urllib.request.urlopen(req, timeout=25) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read()[:200]
    except Exception as e:                       # noqa: BLE001
        return "ERR", repr(e)[:200]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--token")
    ap.add_argument("--refresh", default="")
    ap.add_argument("--out", required=True)
    ap.add_argument("--type", default="personal")
    args = ap.parse_args()

    tok = (args.token or os.environ.get("WB_ACCESS_TOKEN") or "").strip()
    if not tok:
        sys.exit("[FATAL] 没有 accessToken（--token 或 WB_ACCESS_TOKEN）")
    refresh = (args.refresh or os.environ.get("WB_REFRESH_TOKEN") or "").strip()
    if tok.lower().startswith("bearer "):
        tok = tok[7:].strip()

    hdr, pay = decode_jwt(tok)
    print(f"[check] JWT header : alg={hdr.get('alg')} typ={hdr.get('typ')}")
    print(f"[check] azp(claim) : {pay.get('azp')}")
    print(f"[check] iss        : {pay.get('iss')}")
    print(f"[check] sub        : {pay.get('sub')}")
    print(f"[check] nickname   : {pay.get('preferred_username')}")
    print(f"[check] exp        : {pay.get('exp')}")

    # 必须是 Keycloak 给 console client 签发的 Bearer，否则 hub-go 后端不认
    if hdr.get("alg") != "RS256":
        sys.exit(f"[FATAL] alg={hdr.get('alg')}，不是 Keycloak 的 RS256 授权 token。\n"
                 "        这是应用层的 agentos token，打 /v2/chat/completions 会 401。\n"
                 "        请从 Network 面板里 API 请求的 Request Headers 复制 Authorization。")
    if pay.get("azp") != "console":
        print(f"[WARN ] azp={pay.get('azp')}，网页版自己用的是 console")

    code, body = verify(tok)
    print(f"[verify] userinfo -> HTTP {code}")
    if code != 200:
        sys.exit(f"[FATAL] token 无效，userinfo 返回 {code}: {body}")
    ui = body
    print(f"[verify] 用户信息   : {json.dumps(ui, ensure_ascii=False)[:200]}")

    now_ms = int(time.time() * 1000)
    exp = pay.get("exp")
    if exp:
        expires_at = int(exp) * 1000
        expires_in = max(0, int(exp) - int(time.time()))
    else:
        expires_at = now_ms + DEFAULT_EXPIRES_IN * 1000
        expires_in = DEFAULT_EXPIRES_IN

    r_pay = None
    if refresh:
        try:
            r_pay = decode_jwt(refresh)[1]
        except SystemExit:
            r_pay = None
    if r_pay and r_pay.get("exp"):
        refresh_expires_at = int(r_pay["exp"]) * 1000
        refresh_expires_in = max(0, int(r_pay["exp"]) - int(time.time()))
    else:
        refresh_expires_at, refresh_expires_in = 0, 0

    account = {
        "uid": pay.get("sub", ""),
        "nickname": pay.get("preferred_username", ""),
        "uin": "",
        "type": args.type,
    }
    doc = {
        "auth": {
            "accessToken": tok,
            "refreshToken": refresh,
            "tokenType": "Bearer",
            "domain": DOMAIN,
            "expiresAt": expires_at,
            "expiresIn": expires_in,
            "refreshExpiresAt": refresh_expires_at,
            "refreshExpiresIn": refresh_expires_in,
            "lastRefreshTime": now_ms,
            "scope": pay.get("scope", "openid profile offline_access email"),
        },
        "account": account,
        "accounts": [dict(account)],
        "allAccounts": [dict(account)],
    }

    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    tmp = args.out + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False)
    os.replace(tmp, args.out)
    try:
        os.chmod(args.out, 0o600)
    except OSError:
        pass
    print(f"\n[OK] 已写入 {args.out}  ({os.path.getsize(args.out)} bytes)")
    print(f"[OK] 账号 {account['nickname']} / uid {account['uid']}")
    print(f"[OK] expiresAt {expires_at} = {time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(expires_at/1000))}"
          f"  (expiresIn {expires_in}s ≈ {expires_in/86400:.0f} 天)")


if __name__ == "__main__":
    main()
