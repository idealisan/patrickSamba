#!/bin/sh
# save-history.sh —— 把 CodeBuddy 的会话历史快照进 git 仓库。
#
# 为什么需要它：
#   本项目的开发容器会不定期崩溃并清空 git 仓库以外的一切，
#   而 CodeBuddy 的会话历史默认只存在仓库外的 ~/.codebuddy 下。
#   更要命的是崩溃后 `codebuddy -c` / `--resume` 经常恢复不出东西：
#   `-c` 接的是「最近一次」会话，崩溃后新开的那个空会话就是最近的，
#   于是恢复出一个空壳，真正的历史还躺在上一个 uuid 里。
#   唯一可靠的办法是把原始 jsonl 存进 git。恢复步骤见 AGENTS.md §10。
#
# 用法：
#   scripts/save-history.sh              # 快照 + 提交 + 推送
#   scripts/save-history.sh --no-commit  # 只快照到工作树，不提交
#
# 注意：这是**开发脚本**，不是软件运行时依赖，不违反 AGENTS.md C3。
set -e

SRC=${CODEBUDDY_PROJECT_DIR:-/root/.codebuddy/projects/workspace}

cd "$(dirname "$0")/.."
REPO=$(pwd)
DST="$REPO/history"

if [ ! -d "$SRC" ]; then
    echo "!!! 找不到 CodeBuddy 会话目录: $SRC" >&2
    echo "    可以用 CODEBUDDY_PROJECT_DIR=<路径> 覆盖。" >&2
    exit 1
fi

mkdir -p "$DST"

# ------------------------------------------------------- 1. 拷贝 jsonl 与记忆
#
# 只取会话主线 jsonl、子 agent 分支 jsonl 与记忆 md。
# tool-results/ 下是大块的工具输出缓存，体量大且可从 jsonl 推出，不入库。
( cd "$SRC" && find . -type f \( -name '*.jsonl' -o -name '*.md' \) \
    -not -path './*/tool-results/*' ) | while read -r f; do
    mkdir -p "$DST/$(dirname "$f")"
    cp -p "$SRC/$f" "$DST/$f"
done

# --------------------------------------------------------------- 2. 生成索引
#
# 崩溃后要恢复，第一件事是判断「哪个 uuid 才是有内容的那个」。
# 索引里给出行数、大小、时间和首条用户消息，一眼就能挑出来。
INDEX="$DST/INDEX.md"
{
    echo "# CodeBuddy 会话历史索引"
    echo
    echo "> 本文件由 \`scripts/save-history.sh\` 自动生成，不要手工编辑。"
    echo "> 恢复方法见 [AGENTS.md](../AGENTS.md) §10。"
    echo
    echo "快照时间：$(date '+%Y-%m-%d %H:%M:%S %Z')"
    echo
    echo "| 会话 uuid | 行数 | 大小 | 最后修改 | 首条用户消息 |"
    echo "|---|---:|---:|---|---|"
    for f in "$DST"/*.jsonl; do
        [ -e "$f" ] || continue
        uuid=$(basename "$f" .jsonl)
        lines=$(wc -l < "$f" | tr -d ' ')
        size=$(du -h "$f" | cut -f1)
        mtime=$(date -r "$f" '+%Y-%m-%d %H:%M')
        # 首条用户消息：第一行里 "text":"..." 的内容，截断到 80 字符。
        first=$(head -1 "$f" | sed -n 's/.*"text":"\([^"]*\)".*/\1/p' | cut -c1-80)
        [ -n "$first" ] || first="(无法解析)"
        subs=$(ls "$DST/$uuid/subagents"/*.jsonl 2>/dev/null | wc -l | tr -d ' ')
        [ "$subs" = "0" ] || first="$first （含 $subs 个子 agent 分支）"
        echo "| \`$uuid\` | $lines | $size | $mtime | $first |"
    done
    echo
    echo '恢复某个会话：`codebuddy --resume=<uuid>`。'
    echo '若 resume 失效（崩溃后常见），直接把对应 jsonl 读给新会话看。'
} > "$INDEX"

echo ">>> 已快照到 $DST"
ls -la "$DST"/*.jsonl 2>/dev/null | awk '{print "    " $5 "\t" $9}'

# --------------------------------------------------------------- 3. 提交推送
if [ "$1" = "--no-commit" ]; then
    echo ">>> --no-commit，跳过提交"
    exit 0
fi

cd "$REPO"
git add -- history
if git diff --cached --quiet -- history; then
    echo ">>> 历史无变化，无需提交"
    exit 0
fi

# 与 save.sh 共用同一把锁，避免多 agent 同时 rebase/push 打架。
LOCK="$REPO/.git/stupidsamba-push.lock"
i=0
while ! mkdir "$LOCK" 2>/dev/null; do
    i=$((i + 1))
    if [ "$i" -gt 120 ]; then
        echo "!!! 等锁超时，可能有残留锁目录：$LOCK" >&2
        exit 1
    fi
    sleep 1
done
echo "$$" > "$LOCK/pid"
trap 'rm -rf "$LOCK"' EXIT INT TERM

git commit -q -m "history: 快照 CodeBuddy 会话历史（容器随时崩溃，仓外内容不可信）"
echo ">>> 已提交"
git pull --rebase -q
git push -q
echo ">>> 已推送"
