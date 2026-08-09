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
# ⚠️ 退出码取的是**最近一条**记录（data[0]），而同一个 commit 会有 push 和
#    pull_request 两条、跑的是不同版本的 .cnb.yml，红绿可以相反（§10.3 第 10 条）。
#    合 PR 前请 `-n 5` 看一眼输出里的 `(event -> targetRef)`，认准
#    `pull_request` 那条，别拿 push 的假红卡自己。
#
# ⚠️ 本端点**没有 stage 级明细**，别在这里找「红在哪一关」，理由与替代做法
#    写在下面 jq 那段的注释里（2026-08-09 实测）。
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

# ---------------------------------------------------------------------------
# ⚠️ 这个端点给不了 stage 级明细，别指望在这里定位「红在哪一关」（2026-08-09 实测）
# ---------------------------------------------------------------------------
#
# 1) `pipelines[]` 里每个元素**只有 5 个字段**：id / createTime / duration /
#    labels / status。没有 stage 名、没有 job、没有日志摘要。
#    实测 `.data[0].pipelines[0] | keys` 就这 5 个，不是「有但为空」。
#
# 2) **`.stages` / `.jobs` 这两个键根本不存在**（不是空数组）。这个区别很要命：
#    jq 里 `null | length` **等于 0**，于是 `jq '.pipelines[0].stages | length'`
#    会安安静静地返回 0，读起来像「枚举过了，这条流水线有 0 个 stage」，
#    实际上是「压根没有这个字段」。又一例「成功回显 ≠ 事情真的发生」，
#    而且这次连报错都没有。要判断字段在不在，用 `has("stages")`，不要用 length。
#
# 3) 下面这行打印的三个数字是 **pipeline 数，不是 stage 数**。字段名
#    `pipelineTotalCount` 已经把话说明白了，是本脚本原先的标签写成「stage」
#    误导了读者 —— 现已改正。典型误读：看到 `0/1/1` 就以为
#    「整条流水线只有一关，那一关挂了」，进而去猜是哪一关；真相是
#    `.cnb.yml` 里那条流水线**整体**算一个 pipeline，里面有多少 stage 这个
#    接口一个字都不会说。用错误的分母去推断，越推越远。
#
# 那红因到底怎么定位（按代价从低到高）：
#   a. **相邻 commit 对照**：同分支查 `-n 5`，找出第一个变红的 sha，
#      `git show <sha>` 看那一个提交改了什么。这是最省事也最准的一招。
#   b. **本地同 commit 复跑**：临时 worktree 检出那个 sha
#      （`git worktree add --detach /tmp/ci-<sha> <sha>`，用完留着别删，见 §7.5），
#      在里面跑 `sh test/ci/check-test-compile.sh` 等各关，本地就能复现。
#   c. **时长旁证**：`duration` 是唯一能免费拿到的进度信号。本仓库正常一轮
#      约 100~130 秒；十几秒就结束的多半是**早期 stage 就崩了**（编译/vet），
#      跑满两分钟才红的一般是后面的测试关。它只能缩小范围，不能定案。
#   d. 上面都不够时，打开 `buildLogUrl` 用人眼看 —— 这是唯一有 stage 明细的地方，
#      但它是网页不是 API，脚本取不到。
#
# 顺带：这里打印 `event`（push / pull_request）不是凑热闹。AGENTS.md §10.3 第 10 条
# 记过一个真实的坑：**两种事件跑的是不同版本的 `.cnb.yml`，同一个 commit 可以
# push 红、PR 绿**。不显示事件类型，就会拿 push 的「假红」去卡一个其实能合的 PR。
printf '%s' "$RESP" | jq -r --arg b "$BRANCH" '
    .data[] |
    "[\(.status | ascii_upcase)] \($b)  \(.sha[0:8])  (\(.event) -> \(.targetRef))  \(.commitTitle)\n" +
    "        pipeline 成功/失败/总数 = \(.pipelineSuccessCount)/\(.pipelineFailCount)/\(.pipelineTotalCount)   耗时 \(.duration/1000)s\n" +
    "        \(.buildLogUrl)"'

STATUS=$(printf '%s' "$RESP" | jq -r '.data[0].status')
case "$STATUS" in
    success) exit 0 ;;
    pending) exit 2 ;;
    *)       exit 1 ;;
esac
