#!/bin/sh
# portable-mode.sh —— 让 `filesystem_mode: portable` 这条路**在 CI 里真的被执行一遍**。
#
# 为什么必须有这一关
# ------------------
# AGENTS.md §1.2 原文：「`portable` 模式必须在 CI 里真跑一遍，不能只是配置项里
# 多一个取值……**没有 CI 覆盖的 builtin 就是一份薛定谔的实现**，写了等于没写。」
#
# 理由不是形式主义：builtin 是「将来移植到未知系统」（嵌入式、只读根文件系统、
# 我们没见过的 NAS 固件）的唯一底座。一条在 CI 里从未被执行过的路径，到需要它的
# 那天一定是坏的 —— 而那时既没有原始作者在场，也没有可对照的正确行为。
# 本仓库已有同型前科：挂在特定 build tag 下的代码默认 CI 一行都不会编译执行，
# 直到有人专门补一关才被看见（见 test/ci/check-test-compile.sh 头部注释）。
#
# 本脚本的四关
# ------------
#   1. 必需用例断言：逐条断言关键 portable 用例存在（缺任意一条即红），
#      且 portable 用例总数不少于下界（抓「整批没了」）。
#   2. 真跑：执行这些用例，核对「跑了几条 == 有几条」，且**任何子测试都
#      不许 SKIP**（父测试在子测试被跳过时仍报 PASS，所以子测试 SKIP 行
#      必须连缩进的一起数），再核对含子测试的结果行总数不低于下界
#      （连 SKIP 都不留的「子测试整体没执行」只有这个数抓得到）。
#   3. builtin 适配器：包必须存在、且**不许零测试**，然后全量跑一遍。
#   4. 反向对照：注入 4 个变异体（A/B/C/D，各对一道判据），断言本门禁**确实会红、且红在预期的那道判据上**
#      —— 上面每一道判据都有一个对应的变异体，没有哪条是「从来没红过」的。
#
# 用法：
#   sh test/ci/portable-mode.sh
# 退出码：0 = 通过；非 0 = 失败（失败时会打印是哪一关挂的）。
set -e

REPO=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$REPO"

command -v go >/dev/null 2>&1 || PATH=$PATH:/usr/local/go/bin
export PATH
export CGO_ENABLED=0

# ---------------------------------------------------------------- 可调参数
#
# ⚠️ 下面两个是**下界**，不是精确值：新增用例时请**上调**。
# 每个数字都必须能说出出处，不许拍脑袋。
#
# 两个数字的性质不一样，改法也不一样，别混着用：
#
#   MIN_PORTABLE_RESULTS=50 —— **棘轮（ratchet）**。它等于当下实测值，
#       也就是说任何人删掉一条子测试都会判红。**这是故意的，不是巧合**：
#       这道门禁存在的全部理由就是防退化，而退化最常见的形态恰恰是
#       「删一条不起眼的用例，数字降一点，没人注意」。
#
#   MIN_BUILTIN_TESTS=6 —— **语义下限**，刻意远低于实测的 34（理由见该行注释）。
#       它只拦「一整包产品代码配 0 条测试」，不参与防退化。
#
# 合法调低棘轮的**唯一**方式（两步缺一不可）：
#   1. 在 PR 描述里写清**删了哪条、为什么不再需要**（不是「重构了」三个字）；
#   2. **同一个 PR** 里改这个数字，并在提交信息写明新数字的出处（怎么数出来的）。
#
# ❌ 明确禁止：**跑红了就把数字调小**。那正是这道门禁要防的那件事本身，
#    把它做进门禁的修法里等于当场废掉门禁。红了先去看是谁删了什么。

