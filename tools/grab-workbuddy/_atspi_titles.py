#!/usr/bin/env python3
"""AT-SPI 窗口标题探测（独立进程：Atspi.init() 连不上总线会 abort，不能在主流程里调）。
输出 Chrome 各窗口标题，一行一个；拿不到就非零退出。"""
import os, sys
os.environ.setdefault("AT_SPI_BUS_ADDRESS", "unix:path=/run/user/%s/at-spi/bus" % os.getuid())
if not os.environ.get("DBUS_SESSION_BUS_ADDRESS") and os.path.exists("/run/user/%s/bus" % os.getuid()):
    os.environ["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=/run/user/%s/bus" % os.getuid()
try:
    import gi
    gi.require_version("Atspi", "2.0")
    from gi.repository import Atspi
except Exception:
    sys.exit(3)
Atspi.init()
d = Atspi.get_desktop(0)
app = None
for i in range(d.get_child_count()):
    a = d.get_child_at_index(i)
    if a and a.get_name() == "Google Chrome":
        app = a
        break
if app is None:
    sys.exit(4)
for j in range(app.get_child_count()):
    w = app.get_child_at_index(j)
    try:
        print(w.get_name() or "")
    except Exception:
        print("")
