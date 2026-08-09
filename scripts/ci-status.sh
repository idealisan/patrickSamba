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
#   3 = 该分支从来没有构建记录（真的没跑，该等 / 该查为什么没触发）
#   4 = 调用失败（token/网络/权限）
#   5 = 按 .cnb.yml 的 ifModify，本次变更（纯文档）**设计上就不跑 CI**。两种表现形式
#       都归到这里：① 有记录但一个 stage 都没执行（实测的常见形态，status 也是
#       success，耗时 0.3 秒）；② 压根没有记录。**不用等，直接走人工评审合并。**
#       刻意选了个非 0 的码：跳过 ≠ 通过，别让 `ci-status.sh && merge` 把
#       「一关没跑」误当成「全关通过」——本仓库在「静默变绿」上栽过不止一次。
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

# ── 文档类路径判定 ───────────────────────────────────────────────────────────
# ⚠ 这份列表必须与 **.cnb.yml 里 push / pull_request 的 ifModify 排除项**逐条对应，
#   改一处必须同时改另一处（.cnb.yml 那边也写了指回这里的提醒）。
#     "!(**/*.md)"    → 任何以 .md 结尾的文件
#     "!(docs/**)"    → docs/ 下的一切
#     "!(memory/**)"  → memory/ 下的一切
#     "!(history/**)" → history/ 下的一切
#
# 为什么脚本要跟着 .cnb.yml 走：加了 ifModify 之后，纯文档的分支/PR **根本不会产生
# 任何流水线记录**。若这里不区分，调用者只会看到 rc=3「没有构建记录」，而团队纪律是
# 「CI 绿才能合」，于是有人会对着一个**永远不会出现**的流水线无限期等待 ——
# 那只是把浪费的机器时间换成了浪费的人的时间，比原来更糟。
DOC_ONLY_EXCLUDES='*.md, docs/**, memory/**, history/**'
is_doc_path() {
    case "$1" in
        *.md|docs/*|memory/*|history/*) return 0 ;;
        *)                              return 1 ;;
    esac
}

# 判断某分支相对 main 的全部变更是否都是文档。
# 返回：0 = 纯文档   1 = 含非文档变更   2 = 无法判断（本地缺 ref / 没有差异）
# 用 merge-base 三点语义（等价于 git diff origin/main...<branch>），与 PR 的
# 变更集口径一致；不用 HEAD~1，因为那是单次 push 的口径，回答不了「这个 PR 会不会跑 CI」。
branch_is_doc_only() {
    _ref=''
    for _cand in "origin/$1" "$1"; do
        if git rev-parse --verify --quiet "$_cand^{commit}" >/dev/null 2>&1; then
            _ref=$_cand
            break
        fi
    done
    [ -n "$_ref" ] || return 2
    git rev-parse --verify --quiet 'origin/main^{commit}' >/dev/null 2>&1 || return 2
    _base=$(git merge-base origin/main "$_ref" 2>/dev/null) || return 2
    _files=$(git diff --name-only "$_base" "$_ref" 2>/dev/null) || return 2
    [ -n "$_files" ] || return 2

    _verdict=0
    # 用 while read 而不是 `for f in $_files`：后者按空白切词，带空格的文件名会被
    # 拆成两个"不存在的路径"，判定结果随之出错。
    while IFS= read -r _f; do
        [ -n "$_f" ] || continue
        if ! is_doc_path "$_f"; then
            _verdict=1
            break
        fi
    done <<EOF
$_files
EOF
    return "$_verdict"
}

BRANCH=""
LIMIT=1
while [ "$#" -gt 0 ]; do
    case "$1" in
        -n) LIMIT=$2; shift 2 ;;
        -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
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
    DOC=0
    # 注意用 `|| DOC=$?` 而不是 `; DOC=$?`：本脚本开了 set -e，
    # 后者会让"返回 1（含代码变更）"这个**正常结论**直接把脚本打死。
    branch_is_doc_only "$BRANCH" || DOC=$?
    if [ "$DOC" = "0" ]; then
        echo "$SLUG @ $BRANCH: 没有构建记录，而且**设计上就不会有** —— 不用等 CI。"
        echo "        本分支相对 main 的变更全部是文档（$DOC_ONLY_EXCLUDES），"
        echo "        .cnb.yml 的 push / pull_request 已用 ifModify 把这类变更排除在触发条件外。"
        echo "        → 直接走人工评审合并即可；等待是等不到东西的。"
        exit 5
    fi
    echo "$SLUG @ $BRANCH: 没有任何构建记录（分支没推上去？或流水线未触发）"
    if [ "$DOC" = "2" ]; then
        echo "        （本地缺 origin/main 或该分支的引用，无法判断是不是"
        echo "          「纯文档变更、按 .cnb.yml 的 ifModify 设计上跳过」；先 git fetch 再跑一次。）"
    fi
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

# ── 被 ifModify 跳过的流水线长什么样（实测，不是推测）────────────────────────
# 纯文档变更**照样会产生一条构建记录**，而且 status 就是 `success` —— 一开始以为
# 它压根不建记录（那样只会 rc=3），实测不是。两者的区别只在计数与耗时上：
#     跳过：success，pipelineSuccessCount=0，failCount=0，duration≈345ms
#     真跑：success，pipelineSuccessCount=1，failCount=0，duration≈120000ms
# （证据：ci/skip-docs-only 上 57f0bd97 是前者，6fbcff4a / e1446467 是后者。）
#
# 这是本仓库最熟悉的那种坑的又一个形态：**一条 pipeline 都没执行，看板照样是绿的**。
# （用词跟上面那段保持一致：这个接口给的是 **pipeline 计数**，不是 stage 计数。）
# 若不特判，`ci-status.sh && merge` 这类用法会把「什么都没验」当成「验过了」；
# 更糟的是将来谁把排除列表写错成把代码目录也排掉，每次推送都是 0.3 秒的绿，
# 没有任何地方会提示。所以这里单独给一个退出码，让它**不等于 0**。
# 判据用计数而不是耗时阈值：真跑成功时 successCount 必 ≥1，不会因为机器快慢误判。
if [ "$STATUS" = "success" ]; then
    OK_N=$(printf '%s' "$RESP" | jq -r '.data[0].pipelineSuccessCount // 0')
    BAD_N=$(printf '%s' "$RESP" | jq -r '.data[0].pipelineFailCount // 0')
    if [ "$OK_N" = "0" ] && [ "$BAD_N" = "0" ]; then
        echo
        echo "注意：这条记录**一条 pipeline 都没执行**（成功 0 / 失败 0），不是「跑过并通过」。"
        echo "      按 .cnb.yml 的 ifModify，本次变更全是文档（$DOC_ONLY_EXCLUDES）"
        echo "      所以整条流水线被跳过 —— 这是设计如此，**不用等，也没什么可等的**。"
        echo "      → 文档类改动直接走人工评审合并；若你以为这里该跑门禁，那说明"
        echo "        变更里混进了本该排除的文件，或 .cnb.yml 的排除列表写错了，去查那边。"
        exit 5
    fi
fi

case "$STATUS" in
    success) exit 0 ;;
    pending) exit 2 ;;
    *)       exit 1 ;;
esac
