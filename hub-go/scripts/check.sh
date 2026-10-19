#!/usr/bin/env bash
# ============================================================
#  AIHub (Go) - 提交前检查（vet + test + race）
#
#  用法：
#    ./scripts/check.sh              # 完整检查（含 race，约 80s）
#    ./scripts/check.sh --fast       # 跳过 race（约 45s）
#    ./scripts/check.sh --short      # 只跑 -short 子集（vet + test -short，不含 race）
#
#  为什么要有这个脚本
#  ------------------
#  2026-10-19 第一次跑 `go test -race`，立刻抓到 stopAggLine 不等 Serve
#  goroutine 收尾的数据竞争（生产代码缺陷，存在已久，被测试自身的 race 掩盖）。
#  此前所有检查都是 `go vet` + `go test -short`，从不跑 race —— 并发缺陷因此
#  长期不可见。这个脚本把 race 变成默认动作。
#
#  race 的代价：编译慢 + 运行慢（约 2 倍），全量约 40s → 80s。
#  提交前跑一次完全可以接受；改文档之类不想等时用 --fast。
# ============================================================
set -uo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$SRC_DIR" || exit 1

FAST=0
for arg in "$@"; do
  case "$arg" in
    --fast|--short) FAST=1 ;;
    -h|--help)
      sed -n '3,18p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "未知参数：$arg（用 --help 看用法）" >&2
      exit 2
      ;;
  esac
done

export PATH="$PATH:/usr/local/go/bin"
command -v go >/dev/null || { echo "✗ 找不到 go 命令" >&2; exit 1; }

fail=0
step() { printf '\n== %s ==\n' "$1"; }

# 1) go vet：静态检查（快，永远跑）
step "go vet"
if go vet ./... ; then
  echo "✓ vet 通过"
else
  echo "✗ vet 失败" >&2
  fail=1
fi

# 2) go test -short：功能测试
step "go test -short"
if go test -short . ; then
  echo "✓ 功能测试通过"
else
  echo "✗ 功能测试失败" >&2
  fail=1
fi

# 3) go test -short -race：并发检查（默认跑，--fast 跳过）
if [ "$FAST" -eq 0 ]; then
  step "go test -short -race（并发检查）"
  if go test -short -race . ; then
    echo "✓ 无数据竞争"
  else
    echo "✗ 检测到数据竞争或测试失败" >&2
    echo "  看上面的 'WARNING: DATA RACE' 段：Write/Previous read 两处就是打架的位置。" >&2
    echo "  常见成因：后台 goroutine 收尾时读全局状态，而调用方没等它退出。" >&2
    fail=1
  fi
else
  echo ""
  echo "（--fast：已跳过 race 检查）"
fi

echo ""
if [ "$fail" -eq 0 ]; then
  echo "✓ 全部通过"
  exit 0
fi
echo "✗ 有检查未通过" >&2
exit 1
