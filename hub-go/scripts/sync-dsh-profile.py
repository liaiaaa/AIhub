#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 aihub 聚合网关的模型名同步进 dsh profile。

为什么需要这个脚本
==================
dsh 的 `llm-pi-ai` 插件一旦在 profile 里看到 `models:` 列表，就**完全用配置里的
定义**，不会去请求上游 `/v1/models`（见 dsh-llm-pi-ai/lib/index.js 里
`configured.length > 0 ? configured : ...` 那一行）。所以：

  - aihub 侧倍率/上下文改成上游实时值 → dsh 里**看不到**，除非同步 profile；
  - 而 profile 是 YAML 文本，手工改容易漏、容易写错缩进。

这个脚本从 `:8790/v1/models` 拉当前真值，把 aihub provider 下每个模型的
`name` / `contextWindow` / `maxTokens` 就地更新，其余字段（input、reasoningEfforts
等）原样保留。

用法
====
    python3 hub-go/scripts/sync-dsh-profile.py            # 只预览差异
    python3 hub-go/scripts/sync-dsh-profile.py --write    # 实际写入（自动备份）
    python3 hub-go/scripts/sync-dsh-profile.py --write --profile web

改完 profile **必须重启 dsh** 才生效（profile 只在启动时读一次）。

设计取舍
========
- 只改「已存在于 profile 里的模型」，不自动新增。新增模型该不该挂、挂哪条线路
  是人的决定（用户 2026-10-15 明确要求过：候选单不等于聚合白名单）。
- 只改三个字段：name / contextWindow / maxTokens。input、reasoningEfforts 等
  属于人工决策，脚本不碰。
