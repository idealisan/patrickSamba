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

# C9 那一节（第 5 节）是唯一一个**必须往工作树里真落一个文件**的反向对照：
# check-constraints.sh 是文本扫描（find + grep），不经过 go 的构建系统，
# 所以 -overlay 那套「只存在于内存映射」的手法对它无效。
# 既然只能落盘，就必须保证**任何退出路径都清得掉** —— 残留一个违规样本
# 会让整个仓库的 CI 从此变红，那比没有门禁更糟。故列进 cleanup。
C9SCRATCH="$REPO/internal/vfs/zz_c9_negative_scratch.go"

cleanup() {
    if [ -f "$C9SCRATCH" ]; then rm "$C9SCRATCH"; fi
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

# ============================================================ 5. C9 门禁
say "5. C9 门禁：操作系统只作为「文件系统 + 套接字」提供方"

# 这一节验的是 scripts/check-constraints.sh 新增的 C9 段。
#
# 为什么非验不可：C9 守的是项目所有者亲自划的那条线 ——
#   「绝不应该依赖操作系统提供的任何相关机制，操作系统只被当作一个普通的
#     文件系统提供方」。
# 而在此之前这条线是靠**人肉 grep** 证明的。人肉 grep 只能证明「此刻是干净的」，
# 证明不了「以后有人弄脏时会被发现」。一盏从来没亮过的红灯，
# 和没有这盏灯是一回事 —— 这正是本文件存在的理由。
#
# 手法与前面几节不同：C9 是 find+grep 的文本扫描，不走 go 的构建系统，
# 所以 -overlay 对它无效，只能往工作树里真落一个样本文件（见顶部 C9SCRATCH 注释）。
# 样本带 `//go:build ignore`：go 工具链会完整跳过它（`ignore` 已在
# check-test-compile.sh 的 KNOWN 列表里），因此不会干扰 go build/vet/list；
# 而文本扫描照样看得见它 —— 要验的恰恰就是文本扫描这一层。

C9GATE="sh $REPO/scripts/check-constraints.sh"

# c9_expect_red <一句话说明> —— 断言当前样本会让 C9 判红。
#
# 两条断言缺一不可。只断言「退出码非零」是不够的：check-constraints.sh 里
# 随便哪一段红了都会非零退出（go list 挂掉、C8 段误伤、脚本自己语法错…），
# 那样这个反向对照就变成了**假阳性** —— 看着是"门禁抓住了"，实际抓的是别的东西。
# 所以第二条断言要求报错信息**点名 C9**。
c9_expect_red() {
    assert_rc fail "C9 · $1 · 门禁必须非零退出" sh -c "$C9GATE"
    assert_rc pass "C9 · $1 · 且报错点名 C9（排除「因别的原因红」的假阳性）" \
        sh -c "$C9GATE 2>&1 | grep -q '违反 C9'"
}

echo "  5a. 正向对照：干净树上 C9 必须是绿的"
echo "      （尤其要证明它没有误伤 internal/vfs/sys_windows.go 那行 kernel32 绑定 ——"
echo "       门禁误伤现有代码 = 整个仓库变红，比没有门禁更糟）"
assert_rc pass "干净树 · check-constraints.sh 全绿" sh -c "$C9GATE"

echo "  5b. 反向对照：挂载系统调用符号"
cat > "$C9SCRATCH" <<'EOF'
//go:build ignore

// 由 test/ci/negative-verify.sh 临时生成的 C9 违规样本，用完即删。
package vfs

func c9ScratchMount() error {
	return unix.Mount("//192.168.1.2/share", "/mnt/x", "cifs", 0, "")
}
EOF
c9_expect_red "unix.Mount 符号"

echo "  5c. 反向对照：命名空间常量"
cat > "$C9SCRATCH" <<'EOF'
//go:build ignore

// 由 test/ci/negative-verify.sh 临时生成的 C9 违规样本，用完即删。
package vfs

const c9ScratchFlags = CLONE_NEWUSER | CLONE_NEWNS
EOF
c9_expect_red "CLONE_NEWUSER / CLONE_NEWNS 命名空间常量"

echo "  5d. 反向对照：系统名字解析配置文件的字符串字面量"
cat > "$C9SCRATCH" <<'EOF'
//go:build ignore

// 由 test/ci/negative-verify.sh 临时生成的 C9 违规样本，用完即删。
package vfs

var c9ScratchResolv = "/etc/resolv.conf"
EOF
c9_expect_red "\"/etc/resolv.conf\" 字符串字面量"

echo "  5e. 反向对照：加载非系统 DLL"
cat > "$C9SCRATCH" <<'EOF'
//go:build ignore

// 由 test/ci/negative-verify.sh 临时生成的 C9 违规样本，用完即删。
package vfs

var c9ScratchProc = windows.NewLazySystemDLL("evil.dll").NewProc("DoEvil")
EOF
c9_expect_red "NewLazySystemDLL(\"evil.dll\") 非白名单 DLL"

echo "  5f. 正向对照：系统 DLL 白名单必须放行（豁免要真的生效，不能是「全都红」）"
echo "      一个只会判红的检查和一个只会判绿的检查同样没用；"
echo "      下面三条证明 C9 划的是**线**，不是一刀切。"
cat > "$C9SCRATCH" <<'EOF'
//go:build ignore

// 由 test/ci/negative-verify.sh 临时生成的 C9 正向样本，用完即删。
package vfs

var c9ScratchOK = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetDiskFreeSpaceW")
EOF
assert_rc pass "C9 · NewLazySystemDLL(\"kernel32.dll\") 属平台 ABI，必须放行" \
    sh -c "$C9GATE"

cat > "$C9SCRATCH" <<'EOF'
//go:build ignore

// 由 test/ci/negative-verify.sh 临时生成的 C9 正向样本，用完即删。
package vfs

import "net"

func c9ScratchSocket(ifi *net.Interface) (*net.UDPConn, error) {
	return net.ListenMulticastUDP("udp4", ifi, &net.UDPAddr{
		IP:   net.IPv4(224, 0, 0, 251),
		Port: 5353,
	})
}
EOF
assert_rc pass "C9 · UDP 组播套接字（C4 要求的 mDNS 做法）不得被误伤" \
    sh -c "$C9GATE"

cat > "$C9SCRATCH" <<'EOF'
//go:build ignore

// 由 test/ci/negative-verify.sh 临时生成的 C9 正向样本，用完即删。
package vfs

// 说明性注释：本项目禁止 unix.Mount、禁止 syscall.Chroot，
// 也禁止读取 "/etc/resolv.conf" 与 "/proc/net/tcp"。
// 这些词出现在注释里是完全正常的，不该把 CI 判红 ——
// 否则大家只会去改注释而不是改代码。
var c9ScratchDoc = 1
EOF
assert_rc pass "C9 · 注释里出现这些词不算违规（strip_comments 防假阳性）" \
    sh -c "$C9GATE"

echo "  5g. 清理并确认恢复绿（残留一个违规样本会让仓库 CI 从此变红）"
rm "$C9SCRATCH"
assert_rc pass "C9 · 移除样本后门禁恢复绿" sh -c "$C9GATE"
# 只断言样本文件本身不在了 —— 不要用 `git status --porcelain` 判，
# 那会把开发者自己在 internal/vfs/ 下的正常改动误判成残留。
assert_rc pass "C9 · 样本文件确已删除，工作树无残留" sh -c "[ ! -e '$C9SCRATCH' ]"

# ============================================================ 6. 洞 6
say "6. 洞 6：带 //go:build !linux && !darwin && !windows 的兜底文件从未被编译过"

# 故障素材：在 freebsd 专属兜底文件 native_other.go 里塞一个类型错误。
# 该文件只在「非 linux / 非 darwin / 非 windows」平台被编译（见文件头 build 约束），
# 所以 C7 的四平台（linux/amd64 linux/arm64 darwin/arm64 windows/amd64）永远不会
# 编译它 —— 在 check-test-compile.sh 加 freebsd/amd64 一档之前，这类文件里就算有
# 编译错误也永远发现不了。同型兜底文件共 7 个：
#   internal/oscap/native/native_other.go
#   internal/oscap/probe_other.go、probe_helper_other_test.go
#   internal/vfs/{attr,sparse,sys,xattr}_other.go
cat > "$SCRATCH/type-broken-native-other.go" <<'EOF'
//go:build !linux && !darwin && !windows

package native

var deliberateTypeError int = "this is not an int"
EOF

OV6=$(mkoverlay internal/oscap/native/native_other.go "$SCRATCH/type-broken-native-other.go")

echo "  6a. 正向对照：干净树（含 freebsd/amd64 五平台）上门禁全绿"
assert_rc pass "干净树 · 新门禁 check-test-compile.sh（含 freebsd/amd64 一档）" sh -c "$GATE"

echo "  6b. 反向对照（核心）：注入类型错误后，新门禁（含 freebsd 一档）必须拦住它，"
echo "      且报错必须点名这处注入的故障（排除「因别的原因红」的假阳性）。"
assert_rc fail "新门禁 · internal/oscap/native/native_other.go 类型错误" \
    with_overlay "$OV6" sh -c "$GATE"
assert_rc pass "新门禁 · 报错点名注入故障（含 'not an int' 字样，归因到 freebsd 兜底文件而非误伤）" \
    with_overlay "$OV6" sh -c "$GATE 2>&1 | grep -q 'not an int'"

echo "  6c. 归因对照：只用旧四平台（不含 freebsd/amd64）编译，同一份故障变回绿的 ——"
echo "      这正是洞存在的证据：freebsd 那一档之前，broken 兜底文件无人编译也无人发现。"
assert_rc pass "旧四平台 go vet（linux/amd64 linux/arm64 darwin/arm64 windows/amd64）对 native_other.go 类型错误无感" \
    with_overlay "$OV6" sh -c '
        set -e
        for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do
            GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go vet -tags integration,smoke,metabolt,qadefect ./...
        done'

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
