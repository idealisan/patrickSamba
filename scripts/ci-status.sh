#!/bin/sh
# ci-status.sh —— 查某个分支最近一次流水线的状态，合 PR 前先跑它。
#
# 用法：
#   scripts/ci-status.sh                      # 当前分支
#   scripts/ci-status.sh win-vfs/wire-validate
#   scripts/ci-status.sh main -n 5            # 看该分支最近 5 次
#
# 退出码（可直接用在 && / if 里）：
#   0 = success（绿，可以合）
#   1 = error / cancel（红，别合）
#   2 = pending（还在跑，等）
#   3 = 该分支从来没有构建记录
#   4 = 调用失败（token/网络/权限）
#
# 为什么需要这个：本项目的 CI 曾经**从建项目起就没真正跑完过**——
# .cnb.yml 里 `CGO_ENABLED=0 go test -race` 因为 race detector 需要 cgo，
# 0.1 秒退出码 2，而它是第一个 stage，后面 4 个 stage 全被跳过，
# 没人发现。「有 CI」不等于「CI 在跑」，必须能主动查。
#
# 注意：这是**开发脚本**，不是软件运行时依赖，不违反 AGENTS.md C3。

set -e

BRANCH=""
LIMIT=1
while [ "$#" -gt 0 ]; do
    case "$1" in
        -n) LIMIT=$2; shift 2 ;;
        -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
        *) BRANCH=$1; shift ;;
    esac
done

if [ -z "$CNB_TOKEN" ]; then
    echo "CNB_TOKEN 未设置" >&2
    exit 4
fi

cd "$(dirname "$0")/.."
[ -n "$BRANCH" ] || BRANCH=$(git rev-parse --abbrev-ref HEAD)

# slug 从 remote 推导，别写死：worktree/fork 场景下写死会查错仓库。
SLUG=$(git remote get-url origin | sed -e 's#^https\{0,1\}://[^/]*/##' -e 's#^git@[^:]*:##' -e 's#\.git$##')

# Accept 头：本 GET 端点实测不带也返回 200，但 CNB 的部分接口（如 PR 的
# GET/PUT）不带 Accept: application/json 会直接 406，报错文案还说的是
# content type，极易误判。统一都带上，省得下次踩。
RESP=$(curl -sS --fail-with-body \
    -H "Authorization: Bearer $CNB_TOKEN" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json" \
    "https://api.cnb.cool/$SLUG/-/build/logs?sourceRef=$BRANCH&page_size=$LIMIT" 2>&1) || {
    echo "调用 CNB API 失败：$RESP" >&2
    exit 4
}

COUNT=$(printf '%s' "$RESP" | jq -r '.data | length')
if [ "$COUNT" = "0" ] || [ "$COUNT" = "null" ]; then
    echo "$SLUG @ $BRANCH: 没有任何构建记录（分支没推上去？或流水线未触发）"
    exit 3
fi

printf '%s' "$RESP" | jq -r --arg b "$BRANCH" '
    .data[] |
    "[\(.status | ascii_upcase)] \($b)  \(.sha[0:8])  \(.commitTitle)\n" +
    "        stage 成功/失败/总数 = \(.pipelineSuccessCount)/\(.pipelineFailCount)/\(.pipelineTotalCount)   耗时 \(.duration/1000)s\n" +
    "        \(.buildLogUrl)"'

STATUS=$(printf '%s' "$RESP" | jq -r '.data[0].status')
case "$STATUS" in
    success) exit 0 ;;
    pending) exit 2 ;;
    *)       exit 1 ;;
esac
