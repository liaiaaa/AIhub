#!/usr/bin/env python3
# ============================================================
#  控制台网页（webui.go）离线验证桩服务
#
#  为什么需要它：webui.go 里内嵌的页面有一批「随状态变化才重取」的下拉/列表，
#  直接对着线上 :8801 点按钮验证会打断正在跑的代理（本机 Codex 就挂在 :8790 上）。
#  这个桩把 /api/* 换成可编程的假数据，页面本身仍是从 webui.go 里抽出来的真货，
#  于是可以在不碰线上实例的前提下，用真实浏览器复现并回归 UI 行为。
#
#  用法：
#     python3 stub.py [port]          # 默认 8899
#     然后在另一个终端跑 verify.js
#
#  控制接口（给测试脚本用）：
#     GET /ctl?agg=1&intl=0&dom=1     置各路线的「已启动」状态
# ============================================================
import http.server, json, sys, io, os, re, socketserver, urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
WEBUI = os.path.join(HERE, "..", "..", "webui.go")


def extract_html(path=WEBUI):
    """从 webui.go 里抽出内嵌的 webHTML 模板串（两个反引号之间的内容）。"""
    s = io.open(path, encoding="utf-8").read()
    i = s.index("const webHTML = `") + len("const webHTML = `")
    j = s.rindex("`")
    return s[i:j]


HTML = extract_html()
S = {"agg": False, "intl": False, "dom": False, "qoder": False, "trae": False}


class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _j(self, o, code=200):
        b = json.dumps(o, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        if u.path == "/":
            b = HTML.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)
            return
        if u.path == "/ctl":
            for k in list(S):
                if k in q:
                    S[k] = q[k][0] == "1"
            return self._j(S)
        if u.path == "/api/status":
            def line(up):
                return {"up": up, "ok": up, "nick": "",
                        "detail": "workbuddy-desktop.info" if up else ""}
            out = {k: line(S[k]) for k in ("dom", "intl", "qoder", "trae")}
            out["mimo"] = line(False)
            out["zcode"] = line(False)
            out["agg"] = {"up": S["agg"], "models": 3}
            out["creds"] = ["cred-a"]
            out["intl_cred"] = "cred-a"
            out["default_cred"] = "cred-a"
            # 用与真实实例同形状的签到记录（含原始措辞，便于验证 title 不丢信息）
            out["checkin"] = {"date": "2026-10-12", "time": "10:46:02",
                              "dom": "签到成功，+100 积分", "qoder": "已停用",
                              "trae": "签到成功", "ok": True}
            return self._j(out)
        if u.path == "/api/nick":
            return self._j({})
        if u.path == "/api/agg/models":
            return self._j({"ok": True, "models": [
                {"line": "intl", "slug": "intl.deepseek-v4.1-flash",
                 "backend": "deepseek-v4.1-flash", "display": "DeepSeek 4.1 Flash（国际）"},
                {"line": "dom", "slug": "dom.glm-5.3-flash",
                 "backend": "glm-5.3-flash", "display": "GLM 5.3 Flash（国内）"}],
                "ratios": {"intl.deepseek-v4.1-flash": "免费"}})
        if u.path == "/api/agg/line_models":
            ln = (q.get("line") or ["intl"])[0]
            if not S.get(ln, False):
                return self._j({"ok": False, "msg": "线路未启动，请先在上方启动它",
                                "models": []})
            return self._j({"ok": True, "models": ["deepseek-v4.1-flash", "glm-5.2"],
                            "ratios": {"deepseek-v4.1-flash": "免费"}})
        if u.path == "/api/usage":
            return self._j({"days": [], "lines": [], "models": [], "summary": {},
                            "from": "", "to": "", "span": 0, "file_mb": "0",
                            "recorded": 0})
        if u.path == "/api/credits":
            return self._j({})
        if u.path == "/api/intl/creds":
            # 模拟：一个可用、一个限流中、一个账号级风控
            return self._j({"ok": True, "creds": [
                {"name": "cred-a", "nick": "bob@example.com", "uid": "u1",
                 "remain": 160, "total": 190, "ok": False, "state": "风控",
                 "reason": "账号级风控(11140)", "selected": False},
                {"name": "cred-b", "nick": "alice", "uid": "u2",
                 "remain": 130, "total": 160, "ok": False, "state": "限流",
                 "until": "20:01", "remain_txt": "还剩 42 分钟",
                 "reason": "用量限流(6004)", "selected": False},
                {"name": "cred-d", "nick": "user@example.com", "uid": "u3",
                 "remain": 346, "total": 350, "ok": True, "state": "可用",
                 "selected": True},
            ]})
        return self._j({})

    def do_POST(self):
        u = urllib.parse.urlparse(self.path)
        n = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(n)
        if u.path == "/api/agg/start":
            S["agg"] = True
            return self._j({"ok": True, "msg": "聚合线路已启动（:8790，3 个模型）"})
        if u.path == "/api/agg/stop":
            S["agg"] = False
            return self._j({"ok": True, "msg": "聚合已停止"})
        return self._j({"ok": True, "msg": "ok"})


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8899
    socketserver.TCPServer.allow_reuse_address = True
    print("webui 桩服务: http://127.0.0.1:%d/  （HTML 取自 %s）" % (port, WEBUI))
    socketserver.TCPServer(("127.0.0.1", port), H).serve_forever()
