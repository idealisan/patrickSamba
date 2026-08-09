#!/bin/sh
# restore-history.sh —— 容器重启后，把仓库里的 CodeBuddy 会话历史放回原位。
#
# 背景：容器崩溃会清空 git 仓库以外的一切，包括 ~/.codebuddy 下的会话记录。
#   `codebuddy -c` 接的是「最近一次」会话 —— 崩溃后你新开的那个空会话就是最近的，
#   于是恢复出一个空壳，这就是「resume 不管用」的真相。
#   而 `codebuddy --resume=<uuid>` 是**可用的**（已实测），前提是那个 uuid 的
#   jsonl 还在 ~/.codebuddy/projects/workspace/ 下。本脚本负责把它放回去。
#
# 用法：
#   scripts/restore-history.sh           # 列出仓库里存了哪些会话
#   scripts/restore-history.sh <uuid>    # 放回指定会话（前缀匹配即可）
#   scripts/restore-history.sh --all     # 全部放回
#
# 恢复后直接：codebuddy --resume=<uuid>
# 这条路**不会重跑任何工作**，只是把历史上下文读回来，代价远低于从头再来。
#
# 注意：这是**开发脚本**，不是软件运行时依赖，不违反 AGENTS.md C3。
set -e

DST=${CODEBUDDY_PROJECT_DIR:-/root/.codebuddy/projects/workspace}

cd "$(dirname "$0")/.."
SRC="$(pwd)/history"

if [ ! -d "$SRC" ]; then
    echo "!!! 仓库里没有 history/ 目录，说明还没人跑过 scripts/save-history.sh" >&2
    exit 1
fi

list() {
    echo "仓库里的会话快照（$SRC）："
    echo
    for f in "$SRC"/*.jsonl; do
        [ -e "$f" ] || { echo "  (空)"; return; }
        uuid=$(basename "$f" .jsonl)
        lines=$(wc -l < "$f" | tr -d ' ')
        size=$(du -h "$f" | cut -f1)
        subs=$(ls "$SRC/$uuid/subagents"/*.jsonl 2>/dev/null | wc -l | tr -d ' ')
        printf '  %s  %6s 行  %6s  子 agent %s 个\n' "$uuid" "$lines" "$size" "$subs"
    done
    echo
    echo "详细索引见 history/INDEX.md。放回某个会话：scripts/restore-history.sh <uuid>"
}

copy_one() {
    # $1 = uuid
    uuid=$1
    mkdir -p "$DST"
    cp -p "$SRC/$uuid.jsonl" "$DST/$uuid.jsonl"
    n=1
    if [ -d "$SRC/$uuid" ]; then
        ( cd "$SRC/$uuid" && find . -type f ) | while read -r f; do
            mkdir -p "$DST/$uuid/$(dirname "$f")"
            cp -p "$SRC/$uuid/$f" "$DST/$uuid/$f"
        done
        n=$(( $(ls "$SRC/$uuid/subagents"/*.jsonl 2>/dev/null | wc -l) ))
    fi
    echo ">>> 已放回 $uuid（含 $n 个子 agent 分支）"
}

case "$1" in
    "")
        list
        exit 0
        ;;
    --all)
        for f in "$SRC"/*.jsonl; do
            [ -e "$f" ] || continue
            copy_one "$(basename "$f" .jsonl)"
        done
        ;;
    *)
        # 支持 uuid 前缀，省得手打 36 个字符。
        match=$(ls "$SRC"/"$1"*.jsonl 2>/dev/null | head -2)
        count=$(echo "$match" | grep -c . || true)
        if [ -z "$match" ]; then
            echo "!!! 没有匹配 '$1' 的会话快照。" >&2
            echo >&2
            list >&2
            exit 1
        fi
        if [ "$count" -gt 1 ]; then
            echo "!!! 前缀 '$1' 匹配到多个会话，请写长一点：" >&2
            echo "$match" >&2
            exit 1
        fi
        copy_one "$(basename "$match" .jsonl)"
        ;;
esac

# 记忆也一并放回（仓外记忆同样会被清空）。
if [ -d "$SRC/memory" ]; then
    mkdir -p "$DST/memory"
    cp -p "$SRC"/memory/*.md "$DST/memory/" 2>/dev/null || true
    [ -f "$SRC/MEMORY.md" ] && cp -p "$SRC/MEMORY.md" "$DST/MEMORY.md"
    echo ">>> 已放回仓外记忆副本"
fi

echo
echo "现在可以恢复了："
for f in "$DST"/*.jsonl; do
    [ -e "$f" ] || continue
    echo "    codebuddy --resume=$(basename "$f" .jsonl)"
done
echo
echo "不要用 codebuddy -c —— 它接的是最近一次会话，崩溃后往往是个空壳。"