# 【已删除 MIN_PORTABLE_CASES —— 它是一道永远不可能触发的判据，别再加回来】
#
# 曾经写作 `MIN_PORTABLE_CASES=12`，与 REQUIRED_PORTABLE_CASES 的清单长度相等，
# 定位是「兜底『整批没了』」。但这个数**在数学上永远不会红**：
# 必需用例是从同一份 `-list` 枚举结果里挑出来的，是它的子集；
# 只要上面那 12 条点名断言全过，枚举总数必然 ≥ 12，下界判定必然放行。
# 也就是说这行 `if` 的两个分支里，红的那个分支**不可达**。
#
# 实测坐实（2026-08-09 17:33，overlay 变异：把 12 条之外的 portable 用例全改名）：
#   枚举 12 条 → 总数下界 12：**放行**
#   结果行 18 条 → MIN_PORTABLE_RESULTS 50：**判红**
# 兜底「整批没了」这件事，从头到尾是下面那个结果行下界在做，与这个数无关。
#
# 取 35 也不对，但错在另一头：它会因为一次合理的表驱动重构而误报，
# 而误报会教会大家忽略这道门禁 —— 自废。而结果行下界天然扛得住这种重构
# （34 个顶层函数合并成 1 个带 34 个子测试的表驱动，结果行数基本不变）。
# 所以正确答案不是把 12 调成某个数，而是**不要这个数**：
# 结构性退化交给点名断言，数量退化交给结果行下界，各有各的变异体做反向对照。
#
# 留着一个不可达的 `if` 比没有更糟：它让读者以为「整批没了」已经有人管，
# 于是不会再去问那件事到底谁管 —— 本仓库把这种形态记在案，叫「被架空的逻辑」。

# portable 用例执行后**所有结果行**（顶层 + 各级子测试）的条数下界。
# 出处：同上一次实测，`-v` 输出里 `--- PASS/FAIL/SKIP` 行共 50 条 = 35 顶层 + 15 子测试。
#
# 为什么光有上面那个 35 不够（这一条是 2026-08-09 由 team-lead 提出、本脚本作者用变异体证实的）：
# `-list` 只看得见**顶层函数名**，`^--- PASS` 也只数顶格的顶层结果行。于是
# 「顶层函数还在、还报 PASS，但它内部的子测试一条都没真跑」这种坏法完全隐形。
# 实测变异体 D（把 `t.Run(...)` 换成一个立即调用但什么都不做的函数字面量）：
# 六项能力往返一次都没执行，顶层 PASS 仍是 35、枚举仍是 35、SKIP 仍是 0 —— 现行判据全绿，
# 而结果行从 50 掉到 44。所以真正扛事的是这个数，上面那个 35 只拦「整个函数没了」。
MIN_PORTABLE_RESULTS=50

# builtin 包自身的测试条数下界。
# 这里**刻意不限定用例命名**（命名是 builtin 作者的自由），只拦「一整包产品代码
# 配 0 条测试」这件事。取 6 的依据是语义而不是当前实测值：builtin 要兑现六项能力，
# 每项至少得有一条往返用例。实测 main e4f0f80 上有 34 条，余量充足 ——
# 刻意不把下界顶到 34，那会让任何一次用例合并重构都误报。
MIN_BUILTIN_TESTS=6

# 匹配 portable 语义用例的前缀。改这个之前先跟 internal/oscap 的负责人对齐，
# 否则门禁会数不到别人的用例，然后以「空转」的名义把好人判红。
PORTABLE_RE='TestPortable'

# 必需 portable 用例清单：每条都在守护一个「掉了就有一类语义彻底失守」的方向。
#
# 这里**不**用裸计数（"至少 35 条"），因为裸计数只告诉你少了几条、不告诉你
# 少了哪条。有人删掉 TestPortableIncompleteBuiltinIsRejected、再补两条 xattr
# 边界用例，计数从 35 变 37，门禁全绿，而 AGENTS.md §1.2「builtin 必须完整」
# 的强制执行已经没了 —— 这正是本仓库反复栽的形状：指标还在动，被守护的东西
# 已经没了。所以逐条断言存在，缺任意一条即红，并打印缺的是哪条、它守的是什么。
#
# 清单来源：team-lead 按「掉了它就有一类语义彻底失守」筛的 12 条。
# 每条后面的注释是这份清单唯一的防腐剂：没有理由的名字清单，三个月后会被当成
# 过时残留删掉。新增必需用例时，请连它守的语义一起写进注释。
# 格式：每行 `名字|它守的语义`，解析时按 `|` 拆分。
REQUIRED_PORTABLE_CASES='
TestPortableXattrRoundTrip|六项能力① xattr 命名流/扩展属性往返
TestPortablePunchHoleObservableSemantics|六项能力② 稀疏区间可观测语义（打洞后读得到空洞）
TestPortableNamedStreamRoundTrip|六项能力③ 命名流/ADS 往返
TestPortableFileIDStableUniqueAndSurvivesRename|六项能力④ 稳定且唯一、改名后仍持有的 FileID
TestPortableCreationTimeRoundTrip|六项能力⑤ 真实创建时间往返
TestPortableDOSAttributesRoundTrip|六项能力⑥ DOS 属性往返
TestPortableIgnoresNativeEvenWhenAvailable|portable 不许碰 native factory（含零调用计数）—— 装配语义核心
TestPortableIncompleteBuiltinIsRejected|§1.2「builtin 不许赊账」的强制执行：缺一项能力就硬失败
TestPortableNilBuiltinFactoryIsRejected|builtin==nil 是独立 return 点，上一条覆盖不到
TestPortableReadOnlyRejectsEveryWrite|只读共享的拒绝路径（不是「不 panic」）
TestPortableDefaultMetadataPathOutsideRoot|库文件不许落进共享根，否则客户端能看见
TestPortableFileIDReadOnlyWithoutInodeIsHonest|不发出会变的号，宁可 ErrNotSupported
'

