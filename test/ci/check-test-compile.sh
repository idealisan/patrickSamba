#!/bin/sh
# check-test-compile.sh —— 保证**每一个 .go 文件都真的被编译器看过**。
#
# 这一关堵的是两个已确诊的「假绿」漏洞，都是真实发生过、且直到 v0.1.0
# 发布前夕才被发现的：
#
#   洞 1：`test/` 下所有文件都带 `//go:build integration`，
#         而 CI 的 build / vet / test 三关**都不带 tag**，
#         于是这些文件从来没有被编译过 —— 里面写个语法错误照样一路绿。
#         同样中招的还有 internal/mdns/responder_integration_test.go。
#
#   洞 2：`go build` **根本不编译 _test.go**。
#         所以「交叉编译校验」那一关（对四个平台跑 go build）
#         抓不到 internal/vfs/metadata_windows_test.go 这类平台专属测试文件。
#         Windows 后端正在开发中，这个洞尤其危险：在 Linux 上跑 go test 时
#         windows 专属测试被 GOOS 排除，在跨平台 go build 时又被 _test.go 排除，
#         两头落空，等于永远没人编译它。
#
# 解法：用 `go vet` 而不是 `go build` —— vet 做完整类型检查**且包含测试文件**；
# 再对「所有 build tag 的并集」× 「所有目标平台」各跑一遍。
#
# 为什么不用 `go test -run '^$'`：集成测试要起真服务、要 smbclient，
# 构建机上跑不了；而且 TestMain 照样会被执行。我们这里只要「能编译」。
#
# 关于 tag 的并集是否够：本仓库没有 `//go:build !integration` 这类**反向**约束，
# 所以「带上全部 tag 编译到的文件集合」是「不带 tag」的超集，跑一遍就够。
# 将来若真出现反向约束，下面要改成带 tag / 不带 tag 各跑一遍。
# 脚本里有一道自检会在出现反向约束时直接报错提醒（见下）。
#
# 用法：
#   sh test/ci/check-test-compile.sh
#
# 负向验证见 test/ci/negative-verify.sh —— 没做过负向验证的门禁一律不算数。
set -e

cd "$(dirname "$0")/../.."

export CGO_ENABLED=0
command -v go >/dev/null 2>&1 || PATH=$PATH:/usr/local/go/bin
export PATH

# 本仓库用到的全部 build tag。新增 tag 时必须同步加到这里，
# 否则新 tag 下的文件又会变成没人编译的死角。
# `metabolt` 是 internal/meta 给 Linux CI 用的逃生 tag：bolt.go 带
# `//go:build windows || metabolt`，Linux 上必须靠它才能编译/测试。
# 它的存在同时带来反向约束 `!metabolt`（noop.go），与 `!windows` 同理——
# 脚本对平台类反向约束本来就放行（见下方 KNOWN），这里把 metabolt 也纳入，
# 让 `!metabolt` 走同一套「已知反向约束、不报错」的处理。
TAGS=integration,smoke,metabolt

# ------------------------------------------------------- 0. tag 清单自检
#
# 门禁最怕的是「悄悄失效」：有人加了个新 build tag（比如 `//go:build e2e`），
# 上面的 TAGS 没跟着改，那些文件就重新变成死角，而门禁依旧绿。
# 这里把源码里出现的 tag 名扫一遍，和 TAGS 对账；顺便拦住反向约束。
scan_tags() {
    # 只看约束行本身；平台/架构类 tag（linux、windows、amd64、unix…）由
    # 下面的跨平台循环覆盖，不需要出现在 TAGS 里。
    grep -rh --include='*.go' '^//go:build ' . 2>/dev/null |
        sed 's|^//go:build ||' |
        tr ' ()' '\n\n\n' |
        sed 's/^&&$//; s/^||$//' |
        grep -v '^$' | sort -u
}

KNOWN='linux|darwin|windows|unix|js|wasip1|plan9|aix|android|dragonfly|freebsd|hurd|illumos|ios|netbsd|openbsd|solaris|amd64|arm64|386|arm|riscv64|ppc64|ppc64le|s390x|mips.*|loong64|wasm|cgo|race|go1\..*|gc|gccgo|ignore|metabolt'

UNKNOWN=$(scan_tags | grep -Ev "^(!?($KNOWN))$" || true)
for t in $UNKNOWN; do
    case "$t" in
    !*)
        echo "错误: 发现反向 build 约束 '$t'。" >&2
        echo "      本脚本的「tag 并集是超集」假设不再成立，" >&2
        echo "      必须改成带 tag / 不带 tag 各跑一遍。见脚本头部注释。" >&2
        exit 1
        ;;
    esac
    case ",$TAGS," in
    *",$t,"*) ;;
    *)
        echo "错误: build tag '$t' 出现在源码里，但不在本脚本的 TAGS=$TAGS 中。" >&2
        echo "      这意味着带该 tag 的文件没有任何一关会编译它（历史上的假绿就是这么来的）。" >&2
        echo "      请把它加进 test/ci/check-test-compile.sh 的 TAGS。" >&2
        exit 1
        ;;
    esac
done
echo "OK: build tag 清单自检通过 (TAGS=$TAGS)"

# ------------------------------------------------------- 1. 本机 + 全部 tag
echo ">>> go vet -tags $TAGS ./...   (GOOS=$(go env GOOS)/$(go env GOARCH))"
go vet -tags "$TAGS" ./...

# ------------------------------------------------------- 2. 跨平台（含 _test.go）
#
# 平台列表与 AGENTS.md C7 一致。本机那一档上面已经跑过，这里不重复。
for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do
    _os=${t%/*}
    _arch=${t#*/}
    [ "$_os/$_arch" = "$(go env GOOS)/$(go env GOARCH)" ] && continue
    echo ">>> GOOS=$_os GOARCH=$_arch go vet -tags $TAGS ./..."
    GOOS=$_os GOARCH=$_arch go vet -tags "$TAGS" ./...
done

echo "OK: 所有 .go 文件（含 _test.go、含全部 build tag、含全部目标平台）均通过类型检查"
