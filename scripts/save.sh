#!/bin/sh
# save.sh —— 多 agent 并行开发时的安全提交推送脚本。
#
# 用法：  scripts/save.sh "wire: 实现 SMB2 Header 编解码"
#
# 做的事：
#   1. 校验 CGO_ENABLED=0 能编译（AGENTS.md C1）
#   2. 只提交当前工作区改动
#   3. pull --rebase 后 push，失败自动重试（多 agent 并发推送会撞车）
#
# 注意：这是**开发脚本**，不是软件运行时依赖，不违反 AGENTS.md C3。
set -e

MSG="$1"
if [ -z "$MSG" ]; then
    echo "用法: scripts/save.sh \"<模块>: <说明>\"" >&2
    exit 1
fi

cd "$(dirname "$0")/.."
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

echo ">>> go build ./..."
go build ./...

if [ -z "$(git status --porcelain)" ]; then
    echo ">>> 无改动，跳过提交"
else
    git add -A
    git commit -q -m "$MSG"
    echo ">>> 已提交: $MSG"
fi

i=1
while [ "$i" -le 8 ]; do
    if git pull --rebase -q origin main && git push -q origin main; then
        echo ">>> 已推送 (第 $i 次尝试)"
        exit 0
    fi
    echo ">>> 推送失败，重试 $i/8 ..." >&2
    sleep $((i * 2))
    i=$((i + 1))
done

echo ">>> 推送失败，请手动处理冲突" >&2
exit 1