- 幂等：内容没变化时不写盘，不制造无意义的 mtime 变化。
"""

import argparse
import json
import os
import re
import shutil
import sys
import time
import urllib.request

AGG_URL = "http://127.0.0.1:8790/v1/models"
DEFAULT_PROFILE = os.path.expanduser("~/.dsh/profiles/web/cordis.patch.yml")


def fetch_models(url=AGG_URL, timeout=20):
    """拉聚合网关的模型目录，返回 id -> {name, contextWindow, maxTokens}。"""
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        doc = json.loads(r.read().decode())
    out = {}
    for m in doc.get("data", []):
        mid = m.get("id")
        if not mid:
            continue
        entry = {}
        if m.get("name"):
            entry["name"] = m["name"]
        # 上下文：contextWindow 优先，回退 max_input_tokens
        cw = m.get("contextWindow") or m.get("max_input_tokens")
        if isinstance(cw, int) and cw > 0:
            entry["contextWindow"] = cw
        # 输出上限：max_output_tokens
        mo = m.get("max_output_tokens")
        if isinstance(mo, int) and mo > 0:
            entry["maxTokens"] = mo
        out[mid] = entry
    return out


def sync_text(text, catalog):
    """在 profile 文本里就地更新 aihub provider 下各模型的字段。

    返回 (新文本, 变更列表)。变更列表元素是 (model_id, field, old, new)。

    做法：逐行扫描，靠缩进判断「当前在哪个模型块里」。不引入 YAML 库是为了
    保住原文的注释、空行、`!!js` 表达式等——PyYAML 往返会把这些全吃掉。
    """
    lines = text.split("\n")
    changes = []

    # 定位 aihub provider 的 models 段范围。
    # 结构固定为：  providers: -> aihub: -> models: -> 每个 "- id: xxx"
    in_aihub = False
    in_models = False
    models_indent = None
    item_indent = None
    cur_id = None
    # 每个模型块的字段行号：{id: {field: line_index}}
    field_lines = {}

    for i, line in enumerate(lines):
        stripped = line.strip()
        indent = len(line) - len(line.lstrip())

        # 进入 aihub 块（8 空格缩进的 "aihub:"）
        if re.match(r"^\s{6}aihub:\s*$", line):
            in_aihub = True
            continue
        # 离开 aihub 块：遇到缩进 <= 6 且非空的新键
        if in_aihub and stripped and indent <= 6 and not stripped.startswith("-"):
            in_aihub = False
            in_models = False
            cur_id = None
            continue
        if not in_aihub:
            continue

        if re.match(r"^\s{8}models:\s*$", line):
            in_models = True
            models_indent = indent
            continue
        if not in_models:
            continue

        m = re.match(r"^(\s*)-\s+id:\s*(\S+)\s*$", line)
        if m:
            item_indent = len(m.group(1))
            cur_id = m.group(2).strip("'\"")
            field_lines[cur_id] = {"_indent": item_indent, "_start": i}
            continue

        if cur_id is None:
            continue

        # 模型块内字段（缩进 > item_indent）
        fm = re.match(r"^(\s*)(name|contextWindow|maxTokens):\s*(.*)$", line)
        if fm and len(fm.group(1)) > item_indent:
            field_lines[cur_id][fm.group(2)] = i
            continue

    # 应用变更
    for mid, fields in field_lines.items():
        if mid not in catalog:
            continue
        want = catalog[mid]
        ind = " " * (fields["_indent"] + 2)
        for key in ("name", "contextWindow", "maxTokens"):
            if key not in want:
                continue
            newval = want[key]
            if key in fields:
                li = fields[key]
                old_line = lines[li]
                oldval = old_line.split(":", 1)[1].strip().strip("'\"")
                if oldval == str(newval):
                    continue
                lines[li] = f"{ind}{key}: {newval}"
                changes.append((mid, key, oldval, newval))
            else:
                # 字段缺失：插到 name 行之后（保持 name 在最前）
                anchor = fields.get("name", fields["_start"])
                lines.insert(anchor + 1, f"{ind}{key}: {newval}")
                # 后续行号全部后移，需要重算
                for other in field_lines.values():
                    if other["_start"] > anchor:
                        other["_start"] += 1
                    for k2, v2 in list(other.items()):
                        if isinstance(v2, int) and v2 > anchor:
                            other[k2] = v2 + 1
                changes.append((mid, key, "(缺)", newval))

    return "\n".join(lines), changes


def main():
    ap = argparse.ArgumentParser(description="把 aihub 的模型倍率/上下文同步进 dsh profile")
    ap.add_argument("--write", action="store_true", help="实际写入（默认只预览）")
    ap.add_argument("--profile", default=None, help="profile 名或 cordis.patch.yml 路径")
    ap.add_argument("--url", default=AGG_URL, help="聚合网关 /v1/models 地址")
    args = ap.parse_args()

    if args.profile is None:
        path = DEFAULT_PROFILE
    elif args.profile.endswith(".yml"):
        path = args.profile
    else:
        path = os.path.expanduser(f"~/.dsh/profiles/{args.profile}/cordis.patch.yml")

    if not os.path.exists(path):
        print(f"✗ 找不到 profile：{path}", file=sys.stderr)
        return 1

    try:
        catalog = fetch_models(args.url)
    except Exception as e:
        print(f"✗ 拉不到 {args.url}：{e}", file=sys.stderr)
        print("  先确认 aihub 在跑：curl -s http://127.0.0.1:8801/api/status", file=sys.stderr)
        return 1

    with open(path, encoding="utf-8") as f:
        original = f.read()

    updated, changes = sync_text(original, catalog)

    if not changes:
        print("✓ 已是最新，无需改动")
        return 0

    print(f"发现 {len(changes)} 处差异：\n")
    by_model = {}
    for mid, field, old, new in changes:
        by_model.setdefault(mid, []).append((field, old, new))
    for mid, items in by_model.items():
        print(f"  {mid}")
        for field, old, new in items:
            print(f"      {field}: {old}  →  {new}")

    if not args.write:
        print(f"\n（预览模式，未写入。加 --write 实际修改 {path}）")
        return 0

    if updated == original:
        print("✓ 内容无变化，不写盘")
        return 0

    backup = f"{path}.bak-sync-{time.strftime('%Y%m%d-%H%M%S')}"
    shutil.copy2(path, backup)
    with open(path, "w", encoding="utf-8") as f:
        f.write(updated)
    print(f"\n✓ 已写入 {path}")
    print(f"  备份：{backup}")
    print("\n⚠ 改完必须重启 dsh 才生效（profile 只在启动时读一次）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
