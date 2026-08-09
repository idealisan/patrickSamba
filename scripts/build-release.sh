#!/bin/sh
# build-release.sh —— 一条命令产出可分发的发布物。
#
# 用法：
#   scripts/build-release.sh              # 版本号自动取自 git describe
#   VERSION=v0.1.0 scripts/build-release.sh
#   ALLOW_MISSING_DOCS=1 scripts/build-release.sh   # 随包文档还没写完时先出包
#
# 产物落在 dist/：每个平台一个压缩包 + 一份 SHA256SUMS。
#
# 设计要点：
#   - **不依赖 zip(1)**。本开发容器和官方 golang 镜像都没装 zip，
#     而 Go 是必然有的 —— 用一段临时的 archive/zip 程序打包，
#     免得给发布流程引入 apt 依赖（装包在 CI 里既慢又会因为源抖动而随机失败）。
#   - **构建完要自检**，不能只看 go build 返回 0。历史教训是"加了字段没人读"，
#     所以这里逐个产物验证：CGO 真的关了、-trimpath 真的生效、
#     GOOS/GOARCH 真的是目标平台、-ldflags 注入的版本号真的能在 -version 里读出来。
set -e

cd "$(dirname "$0")/.."
repo=$(pwd -P)
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

BIN=stupidsamba
PKG=./cmd/stupidsamba
DIST=dist
PLATFORMS="linux/amd64 linux/arm64 darwin/arm64 windows/amd64"

# 随二进制一起分发的文件。路径在压缩包内保持与仓库一致，
# 这样 README 里写的 "configs/example.yaml" 拿到包里也是对的。
EXTRA_FILES="README.md CHANGELOG.md configs/example.yaml"

bad=0
info() { printf '\033[1;36m==> %s\033[0m\n' "$1"; }
pass() { printf '  \033[1;32mOK\033[0m  %s\n' "$1"; }
fail() { printf '  \033[1;31m!!\033[0m  %s\n' "$1" >&2; bad=1; }

# ------------------------------------------------------------------ 版本号
#
# 仓库在 v0.1.0 之前一个 tag 都没有，`git describe --tags` 会直接失败。
# 刻意不用 `--always` 兜底：那样拿到的是裸 commit（如 "f725e9c-dirty"），
# 既不像版本号，做成文件名 `stupidsamba_f725e9c-dirty_linux_amd64.tar.gz`
# 也无从判断新旧。宁可显式造一个语义化的开发版号。

if [ -n "${VERSION:-}" ]; then
    version=$VERSION
    version_src="环境变量 VERSION"
elif version=$(git describe --tags --match 'v*' --dirty 2>/dev/null); then
    version_src="git describe"
else
    short=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
    version="v0.0.0-dev+$short"
    [ -z "$(git status --porcelain 2>/dev/null)" ] || version="$version-dirty"
    version_src="无 tag，回退为开发版号"
fi

commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

info "版本 $version（来源：$version_src）  commit $commit  构建于 $date"

# ------------------------------------------------------------------ 随包文档
#
# 这三个文件缺了包也能出，但发出去的就是个裸二进制，用户拿到不知道怎么用。
# 所以默认视为错误 —— 静默跳过正是要防的那种"发布事故"。

missing=
for f in $EXTRA_FILES; do
    [ -f "$f" ] || missing="$missing $f"
done
if [ -n "$missing" ]; then
    printf '\033[1;31m错误: 以下随包文件不存在:\033[0m\n' >&2
    for f in $missing; do printf '  - %s\n' "$f" >&2; done
    if [ "${ALLOW_MISSING_DOCS:-}" = "1" ]; then
        printf '\033[1;33m已设置 ALLOW_MISSING_DOCS=1，继续构建，压缩包内将不含上述文件。\033[0m\n' >&2
    else
        printf '压缩包里没有它们，用户拿到只有一个裸二进制。\n' >&2
        printf '补齐这些文件后重跑；确实要先出包，用 ALLOW_MISSING_DOCS=1 覆盖。\n' >&2
        exit 1
    fi
    EXTRA_PRESENT=$(for f in $EXTRA_FILES; do [ -f "$f" ] && printf '%s ' "$f"; done)
else
    EXTRA_PRESENT=$EXTRA_FILES
fi

# ------------------------------------------------------------------ zip 打包器
#
# 生成一次，四个平台里只有 windows 用得上。

tmpdir=$(mktemp -d)
cleanup() { [ -n "$tmpdir" ] && rm -rf "$tmpdir"; }
trap cleanup EXIT INT TERM

cat > "$tmpdir/zipdir.go" <<'GOEOF'
// zipdir <root> <subdir> <out.zip>
// 把 <root>/<subdir> 打进 out.zip，包内保留 <subdir> 这一层目录。
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "用法: zipdir <root> <subdir> <out.zip>")
		os.Exit(2)
	}
	root, sub, out := os.Args[1], os.Args[2], os.Args[3]

	f, err := os.Create(out)
	check(err)
	w := zip.NewWriter(f)

	check(filepath.WalkDir(filepath.Join(root, sub), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		// zip 内路径分隔符固定是 '/'（APPNOTE 4.4.17.1），不能用 OS 分隔符。
		h.Name = filepath.ToSlash(rel)
		h.Method = zip.Deflate
		zw, err := w.CreateHeader(h)
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(zw, src)
		return err
	}))

	check(w.Close())
	check(f.Close())
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "zipdir:", err)
		os.Exit(1)
	}
}
GOEOF

