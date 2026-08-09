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
#
# 用 go vet 而不是 go build，有两个原因：
#   1. `go build` **根本不编译 _test.go**。测试文件里写错类型/少个括号照样"绿"，
#      提交推送之后才在 CI 或别人机器上炸掉。go vet 会连测试文件一起编译。
#   2. vet 不落任何产物，不会像 `go build` 那样把 main 包的二进制吐在仓库根，
#      被别人的 `git add -A` 裹挟进提交。
#
# -tags：本仓库用到 integration / smoke 两个 build tag（test/integration、
# internal/mdns/responder_integration_test.go、cmd/stupidsamba/shutdown_smoke_test.go）。
# 不带 tag 时这些文件被整体排除，等于没校验；而仓库里没有 `//go:build !integration`
# 这类反向约束，所以"带上 tag 编译的文件集合"是"不带 tag"的超集，跑一遍就够。
# 将来若真出现反向约束，这里要改成带 tag / 不带 tag 各跑一遍。
VET_TAGS=integration,smoke

if [ "$#" -eq 0 ]; then
    echo ">>> go vet -tags=$VET_TAGS ./...  (整树)"
    go vet -tags="$VET_TAGS" ./...
    ADD_ARGS="-A"
else
    # 把传入的路径（文件或目录）归约成所属的**包目录**再校验。
    #
    # 这里以前写的是 `[ -d "$REPO/$p" ]` 成立才校验，可 AGENTS.md 约定各 agent
    # 传的大多是**文件**路径，于是校验被整个跳过 —— 制造过多次"假绿"提交。
    PATTERNS=$(
        for p in "$@"; do
            p=${p#./}
            if [ -d "$REPO/$p" ]; then
                d=$p
            else
                # 文件路径；文件已被删除时 dirname 依然给得出所属目录
                d=$(dirname "$p")
            fi
            if [ "$d" = "." ]; then
                # 仓库根：只校验根包自身，别退化成整树 vet
                ls "$REPO"/*.go >/dev/null 2>&1 && echo "."
                continue
            fi
            # 目录可能随文件一起被删了
            [ -d "$REPO/$d" ] || continue
            echo "./$d/..."
        done | sort -u
    )

    # 用 go list 把 pattern 展开成真实存在的包，再交给 vet。这一步不能省：
    #   - configs/ 这类纯资源目录压根没有包；
    #   - scripts/clients/gosmb2 是**独立 module**（自带 go.mod），
    #     主 module 的 `./scripts/...` 同样匹配不到它。
    # 而 `go vet` 收到匹配不到包的 pattern 会直接报错退出，set -e 下就把提交
    # 拦在门外了。go list 遇到这种 pattern 只在 stderr 警告一句，不失败。
    VET_PKGS=""
    if [ -n "$PATTERNS" ]; then
        # shellcheck disable=SC2086
        VET_PKGS=$(go list -tags="$VET_TAGS" $PATTERNS 2>/dev/null || true)
    fi

    if [ -n "$VET_PKGS" ]; then
        echo ">>> go vet -tags=$VET_TAGS $(echo "$VET_PKGS" | tr '\n' ' ')"
        # shellcheck disable=SC2086
        go vet -tags="$VET_TAGS" $VET_PKGS
    else
        echo ">>> 传入路径下没有 Go 包，跳过编译校验"
    fi
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
