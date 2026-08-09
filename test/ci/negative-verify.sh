#!/bin/sh
# negative-verify.sh —— 对 CI 门禁做**负向验证**。
#
# 为什么必须有这个文件
# --------------------
# 本项目已经踩过不止一次「门禁看着存在、其实永远绿」的坑：
#   - `.cnb.yml` 把 `main:` 写成了事件名，整段被静默忽略，
#     向 main 推了几十个 commit 一次校验都没跑过；
#   - gofmt 门禁存在，但 HEAD 上就是红的，没人发现；
#   - `test/` 靠 `//go:build integration` 隔离，而所有校验都不带 tag，
#     那些文件从来没被编译过。
#
# 结论：**没做过负向验证的门禁一律不算数。** 一个门禁至少要证明两件事：
#   1. 干净树上它是绿的（否则没人会认真看它）；
#   2. 故意造一个违反的场景，它**必须**变红（否则它只是个摆设）。
#
# 本脚本用 `go` 的 `-overlay` 机制注入故障：故障内容只存在于内存映射里，
# **工作树一个字节都不会被改**，所以不存在「注入的故障不小心被提交」的风险，
# 也不会砸到共用同一份 .git 的其他 worktree。
#
# 用法：
#   sh test/ci/negative-verify.sh
# 全部通过时退出码 0，并打印一份「哪个门禁拦住了哪个故障」的对照表。
set -e

cd "$(dirname "$0")/../.."
REPO=$(pwd)

command -v go >/dev/null 2>&1 || PATH=$PATH:/usr/local/go/bin
export PATH
export CGO_ENABLED=0

SCRATCH=$(mktemp -d /tmp/ss-negverify.XXXXXX)
cleanup() {
    rm -rf "$SCRATCH" "$REPO/test/ci/.negative-scratch"
}
trap cleanup EXIT INT TERM

PASS=0
FAIL=0
REPORT="$SCRATCH/report.txt"
: > "$REPORT"

# assert_rc <期望 pass|fail> <一句话说明> <命令...>
assert_rc() {
    _want=$1
    _what=$2
    shift 2
    _out="$SCRATCH/out.$$"
    _rc=0
    "$@" > "$_out" 2>&1 || _rc=$?
    if [ "$_want" = pass ]; then
        _okcond=$([ "$_rc" -eq 0 ] && echo y || echo n)
    else
        _okcond=$([ "$_rc" -ne 0 ] && echo y || echo n)
    fi
    if [ "$_okcond" = y ]; then
        printf '  \033[1;32m[OK]\033[0m   %s (期望%s，rc=%s)\n' "$_what" "$_want" "$_rc"
        PASS=$((PASS + 1))
        printf 'OK   %s\n' "$_what" >> "$REPORT"
    else
        printf '  \033[1;31m[BAD]\033[0m  %s (期望%s，实际 rc=%s)\n' "$_what" "$_want" "$_rc"
        sed 's/^/         /' "$_out" | head -8
        FAIL=$((FAIL + 1))
        printf 'BAD  %s\n' "$_what" >> "$REPORT"
    fi
    rm -f "$_out"
}

say() { printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"; }

# mkoverlay <目标源文件相对路径> <替换内容文件> —— 生成 overlay JSON，回显其路径
mkoverlay() {
    _n=$(echo "$1" | tr '/.' '__')
    _j="$SCRATCH/overlay-$_n.json"
    printf '{"Replace":{"%s/%s":"%s"}}\n' "$REPO" "$1" "$2" > "$_j"
    echo "$_j"
}

# 故障素材 1：语法错误（少一个右花括号 + 垃圾 token）
cat > "$SCRATCH/syntax-broken.go" <<'EOF'
//go:build integration

package integration

func ThisFileIsDeliberatelyBroken( {
EOF

# 故障素材 2：类型错误（把 string 赋给 int），语法合法、只有类型检查抓得住。
# 用于证明「go build 不编译 _test.go」这个洞：语法/类型都错，go build 依旧绿。
cat > "$SCRATCH/type-broken-windows.go" <<'EOF'
//go:build windows

package vfs

import "testing"

func TestDeliberateTypeError(t *testing.T) {
	var n int = "this is not an int"
	_ = n
}
EOF

cat > "$SCRATCH/syntax-broken-mdns.go" <<'EOF'
//go:build integration

package mdns

func AlsoDeliberatelyBroken( {
EOF

GATE="sh $REPO/test/ci/check-test-compile.sh"

# 门禁脚本自己不认识 -overlay，用 GOFLAGS 透传（go 对所有子命令都读 GOFLAGS）。
# 注意保留 devenv.sh 设的 -p=1，别把削峰配置冲掉。
BASE_GOFLAGS=${GOFLAGS:--p=1}
with_overlay() {
    _json=$1
    shift
    GOFLAGS="$BASE_GOFLAGS -overlay=$_json" "$@"
}

# ============================================================ 0. 正向对照
#
# 先证明干净树上一切是绿的。这一步不能省：如果门禁在干净树上就是红的，
# 后面「注入故障后变红」就毫无意义了（历史上 gofmt 门禁正是这个状态）。
say "0. 正向对照：干净树上门禁必须全绿"
assert_rc pass "干净树 · 新门禁 check-test-compile.sh" sh -c "$GATE"
assert_rc pass "干净树 · gofmt 门禁（与 .cnb.yml 中逐字一致）" sh -c '
    out=$(gofmt -l .)
    if [ -n "$out" ]; then echo "以下文件未格式化:"; echo "$out"; exit 1; fi'

# ============================================================ 1. 洞 1
say "1. 洞 1：带 //go:build integration 的文件从来没被编译过"

OV1=$(mkoverlay test/integration/harness_test.go "$SCRATCH/syntax-broken.go")

echo "  1a. 先证明这个洞是真的 —— 注入语法错误后，现有三关**依然全绿**："
assert_rc pass "旧关 go build ./...        对 test/ 的语法错误无感" \
    with_overlay "$OV1" go build ./...
assert_rc pass "旧关 go vet ./...          对 test/ 的语法错误无感" \
    with_overlay "$OV1" go vet ./...
assert_rc pass "旧关 go test ./...（仅编译）对 test/ 的语法错误无感" \
    with_overlay "$OV1" go test -count=1 -run 'ZZZ_NO_SUCH_TEST' ./...

echo "  1b. 新门禁必须拦住它："
assert_rc fail "新门禁 · test/integration 语法错误" \
    with_overlay "$OV1" sh -c "$GATE"

OV1b=$(mkoverlay internal/mdns/responder_integration_test.go "$SCRATCH/syntax-broken-mdns.go")
echo "  1c. 同一个洞在 test/ 之外也有一个（internal/mdns），一并验："
assert_rc pass "旧关 go vet ./...          对 mdns 集成测试的语法错误无感" \
    with_overlay "$OV1b" go vet ./...
assert_rc fail "新门禁 · internal/mdns 集成测试语法错误" \
    with_overlay "$OV1b" sh -c "$GATE"

# ============================================================ 2. 洞 2
say "2. 洞 2：go build 不编译 _test.go，跨平台那一关抓不到平台专属测试文件"

OV2=$(mkoverlay internal/vfs/metadata_windows_test.go "$SCRATCH/type-broken-windows.go")

echo "  2a. 先证明这个洞是真的 —— 注入类型错误后，跨平台 go build **依然全绿**："
assert_rc pass "旧关 交叉编译校验（四平台 go build）对 _windows_test.go 无感" \
    with_overlay "$OV2" sh -c '
        set -e
        for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do
            GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build ./...
        done'
assert_rc pass "旧关 go test ./...（本机 linux）也看不到 windows 测试文件" \
    with_overlay "$OV2" go test -count=1 -run 'ZZZ_NO_SUCH_TEST' ./internal/vfs/

echo "  2b. 新门禁必须拦住它："
assert_rc fail "新门禁 · internal/vfs/metadata_windows_test.go 类型错误" \
    with_overlay "$OV2" sh -c "$GATE"

# ============================================================ 3. gofmt 门禁
say "3. gofmt 门禁：确认它真的会让流水线失败"

mkdir -p "$REPO/test/ci/.negative-scratch"
# 故意写得不合 gofmt：缩进用空格、赋值两边多余空格、花括号换行。
printf 'package scratch\n\nfunc  Bad( ) int {\n   x  :=  1\n     return x\n}\n' \
    > "$REPO/test/ci/.negative-scratch/unformatted.go"

assert_rc fail "gofmt 门禁 · 存在未格式化文件时必须失败" sh -c '
    out=$(gofmt -l .)
    if [ -n "$out" ]; then echo "以下文件未格式化:"; echo "$out"; exit 1; fi'

# 顺带确认 save.sh 的 gofmt 门禁（本地那一道）也拦得住同一个文件。
# 只调它的检查语义，不真的提交 —— 直接复用 gofmt -l 对单路径的行为。
assert_rc fail "save.sh 同款 · gofmt -l <路径> 对该文件有输出" sh -c '
    out=$(gofmt -l test/ci/.negative-scratch)
    [ -z "$out" ]'

rm -rf "$REPO/test/ci/.negative-scratch"
assert_rc pass "移除后 gofmt 门禁恢复绿" sh -c '
    out=$(gofmt -l .)
    if [ -n "$out" ]; then echo "以下文件未格式化:"; echo "$out"; exit 1; fi'

# ============================================================ 3b. 竞态检测门禁
say "3b. 竞态检测门禁：确认它真的抓得住数据竞争"

# 先记录这一关**曾经**的样子：`CGO_ENABLED=0 go test -race`。
# race 检测器依赖 cgo，所以这条命令 0.1 秒就以返回码 2 退出，一个测试都没跑，
# 并且把它后面的四关（交叉编译/静态链接/示例配置/冒烟）全部拖进 skip。
# 保留这条断言是为了防止有人「顺手」把 CGO_ENABLED 改回 0。
assert_rc fail "回归 · CGO_ENABLED=0 go test -race 必定拒绝执行（旧配置就是这样红的）" \
    sh -c 'CGO_ENABLED=0 go test -race -count=1 ./internal/config/'

# 注入一个真实的数据竞争，验证 race 门禁有牙。overlay 支持映射一个**不存在**的
# 路径，等于凭空加一个文件进去，工作树依旧零改动。
cat > "$SCRATCH/race-injected_test.go" <<'EOF'
package config

import (
	"sync"
	"testing"
)

func TestDeliberateDataRace(t *testing.T) {
	x := 0
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5000; j++ {
				x++
			}
		}()
	}
	wg.Wait()
	_ = x
}
EOF
OV3=$(mkoverlay internal/config/zz_race_injected_test.go "$SCRATCH/race-injected_test.go")

assert_rc pass "普通单元测试关对数据竞争无感（所以 race 关不能省）" \
    with_overlay "$OV3" sh -c 'CGO_ENABLED=0 go test -count=1 ./internal/config/'
assert_rc fail "race 门禁 · CGO_ENABLED=1 go test -race 必须抓住注入的数据竞争" \
    with_overlay "$OV3" sh -c 'CGO_ENABLED=1 go test -race -count=1 ./internal/config/'

# ============================================================ 3c. 静态链接门禁的 locale 依赖
say "3c. 静态链接门禁：判定不能取决于机器装了什么语言包"

# ldd 的输出是本地化的：中文环境打印「不是动态可执行文件」，
# 匹配不上英文串 "not a dynamic executable"，门禁就误报「产物不是静态链接」。
# 本地就是中文环境，正好当反向对照用。
CGO_ENABLED=0 go build -o "$SCRATCH/staticprobe" ./cmd/stupidsamba

assert_rc pass "静态链接门禁 · 加了 LC_ALL=C 之后判定正确（与 .cnb.yml 逐字一致）" sh -c "
    if LC_ALL=C ldd '$SCRATCH/staticprobe' 2>&1 | grep -qv 'not a dynamic executable'; then
        echo '错误: 产物不是静态链接'; exit 1
    fi"

if LC_ALL=C ldd "$SCRATCH/staticprobe" 2>&1 | head -1 | grep -q 'not a dynamic'; then
    if [ "$(ldd "$SCRATCH/staticprobe" 2>&1 | head -1)" != "$(LC_ALL=C ldd "$SCRATCH/staticprobe" 2>&1 | head -1)" ]; then
        printf '  \033[1;32m[OK]\033[0m   本机 ldd 确实是本地化输出，LC_ALL=C 这一手不是多余的\n'
        PASS=$((PASS + 1))
    else
        printf '  \033[1;33m[--]\033[0m   本机 ldd 输出本来就是英文，locale 这一档在此机器上无法对照\n'
    fi
fi

# ============================================================ 4. 门禁自身的防腐
say "4. 门禁自身的防腐：新增 build tag 时必须报错，而不是悄悄漏掉"

mkdir -p "$REPO/test/ci/.negative-scratch"
cat > "$REPO/test/ci/.negative-scratch/newtag.go" <<'EOF'
//go:build e2e

package scratch
EOF

assert_rc fail "新门禁 · 出现未登记的 build tag 'e2e' 时必须报错" sh -c "$GATE"

cat > "$REPO/test/ci/.negative-scratch/newtag.go" <<'EOF'
//go:build !integration

package scratch
EOF

assert_rc fail "新门禁 · 出现反向约束 '!integration' 时必须报错" sh -c "$GATE"

rm -rf "$REPO/test/ci/.negative-scratch"
assert_rc pass "清理后新门禁恢复绿" sh -c "$GATE"

# ============================================================ 汇总
say "汇总"
printf '  通过 %s 项，失败 %s 项\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then
    echo
    echo "有门禁没有按预期工作。失败项："
    grep '^BAD' "$REPORT" | sed 's/^/    /'
    exit 1
fi
echo "  全部门禁均已通过负向验证。"
