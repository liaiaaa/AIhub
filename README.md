# WorkBuddy Hub

**全部程序集中在此目录。** 桌面控制台一键开关国内 / 国际路由，并可切换国际「<凭证目录A>」。

## 使用（Go 版，推荐）

| 步骤 | 操作 |
|------|------|
| 首次编译 | 双击 `hub-go\build.bat`（需 `E:\software\go\bin\go.exe`，缓存放 E 盘不装 C 盘） |
| 启动 | 双击 `hub-go\启动控制台-Go.bat`，浏览器自动打开 `http://127.0.0.1:8801` |
| 控制 | 网页上手动点「启动 / 停止」，凭证单选「<凭证目录A> / <凭证目录B>」，底部可「全部停止 / 退出」 |

Go 版特点：

- 单进程单 exe（`hub-go\workbuddy-hub.exe`，约 7.6MB），三个代理按需在进程内启停，无 Python 解释器固定开销。
- **启动默认全部停止**：启动时自动清扫残留进程，由你手动选择开启哪一个。
- 实测内存：空闲 ≈45MB，双代理同时运行 ≈49MB（私有内存）；Python 版对应约 30+44+43≈117MB。
- 换凭证：网页上选凭证 →「换凭证并重启」；偏好写入 `state.json`。
- 「退出控制台」会先停所有代理再退出；关浏览器窗口≠退出（代理继续跑）。
- **直连策略**：国内（8787）和 MiMo（8798）直连后端，不读系统代理——切换网络 / 关闭代理软件也能用；只有国际（8799）`www.workbuddy.ai` 需你的代理 / VPN 开启（会读取系统代理设置）。
- 日志：`hub.log`（控制台）、`converter-cn.log` / `converter-intl.log` / `converter-mimo.log`（各代理一行摘要）。
- 改代码后重新双击 `build.bat` 即可。

## MiMo（小米）代理

| 项 | 值 |
|------|------|
| 端口 | 8798 |
| 后端 | `mimo-server-cn.xiaomimimo.com`（小米桌面端 SSO 免费额度，直连） |
| 模型 | `mimo-x-flash-preview` / `mimo-x-pro-preview` |
| 地址 | DSH 填 `http://127.0.0.1:8798/v1`（**必须带 `http://` 前缀**） |
| 说明 | 需本机登录过 MiMo 桌面端；读取登录 cookie 时 MiMo 桌面端需关闭 |

## 使用（Python 版，备选）

双击 `启动控制台.bat`。

| 区域 | 端口 | 启动脚本 |
|------|------|----------|
| 国内 | 8787 | `converter_cn.py` |
| 国际 | 8799 | `converter.py`（`--auth-dir` 指向所选凭证） |

凭证：

- `<凭证目录A>` / `<凭证目录B>`（默认，李1 无额度时用）
- `<凭证目录C>` 保留在目录中，**不进**切换列表

## DSH

- 国内：`http://127.0.0.1:8787/v1`
- 国际：`http://127.0.0.1:8799/v1`

国际默认模型：`deepseek-v4.1-flash`。

## 目录

```
workbuddy-hub\
├── 启动控制台.bat        # Python 版
├── manager.py
├── converter.py        # 国际
├── converter_cn.py     # 国内
├── desensitize.py
├── hub-go\             # Go 版（推荐）
│   ├── workbuddy-hub.exe
│   ├── build.bat       # 首次编译
│   └── 启动控制台-Go.bat
├── <凭证目录A>\  <凭证目录B>\  <凭证目录C>\
└── README.md
```

原先的 `codebuddy2openai-main`、`codebuddy_guoji` 可在确认本目录双端都正常后删除。

## 说明

- **启动默认全部停止**：打开控制台会自动清理 8787/8799 残留进程，由用户手动选择启动国内 / 国际。
- 国际不设 `--api-key`；换凭证 = 停 8799 再按目录启动。
- 状态探活为轻量轮询（TCP 探测每 3 秒，完整 /health 约 12 秒一次）；偏好在 `state.json`。
- 转换器默认只记一行请求摘要；需完整请求/响应日志时给 converter_cn.py 加 `--debug`。
- 换机器改 `manager.py` 里 `PYTHON`。
- 依赖：`fastapi`、`uvicorn[standard]`、`httpx`。