BUILTIN_DIR=internal/oscap/builtin
BUILTIN_PKG=./internal/oscap/builtin/...

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/ss-portable-gate.XXXXXX")
cleanup() { rm -rf "$SCRATCH"; }
trap cleanup EXIT INT TERM

say() { printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"; }
die() { printf '\033[1;31m门禁失败: %s\033[0m\n' "$1" >&2; exit 1; }

# ================================================================ 1. 必需用例断言
#
# 这一关排在最前面，是因为它抓的是**最隐蔽**的一种失败：什么都没跑，却全绿。
#
# `go test -run TestPortable` 在**一条都没匹配上**时同样打印 `ok` 并退出 0
# （只在末尾附一句 no tests to run）。也就是说用例被改名、被删掉、
# 或者整个包被搬走之后，这一关会安静地变成空转，而流水线一片绿。
# 这正是本仓库反复栽过的「成功回显 ≠ 事情真的发生」。
# 所以：先数 + 逐条点名，数不够 / 缺关键条 直接红，再谈跑。
say "1. 必需用例断言：每条关键 portable 用例都必须存在"

LIST_LOG="$SCRATCH/list.txt"
# -list 只列举不执行，但它**会完整编译测试二进制**，所以编译错误在这一步就会暴露。
if ! go test -list "$PORTABLE_RE" ./internal/oscap/... > "$LIST_LOG" 2>&1; then
    cat "$LIST_LOG" >&2
    die "枚举 portable 用例失败（多半是 ./internal/oscap/... 编译不过），见上方输出。"
fi

FOUND=$(grep -c "^$PORTABLE_RE" "$LIST_LOG" || true)
FOUND=${FOUND:-0}
grep "^$PORTABLE_RE" "$LIST_LOG" | sed 's/^/  - /' || true
echo "  共枚举到 $FOUND 条（这个数只报给人看，不作判据 —— 理由见顶部 MIN_PORTABLE_CASES 那段）"

# 按名字逐条断言。刻意不用「数量不够」这种判法：缺了关键用例时应当直接说
# 「缺的是哪一条、它守的是什么」，而不是甩一个数字让人自己去找。
MISSING=''
while IFS='|' read -r _name _why; do
    [ -z "$_name" ] && continue
    if grep -q "^$_name\$" "$LIST_LOG"; then
        echo "  [有] $_name"
    else
        printf '  \033[1;31m[缺] %s\033[0m —— 它守的是：%s\n' "$_name" "$_why" >&2
        MISSING="$MISSING $_name"
    fi
done <<PORTABLE_LIST
$REQUIRED_PORTABLE_CASES
PORTABLE_LIST
if [ -n "$MISSING" ]; then
    echo "  实际枚举输出：" >&2
    sed 's/^/    /' "$LIST_LOG" >&2
    die "缺少必需的 portable 用例：$MISSING
       每条都有独立守护的语义（见上方 [缺] 行后的注释），删掉任意一条都意味着
       某类 portable 正确性不再被任何测试覆盖 —— 这正是本门禁要拦的
       『指标还在动、被守护的东西没了』。如果是**改名**，请同步更新
       REQUIRED_PORTABLE_CASES；如果是**删除**，请先说清楚 portable 的那个
       承诺改由谁来守，不要悄悄拿别的用例把数量填回去。
       另有一种非代码原因：**分支落后于 main**。builtin 适配器（PR #129）
       2026-08-09 才合入 main e4f0f80，在它之前的基线上 ./internal/oscap/...
       只有 port 层那一条用例，这里会一次缺 11 条。先 git pull --rebase origin main。"
fi
echo "  必需用例齐备（$(printf '%s' "$REQUIRED_PORTABLE_CASES" | grep -c .) 条）"

# ================================================================ 2. 真跑
#
# 上一关只证明用例**存在**，这一关才让它们真的跑。
#
# ⚠️ 这一关的判据被返工过一次，返工的理由值得留在这里：
# 最初只核对「顶层跑了几条 == 枚举到几条」+「顶层 SKIP 为 0」。看着严密，实测有洞 ——
# `go test` 的 `-v` 输出里，**子测试的结果行是缩进的**（`    --- SKIP: TestX/sub`），
# 而顶层的顶格。用 `^--- SKIP:` 去数，子测试的 skip 一条都数不到；
# 更要命的是**父测试在子测试全被 skip 时仍然报 `--- PASS`**（Go 不向上传播 skip）。
# 于是「六项能力往返全 skip」这种坏法下，三个数字仍然是 35/35/0，门禁全绿。
# 2026-08-09 用变异体 C 当场复现过，不是推演。
# 所以现在数三样：顶层通过数、**任意层级**的 SKIP 数、**所有层级**的结果行总数。
say "2. 真跑 portable 用例"

RUN_LOG="$SCRATCH/run.txt"
RC=0
# 刻意不用 `go test ... | tee`：管道的退出码是 tee 的，测试失败会被吞掉，
# 而 /bin/sh 没有 PIPESTATUS 可补救。先落文件再 cat，退出码才是 go test 自己的。
go test -count=1 -run "$PORTABLE_RE" -v ./internal/oscap/... > "$RUN_LOG" 2>&1 || RC=$?
cat "$RUN_LOG"
[ "$RC" -eq 0 ] || die "portable 用例失败（go test 退出码 $RC），见上方输出。"

# 顶层用例的结果行顶格；子测试的结果行带缩进。三个数字各管一件事，别互相替代：
#   PASSED   顶格的 PASS  —— 与枚举数对账，管「顶层函数有没有悄悄少跑」
#   SKIPPED  任意缩进的 SKIP —— 管「用例还在、但被 skip 掉了」，含子测试
#   RESULTS  任意缩进的 PASS/FAIL/SKIP 总数 —— 管「子测试整体消失、父测试照样 PASS」
PASSED=$(grep -c '^--- PASS: ' "$RUN_LOG" || true)
SKIPPED=$(grep -cE '^[[:space:]]*--- SKIP: ' "$RUN_LOG" || true)
RESULTS=$(grep -cE '^[[:space:]]*--- (PASS|FAIL|SKIP): ' "$RUN_LOG" || true)
PASSED=${PASSED:-0}
SKIPPED=${SKIPPED:-0}
RESULTS=${RESULTS:-0}
echo "  顶层通过 $PASSED 条（枚举到 $FOUND 条），各级 SKIP $SKIPPED 条，"
echo "  含子测试的结果行共 $RESULTS 条（下界 $MIN_PORTABLE_RESULTS）"

[ "$SKIPPED" -eq 0 ] || {
    grep -nE '^[[:space:]]*--- SKIP: ' "$RUN_LOG" | sed 's/^/    /' >&2
    die "有 $SKIPPED 条 portable 用例/子测试被 SKIP 了（上方列出）。
       一条被跳过的用例和不存在是等价的 —— 它不会在任何时候告诉你 portable 坏了。
       子测试尤其危险：父测试照样报 PASS，顶层三个数字全都对得上。
       要么让它真跑，要么删掉并同步下调下界，不要留着装点门面。"
}

[ "$PASSED" -eq "$FOUND" ] || die "枚举到 $FOUND 条，却只跑了 $PASSED 条。
       两个数字对不上说明有用例既没通过也没被跳过（构建约束？包被过滤掉了？），
       这种悄悄少跑正是本门禁存在的理由。"

[ "$RESULTS" -ge "$MIN_PORTABLE_RESULTS" ] || die "含子测试的结果行只有 $RESULTS 条，少于下界 $MIN_PORTABLE_RESULTS。
       顶层数字（$PASSED/$FOUND）可能完全正常 —— 这一关抓的正是那种情况：
       顶层函数还在、还报 PASS，但它内部的 t.Run 子测试一条都没被执行
       （循环的集合空了？t.Run 被改写了？断言被搬走了？）。
       六项能力的往返断言全在子测试里，这个数掉下来就等于 portable 没被真正验证过。
       如果是有意重构（子测试合并/改成表驱动导致条数减少），请同步调整
       MIN_PORTABLE_RESULTS 并在提交信息里写清新数字的出处。"

# ================================================================ 3. builtin 适配器
#
# portable 的承诺是「六项能力全部由 builtin 提供」。上面两关验的是**装配语义**
# （矩阵指向 builtin、native factory 一次都不许被调），这一关验的是
# **被指向的那份实现本身**能不能跑。
say "3. builtin 适配器实测"

# ⚠️ 这里**故意没有** `if [ ! -d ... ]; then echo SKIP; exit 0; fi`。
# 那种写法看着体贴，实际是本仓库栽过最多的一类坑：一条永远走 skip 分支的门禁，
# 和没有门禁是一回事，而且更糟 —— 它会在报表上冒充一个绿点。
# builtin 尚未合入时本门禁**就该是红的**，红色本身就是「合并顺序还没到」的通知。
[ -d "$BUILTIN_DIR" ] || die "$BUILTIN_DIR 不存在。
       builtin 适配器（PR #129）已于 2026-08-09 合入 main e4f0f80，所以在最新主干上
       这个目录**应该**是存在的。看到这条消息只有两种可能：分支基线早于那次合并，
       或者目录被改名/删除了 —— 请当场查，不要顺手把这一关注释掉。"

BLIST_LOG="$SCRATCH/builtin-list.txt"
if ! go test -list '.*' "$BUILTIN_PKG" > "$BLIST_LOG" 2>&1; then
    cat "$BLIST_LOG" >&2
    die "枚举 builtin 用例失败（多半是 $BUILTIN_PKG 编译不过），见上方输出。"
fi

# `go test` 对一个没有任何 _test.go 的包打印 `?  <pkg>  [no test files]` 并**退出 0**。
# 于是「一整包产品代码 + 0 条测试」会一路绿灯 —— 那就是薛定谔的 builtin。
BFOUND=$(grep -c '^Test' "$BLIST_LOG" || true)
BFOUND=${BFOUND:-0}
echo "  builtin 包内枚举到 $BFOUND 条测试（下界 $MIN_BUILTIN_TESTS）"

if [ "$BFOUND" -lt "$MIN_BUILTIN_TESTS" ]; then
    sed 's/^/    /' "$BLIST_LOG" >&2
    die "builtin 包只有 $BFOUND 条测试，少于下界 $MIN_BUILTIN_TESTS。
       go test 对零测试的包打印 no test files 并退出 0，所以不数就发现不了。
       builtin 是移植到未知系统时的唯一底座，一行没跑过的底座等于没有。"
fi

go test -count=1 "$BUILTIN_PKG"

# ================================================================ 4. 反向对照
#
# **一个从来没红过的门禁，和没有门禁是一回事。** 所以这里当场证明它有牙：
# 注入变异体，再把**整个门禁**（就是本脚本自己）重跑一遍，断言它非零退出。
#
# 变异用 `go test -overlay` 注入 —— 替换内容只存在于临时目录与内存映射里，
# **工作树全程零修改**。这也是 AGENTS.md §7.5 要求的做法：验证不许靠
# 「改一下再改回来」，那需要 git restore（HIGH 风险形态，会卡死 TUI）。
#
# 递归保护：子进程带着 SS_PORTABLE_GATE_INNER=1，跑到这里就返回，不会无限套娃。
if [ -n "${SS_PORTABLE_GATE_INNER:-}" ]; then
    echo "（内层运行，跳过反向对照）"
    exit 0
fi

say "4. 反向对照：变异体必须让本门禁变红"

# 门禁脚本自己不认识 -overlay，用 GOFLAGS 透传（go 对所有子命令都读 GOFLAGS）。
# 注意保留调用方已有的 GOFLAGS（devenv.sh 会设 -p=1 削峰），别整个冲掉。
BASE_GOFLAGS=${GOFLAGS:-}

# assert_red <一句话说明> <overlay json 路径> <期望的红因正则>
#
# 第三个参数不是装饰：**「红了」和「因为对的理由红了」是两件事**。
# 变异体注入的是测试代码，一个手滑的 sed 完全可能让包编译不过 —— 那时门禁照样非零退出，
# 反向对照照样报 OK，而它声称验证的那道判据其实一次都没被触发。
# 这正是本仓库反复栽的「成功回显 ≠ 事情真的发生」在**反向对照**上的变体，
# 所以这里额外核对红因文本。
assert_red() {
    _what=$1
    _json=$2
    _why=$3
    _log="$SCRATCH/neg.log"
    _rc=0
    SS_PORTABLE_GATE_INNER=1 GOFLAGS="$BASE_GOFLAGS -overlay=$_json" \
        sh "$REPO/test/ci/portable-mode.sh" > "$_log" 2>&1 || _rc=$?
    if [ "$_rc" -eq 0 ]; then
        echo "  变异体下门禁的完整输出：" >&2
        sed 's/^/    /' "$_log" >&2
        die "反向对照失败 —— $_what，本门禁却退出 0（绿）。
       也就是说这道门禁抓不到它本该抓的东西，现在它只是个装饰品。"
    fi
    if ! grep -qE "$_why" "$_log"; then
        echo "  变异体下门禁的完整输出：" >&2
        sed 's/^/    /' "$_log" >&2
        die "反向对照走样 —— $_what 确实让门禁红了（rc=$_rc），但红因不是预期的
       /$_why/。多半是变异体把代码改到编译不过，于是它撞的是编译错误而不是
       本该被触发的那道判据。这种「红对了结果、错了原因」的反向对照等于没做。"
    fi
    printf '  \033[1;32m[OK]\033[0m %s -> 门禁如期变红 (rc=%s)\n' "$_what" "$_rc"
    printf '       红在: %s\n' \
        "$(grep -m1 -e '门禁失败:' -e '^--- FAIL:' "$_log" | sed 's/\[[0-9;]*m//g' || echo '(见日志)')"
}

# 找出所有声明了 ${PORTABLE_RE}* 的测试文件，供变异体 B/C/D 复用。
# 用 grep 现找而不是写死路径：文件搬家时应当当场报错，而不是变异体悄悄注入不进去。
PORTABLE_TEST_FILES=$(grep -rl "^func $PORTABLE_RE" internal/oscap --include='*_test.go' || true)
[ -n "$PORTABLE_TEST_FILES" ] || die "找不到任何声明 ${PORTABLE_RE}* 的 _test.go 文件，
       无法构造变异体。这与第 1 关数到 $FOUND 条自相矛盾，请当场查。"

# mutate_test_files <变异体名> <sed 表达式> <输出 json 路径>
# 对上面每个文件套用同一条 sed，任何一个文件没被改到就判红（注入不进去的变异体
# 会让反向对照悄悄失效：它跑的其实是原版代码，然后报「通过」）。
mutate_test_files() {
    _tag=$1
    _sed=$2
    _out=$3
    _hit=0
    printf '{"Replace":{' > "$_out"
    _sep=''
    for _f in $PORTABLE_TEST_FILES; do
        _dst="$SCRATCH/$_tag-$(echo "$_f" | tr '/.' '__').go"
        sed "$_sed" "$_f" > "$_dst"
        cmp -s "$_f" "$_dst" || _hit=$((_hit + 1))
        printf '%s"%s":"%s"' "$_sep" "$REPO/$_f" "$_dst" >> "$_out"
        _sep=','
    done
    printf '}}\n' >> "$_out"
    [ "$_hit" -gt 0 ] || die "变异体 $_tag 一个文件都没改到（sed: $_sed）。
       代码形态变了，请更新变异点 —— 注入不进去的变异体会让反向对照悄悄失去意义：
       它照样报通过，因为跑的其实是原版代码。"
}

# ---- 变异体 A：让 portable 偷偷去构造 native 适配器 ----
#
# 这是 portable 最难发现的一种坏法：矩阵还是全 builtin、六个访问器也都返回
# builtin 实例，**但 native factory 被调用了**。副作用（探测、打开设备、
# 建临时文件）已经发生，而所有「结果对不对」的断言都看不出来。
# TestPortableIgnoresNativeEvenWhenAvailable 专门数了这个调用次数，这里验证它真管用。
MUT_A="$SCRATCH/provider-mutant.go"
sed 's/if len(m.Caps(KindNative)) > 0 \&\& native != nil {/if native != nil {/' \
    internal/oscap/provider.go > "$MUT_A"
if cmp -s internal/oscap/provider.go "$MUT_A"; then
    die "变异体 A 没生效：internal/oscap/provider.go 里没找到预期的那行
       （原文形如 if len(m.Caps(KindNative)) > 0 && native != nil {）。
       该行被改写过，请更新本脚本里的变异点 —— 一个注入不进去的变异体
       会让反向对照悄悄失去意义：它照样报通过，因为跑的其实是原版代码。"
fi
printf '{"Replace":{"%s":"%s"}}\n' "$REPO/internal/oscap/provider.go" "$MUT_A" \
    > "$SCRATCH/ov-a.json"
assert_red "变异体 A（portable 偷偷调用 native factory）" "$SCRATCH/ov-a.json" \
    '^--- FAIL: TestPortableIgnoresNativeEvenWhenAvailable'

# ---- 变异体 B：把 portable 用例改名，模拟「用例没了但流水线全绿」 ----
#
# 这一条验的是第 1 关那道必需用例断言本身。没有它，用例被改名之后
# `go test -run TestPortable` 会打印 ok 并退出 0，谁都不会发现。
# 改名用 `zz_<原名>`（带原名后缀）而不是统一定成同一个名字，否则多个
# `func zzRenamedAway` 会让同一个包里出现重复函数声明、变成编译错误而非
# 「用例消失」—— 那样红因就变成编译错误，反向对照的「红在预期判据」核对会失准。
mutate_test_files renamed "s/^func \($PORTABLE_RE[A-Za-z0-9_]*\)/func zz_\1/" "$SCRATCH/ov-b.json"
assert_red "变异体 B（portable 用例被改名 = 门禁空转）" "$SCRATCH/ov-b.json" \
    '门禁失败: 缺少必需的 portable 用例'

# （历史上还有过「变异体 E：只留必需用例、删其余」来验总数下界。但在
#  REQUIRED_PORTABLE_CASES 直接把下界定成「清单长度」之后，删非必需用例本就是
#  允许的——总数下界与清单长度相等，留着 12 条必需用例时总数刚好等于下界，
#  这道变异体不再对应任何「该拦的行为」，故移除，不制造假红。）

# ---- 变异体 C：子测试全部 t.Skip，父测试照样 PASS ----
#
# 这一条验的是第 2 关新增的「任意层级 SKIP 归零」。它抓的是最贴脸的一种假绿：
# 顶层枚举 35、顶层 PASS 35、顶层 SKIP 0 —— 三个数字全对，而六项能力的往返
# 一条都没执行。父测试不会因为子测试全 skip 而变成 SKIP，Go 不向上传播。
mutate_test_files skipsub \
    's|t\.Run("\([A-Za-z0-9_]*\)", func(t \*testing\.T) {|&\n\t\tt.Skip("mutant C")|' \
    "$SCRATCH/ov-c.json"
assert_red "变异体 C（子测试全 SKIP，父测试仍 PASS）" "$SCRATCH/ov-c.json" \
    '门禁失败: 有 [0-9]+ 条 portable 用例/子测试被 SKIP'

# ---- 变异体 D：子测试整体不执行（连 SKIP 都不留） ----
#
# 比 C 更隐蔽：把 `t.Run(名字, 闭包)` 换成一个立即调用、但把两个参数都丢掉的函数字面量，
# 于是子测试**连结果行都不打印**。C 还能靠 SKIP 计数抓，D 只能靠结果行总数抓。
# 现实里的同型坏法：循环的集合变空、断言被搬进一个再也没人调的 helper。
mutate_test_files dropsub \
    's|t\.Run("\([A-Za-z0-9_]*\)", func(t \*testing\.T) {|func(_ string, _ func(*testing.T)) {}("\1", func(t *testing.T) {|' \
    "$SCRATCH/ov-d.json"
assert_red "变异体 D（子测试整体不执行，无 SKIP 痕迹）" "$SCRATCH/ov-d.json" \
    '门禁失败: 含子测试的结果行只有'

say "OK: portable 路径已在本次构建中真实执行，且本门禁经反向对照证明有牙"