# ------------------------------------------------------------------ 构建

rm -rf "$DIST"
mkdir -p "$DIST"

ldflags="-s -w -X main.version=$version -X main.commit=$commit -X main.date=$date"
host_os=$(go env GOOS)
host_arch=$(go env GOARCH)

for target in $PLATFORMS; do
    os=${target%/*}
    arch=${target#*/}
    exe=
    [ "$os" = "windows" ] && exe=.exe

    stem="${BIN}_${version}_${os}_${arch}"
    stage="$DIST/$stem"
    mkdir -p "$stage"

    info "构建 $os/$arch"
    GOOS=$os GOARCH=$arch CGO_ENABLED=0 \
        go build -trimpath -ldflags "$ldflags" -o "$stage/$BIN$exe" "$PKG"

    for f in $EXTRA_PRESENT; do
        mkdir -p "$stage/$(dirname "$f")"
        cp "$f" "$stage/$f"
    done

    if [ "$os" = "windows" ]; then
        (cd "$tmpdir" && go run zipdir.go "$repo/$DIST" "$stem" "$repo/$DIST/$stem.zip")
        archive="$DIST/$stem.zip"
    else
        tar -czf "$DIST/$stem.tar.gz" -C "$DIST" "$stem"
        archive="$DIST/$stem.tar.gz"
    fi

    # -------------------------------------------------------- 逐产物自检
    binpath="$stage/$BIN$exe"
    meta=$(go version -m "$binpath")

    # C1/C2：CGO 必须是关的。静态链接的根因就在这里，
    # 后面的 ldd 只是对 linux/amd64 的额外实证。
    printf '%s\n' "$meta" | grep -q 'CGO_ENABLED=0' \
        || fail "$target: 构建信息里没有 CGO_ENABLED=0"

    # 目标平台必须真的是目标平台（防止 GOOS/GOARCH 没传进去、拿了缓存的旧产物）。
    printf '%s\n' "$meta" | grep -q "GOOS=$os" || fail "$target: GOOS 不是 $os"
    printf '%s\n' "$meta" | grep -q "GOARCH=$arch" || fail "$target: GOARCH 不是 $arch"

    # -trimpath 生效了才不会把构建机的绝对路径写进二进制。
    printf '%s\n' "$meta" | grep -q -- '-trimpath=true' \
        || fail "$target: -trimpath 未生效"

    # 链接进来的模块必须都在 go.mod 里声明过。
    unexpected=$(printf '%s\n' "$meta" | awk '$1=="dep"{print $2}' | while read -r m; do
        grep -qF "$m " go.mod || printf '%s\n' "$m"
    done)
    [ -z "$unexpected" ] || fail "$target: 链接了 go.mod 未声明的模块: $unexpected"

    # 版本号必须真的注入进去了。本机平台直接跑 -version 是最硬的证据；
    # 交叉产物跑不了，退而求其次在二进制里找这个字符串。
    if [ "$os" = "$host_os" ] && [ "$arch" = "$host_arch" ]; then
        vout=$("$binpath" -version)
        case "$vout" in
            *"$version"*"$commit"*"$date"*) : ;;
            *) fail "$target: -version 输出缺少注入信息: $vout" ;;
        esac
    else
        grep -aqF -- "$version" "$binpath" \
            || fail "$target: 二进制中找不到注入的版本号 $version"
    fi

    # C2：静态链接的直接证据，只有本机能跑的 linux 产物验得了。
    if [ "$os" = "linux" ] && [ "$arch" = "$host_arch" ] && command -v ldd >/dev/null 2>&1; then
        # LC_ALL=C 不能省：ldd 的提示是走 gettext 的，中文环境下会打印
        # “不是动态可执行文件”，按英文串去匹配就会把静态产物误判成动态链接。
        lddout=$(LC_ALL=C ldd "$binpath" 2>&1 || true)
        case "$lddout" in
            *"not a dynamic executable"*|*"statically linked"*)
                pass "$target 静态链接（ldd 实证）" ;;
            *)
                fail "$target: 不是静态链接: $lddout" ;;
        esac
    fi

    size=$(du -h "$archive" | cut -f1)
    binsize=$(du -h "$binpath" | cut -f1)
    pass "$target -> $archive（包 $size，二进制 $binsize）"

    rm -rf "$stage"
done

# ------------------------------------------------------------------ 校验和

info "生成 SHA256SUMS"
(cd "$DIST" && sha256sum ./*.tar.gz ./*.zip > SHA256SUMS)
sed 's/^/  /' "$DIST/SHA256SUMS"

# ------------------------------------------------------------------ 收尾

info "dist/ 内容"
ls -lh "$DIST" | sed 's/^/  /'

if [ "$bad" != "0" ]; then
    printf '\033[1;31m构建完成但自检未全部通过，上面标 !! 的项必须修掉后才能发布。\033[0m\n' >&2
    exit 1
fi
info "全部平台构建与自检通过：$version"
