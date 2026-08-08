#!/bin/sh
# save.sh —— 多 agent 并行开发时的安全提交推送脚本。
#
# 用法：
#   scripts/save.sh "<模块>: <说明>" <路径>...      # 推荐：只校验并提交指定路径
#   scripts/save.sh "<模块>: <说明>"                # 不传路径 = 整树（仅 team-lead 用）
#
# 例：
#   scripts/save.sh "wire: 实现 SMB2 Header" internal/smb/wire internal/smb/status
#   scripts/save.sh "vfs: 路径安全校验"       internal/vfs
#
# 为什么要传路径：
#   5 个 agent 共用同一个工作树 /workspace。如果整树 `go build ./...`，
#   任何一个人的半成品都会卡住所有人的提交；`git add -A` 还会把别人
#   写到一半的文件裹挟进你的提交。传路径可以把彼此完全隔离。
#
# 各 agent 的路径：
#   wire   → internal/smb/wire internal/smb/status
#   auth   → internal/auth internal/smb/crypto
#   vfs    → internal/vfs
#   server → internal/server internal/smb/command internal/smb/dialect test/integration
#   mdns   → internal/mdns internal/config cmd configs
#
# 注意：这是**开发脚本**，不是软件运行时依赖，不违反 AGENTS.md C3。
set -e

MSG="$1"
if [ -z "$MSG" ]; then
    echo "用法: scripts/save.sh \"<模块>: <说明>\" [路径...]" >&2
    exit 1
fi
shift

cd "$(dirname "$0")/.."
REPO=$(pwd)
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

# ---------------------------------------------------------------- 1. 编译校验

if [ "$#" -eq 0 ]; then
    echo ">>> go build ./...  (整树)"
    go build ./...
    ADD_ARGS="-A"
else
    for p in "$@"; do
        # 只对含 .go 文件的目录做编译校验；configs/ 这类纯资源目录直接跳过
        if [ -d "$REPO/$p" ] && [ -n "$(find "$REPO/$p" -name '*.go' -print -quit)" ]; then
            echo ">>> go build ./$p/..."
            go build "./$p/..."
        fi
    done
    ADD_ARGS="$*"
fi

# ---------------------------------------------------------------- 2. 提交

# shellcheck disable=SC2086
git add $ADD_ARGS

if git diff --cached --quiet; then
    echo ">>> 无暂存改动，跳过提交"
else
    git commit -q -m "$MSG"
    echo ">>> 已提交: $MSG"
fi

# ---------------------------------------------------------------- 3. 推送
#
# 用 mkdir 做互斥锁（mkdir 是原子的），避免多个 agent 同时 rebase 打架。
# rebase 期间工作树会被短暂改写，并发进行会互相破坏。

LOCK="$REPO/.git/stupidsamba-push.lock"

acquire() {
    i=0
    while [ "$i" -lt 120 ]; do
        if mkdir "$LOCK" 2>/dev/null; then
            echo "$$" > "$LOCK/pid"
            return 0
        fi
        # 清理陈旧锁（持有者已死）
        if [ -f "$LOCK/pid" ]; then
            oldpid=$(cat "$LOCK/pid" 2>/dev/null || echo "")
            if [ -n "$oldpid" ] && ! kill -0 "$oldpid" 2>/dev/null; then
                echo ">>> 清理陈旧锁 (pid $oldpid 已退出)" >&2
                rm -rf "$LOCK"
                continue
            fi
        fi
        i=$((i + 1))
        sleep 1
    done
    echo ">>> 等待推送锁超时" >&2
    return 1
}

acquire || exit 1
trap 'rm -rf "$LOCK"' EXIT INT TERM

if git push -q origin main 2>/dev/null; then
    echo ">>> 已推送"
    exit 0
fi

i=1
while [ "$i" -le 6 ]; do
    echo ">>> 推送被拒，第 $i 次同步远端 ..." >&2
    # --autostash 保护其他 agent 未提交的工作树改动
    if git pull --rebase --autostash -q origin main && git push -q origin main; then
        echo ">>> 已推送 (第 $i 次重试)"
        exit 0
    fi
    # rebase 冲突要人工处理，不要静默重试掩盖问题
    if [ -d "$REPO/.git/rebase-merge" ] || [ -d "$REPO/.git/rebase-apply" ]; then
        echo ">>> rebase 冲突，已中止。请手工解决后重试。" >&2
        git rebase --abort 2>/dev/null || true
        exit 1
    fi
    sleep $((i * 2))
    i=$((i + 1))
done

echo ">>> 推送失败，请手动处理" >&2
exit 1
