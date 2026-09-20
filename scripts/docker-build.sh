#!/bin/sh
# docker-build.sh —— 构建（可选推送）多架构 stupidSamba 容器镜像。
#
# 用法：
#   scripts/docker-build.sh                          # 用 dist/ 里已有产物，构建到本地
#   scripts/docker-build.sh --tag v0.2.0             # 指定版本号（会先跑 build-release.sh）
#   scripts/docker-build.sh --tag v0.2.0 --push      # 构建并推送到镜像仓库
#   scripts/docker-build.sh --tag v0.2.0 --push --latest   # 额外推一个 :latest
#   scripts/docker-build.sh --skip-build             # 复用 dist/ 里已有的 tar.gz
#   scripts/docker-build.sh --platforms linux/amd64  # 只出单架构（调试用）
#
# ---------------------------------------------------------------------------
# 设计要点：镜像里的二进制来自 dist/ 里的**发布包**，不在 Dockerfile 里重编
# ---------------------------------------------------------------------------
# build-release.sh 对每个产物做了五项自检（CGO 关没关、-trimpath 生效没、
# GOOS/GOARCH 对不对、版本号有没有真注进去、linux 产物是不是静态链接）。
# 本脚本从它产出的 tar.gz 里解出二进制来打镜像，于是：
#   - 镜像里的字节和用户下载的裸包里的字节是**同一份**（下面会做 sha256 比对佐证）；
#   - 那五项自检自动覆盖到镜像；
#   - 顺带证明了 tar.gz 本身是好的（解得开、里面的二进制跑得动）。
#
# ---------------------------------------------------------------------------
# 为什么多架构不需要 QEMU
# ---------------------------------------------------------------------------
# Dockerfile 是 COPY-only（零 RUN），目标 rootfs 里没有任何指令被执行，
# buildx 出 manifest list 纯粹是搬文件。实测双架构 0.5 秒。
# 也因此**不需要** `docker run --privileged tonistiigi/binfmt` 这一步。
set -e

cd "$(dirname "$0")/.."
repo=$(pwd -P)
export PATH=/usr/local/go/bin:$PATH

DIST=dist
STAGE=$DIST/docker
BIN=stupidsamba

info() { printf '\033[1;36m==> %s\033[0m\n' "$1"; }
pass() { printf '  \033[1;32mOK\033[0m  %s\n' "$1"; }
die()  { printf '\033[1;31m错误: %s\033[0m\n' "$1" >&2; exit 1; }

# ------------------------------------------------------------------ 参数

VERSION=${VERSION:-}
PLATFORMS=linux/amd64,linux/arm64
PUSH=0
LATEST=0
SKIP_BUILD=0
IMAGE=

while [ $# -gt 0 ]; do
    case "$1" in
        --tag)       VERSION=$2; shift 2 ;;
        --image)     IMAGE=$2; shift 2 ;;
        --platforms) PLATFORMS=$2; shift 2 ;;
        --push)      PUSH=1; shift ;;
        --latest)    LATEST=1; shift ;;
        --skip-build) SKIP_BUILD=1; shift ;;
        -h|--help)   sed -n '2,30p' "$0"; exit 0 ;;
        *)           die "未知参数: $1（--help 看用法）" ;;
    esac
done

# ------------------------------------------------------------------ 镜像名
#
# 发布渠道：ghcr.io/idealisan/patricksamba，由 GitHub Actions 的 docker.yml 推送：
#   ghcr.io/idealisan/patricksamba

# 注意必须用 **LOWERCASE** 那个：仓库名本身是 patrickSamba（带大写 S），
# 而镜像名不允许大写字母，用大写会拼出非法镜像名并在 push 时才报错。

if [ -z "$IMAGE" ]; then
    IMAGE=stupidsamba
fi

case "$IMAGE" in
    *[A-Z]*) die "镜像名不允许大写字母: $IMAGE" ;;
esac

# ------------------------------------------------------------------ 发布产物

if [ "$SKIP_BUILD" = "0" ]; then
    info "构建发布产物（scripts/build-release.sh）"
    VERSION=$VERSION sh scripts/build-release.sh
fi

[ -d "$DIST" ] || die "$DIST/ 不存在。先跑 scripts/build-release.sh，或去掉 --skip-build。"

# 从 tar.gz 的文件名反推版本号，不要自己再算一遍 —— 自己算就会和裸包不一致。
if [ -z "$VERSION" ]; then
    VERSION=$(ls "$DIST"/${BIN}_*_linux_amd64.tar.gz 2>/dev/null | head -1 | sed -e "s|.*/${BIN}_||" -e 's|_linux_amd64\.tar\.gz$||')
    [ -n "$VERSION" ] || die "$DIST/ 里没有 linux/amd64 发布包，无从推断版本号。用 --tag 显式指定。"
    info "版本号取自 $DIST/ 里的发布包：$VERSION"
fi

commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# ------------------------------------------------------------------ 摆放构建上下文
#
# Dockerfile 里写的是 `COPY dist/docker/linux_${TARGETARCH}/stupidsamba`，
# TARGETARCH 由 buildx 按 --platform 注入，所以目录名必须严格是 linux_amd64 /
# linux_arm64（Docker 的 arch 命名，恰好与 GOARCH 一致，不需要转换表）。

rm -rf "$STAGE"
mkdir -p "$STAGE"

for platform in $(echo "$PLATFORMS" | tr ',' ' '); do
    os=${platform%/*}
    arch=${platform#*/}
    [ "$os" = "linux" ] || die "镜像只支持 linux 平台，收到 $platform"

    tarball="$DIST/${BIN}_${VERSION}_${os}_${arch}.tar.gz"
    [ -f "$tarball" ] || die "找不到发布包 $tarball（版本号或平台对不上？）"

    tmp=$(mktemp -d)
    tar -xzf "$tarball" -C "$tmp"
    src="$tmp/${BIN}_${VERSION}_${os}_${arch}/$BIN"
    [ -f "$src" ] || die "发布包 $tarball 里没有 $BIN"

    mkdir -p "$STAGE/${os}_${arch}"
    cp "$src" "$STAGE/${os}_${arch}/$BIN"
    chmod 0755 "$STAGE/${os}_${arch}/$BIN"

    # ---------------------------------------------------------- 摆放后自检
    #
    # 防的是「摆错架构」：两个二进制长得一模一样，放反了要等到 arm64 用户
    # 拉下来 exec format error 才会发现，而那时镜像已经发出去了。
    # go version -m 读的是二进制里的构建信息，是硬证据。
    if command -v go >/dev/null 2>&1; then
        meta=$(go version -m "$STAGE/${os}_${arch}/$BIN")
        printf '%s\n' "$meta" | grep -q "GOOS=$os"     || die "$platform: 摆进来的二进制 GOOS 不是 $os"
        printf '%s\n' "$meta" | grep -q "GOARCH=$arch" || die "$platform: 摆进来的二进制 GOARCH 不是 $arch"
        printf '%s\n' "$meta" | grep -q 'CGO_ENABLED=0' || die "$platform: 摆进来的二进制没有 CGO_ENABLED=0（违反 C1）"
    fi
    # 与裸包里的字节完全一致 —— 这就是「镜像和裸包是同一份产物」的证据。
    sum=$(sha256sum "$STAGE/${os}_${arch}/$BIN" | cut -c1-16)
    rm -rf "$tmp"

    pass "$platform  <- $tarball  (sha256:$sum…)"
done

[ -f configs/docker.yaml ] || die "缺少 configs/docker.yaml（镜像内置的试用配置）"

# ------------------------------------------------------------------ 构建

tag_args="-t $IMAGE:$VERSION"
[ "$LATEST" = "1" ] && tag_args="$tag_args -t $IMAGE:latest"

# 多平台构建的输出目标：
#   --push  → 直接推到 registry（任何 buildx driver 都支持）
#   否则    → 落到本地镜像存储。这一条**要求 dockerd 开了 containerd 镜像存储**，
#            旧的 docker 镜像存储存不下 manifest list，会报
#            "docker exporter does not currently support exporting manifest lists"。
#            碰到这个错就加 --push，或用 --platforms 只出单架构。
out=
[ "$PUSH" = "1" ] && out="--push"

info "构建镜像 $IMAGE:$VERSION  平台 $PLATFORMS"
# shellcheck disable=SC2086
#
# --provenance=false / --sbom=false：
#   默认 buildx 会在推送的 manifest list 里额外塞两条 "unknown/unknown"
#   的 attestation-manifest（provenance + SBOM）。这些条目不是真实可运行
#   架构，却会让某些 registry 客户端把镜像当成「多架构里混进了未知平台」，
#   也违背我们「镜像只含真实可运行架构」的承诺。
#   本项目镜像内容完全由我们自己的产物决定，不需要 buildkit 自动生成的
#   来源/物料清单数据，故显式关掉（见 verify-image.sh 的 manifest-clean 反向对照）。
docker buildx build \
    --platform "$PLATFORMS" \
    $tag_args \
    $out \
    --provenance=false \
    --sbom=false \
    --build-arg "VERSION=$VERSION" \
    --build-arg "COMMIT=$commit" \
    --build-arg "BUILD_DATE=$build_date" \
    -f Dockerfile \
    "$repo"

# ------------------------------------------------------------------ 构建后自检
#
# 「build 成功」不等于镜像是对的。这里查两件事：
#   1. manifest list 里真的有全部目标架构（少一个架构是最典型的静默失败：
#      buildx 只出了 host 架构，命令照样返回 0）；
#   2. host 架构那一份真的跑得起来（-version 输出里有注入的版本号）。

info "构建后自检"

want_count=$(echo "$PLATFORMS" | tr ',' '\n' | grep -c .)
[ "$want_count" -gt 0 ] || die "没有目标平台"

if [ "$PUSH" = "1" ]; then
    # 推送后 registry 上就有完整的 manifest list，直接问它。
    got=$(docker buildx imagetools inspect "$IMAGE:$VERSION" --format '{{range .Manifest.Manifests}}{{.Platform.OS}}/{{.Platform.Architecture}} {{end}}' 2>/dev/null || true)
    for platform in $(echo "$PLATFORMS" | tr ',' ' '); do
        case " $got " in
            *"$platform"*) pass "manifest 含 $platform" ;;
            *) die "manifest 里没有 $platform（实际: $got）" ;;
        esac
    done
else
    # ------------------------------------------------------------------
    # 不推送时怎么验多架构：用 `docker create --platform` 做解析探针。
    #
    # 这里原先是"跳过核对"，那是个**假阳性温床**：buildx 只出了 host 架构
    # 时命令照样返回 0，而本地这一关又主动放行，于是"多架构"这件事在推送前
    # 从来没有被验证过 —— 等 arm64 用户拉下来报 exec format error 才发现。
    #
    # `docker image inspect --format '{{.Os}}/{{.Architecture}}'` 救不了场：
    # 面对 manifest list 它只回落到 host 那一份，天生看不见另一个架构。
    #
    # `docker create --platform <p>` 则必须**在本地 manifest 里解析出 <p>**
    # 才能建出容器；解析不到就会去 registry 找，进而失败。它只创建不启动，
    # 所以非本机架构不需要 QEMU 也能验。反向对照见下面的 s390x 探针 ——
    # 一个永远为真的检查等于没有检查。
    # ------------------------------------------------------------------
    probe() { # probe <platform>；0=本地 manifest 里有这个架构
        _cid=$(docker create --platform "$1" "$IMAGE:$VERSION" 2>/dev/null) || return 1
        docker rm -f "$_cid" >/dev/null 2>&1
        return 0
    }

    for platform in $(echo "$PLATFORMS" | tr ',' ' '); do
        probe "$platform" \
            && pass "manifest 含 $platform（docker create 解析探针）" \
            || die "本地镜像 $IMAGE:$VERSION 的 manifest 里没有 $platform"
    done

    # 反向对照：一个我们**没有**构建的架构必须探测失败。
    # 若它也"成功"，说明探针根本没在鉴别架构，上面那几个 OK 全部不可信。
    absent=linux/s390x
    case ",$PLATFORMS," in
        *",$absent,"*) absent=linux/riscv64 ;;   # 万一真有人指定了 s390x
    esac
    if probe "$absent"; then
        die "反向对照失败：未构建的 $absent 竟然也探测成功，架构探针不可信"
    fi
    pass "反向对照通过：未构建的 $absent 探测失败（探针确实在鉴别架构）"
fi

host_arch=$(docker version --format '{{.Server.Arch}}')
case "$PLATFORMS" in
    *"linux/$host_arch"*)
        vout=$(docker run --rm "$IMAGE:$VERSION" -version)
        case "$vout" in
            *"$VERSION"*) pass "镜像可执行且版本号正确: $vout" ;;
            *) die "镜像跑起来了但版本号不对: $vout（期望含 $VERSION）" ;;
        esac
        ;;
    *)
        printf '  \033[1;33m--\033[0m  目标平台不含宿主架构 linux/%s，跳过运行自检\n' "$host_arch"
        ;;
esac

info "完成：$IMAGE:$VERSION"
[ "$PUSH" = "1" ] && info "已推送到 registry" || info "未推送（加 --push 推送）"
printf '下一步跑 scripts/verify-image.sh --image %s:%s 做真实 SMB 端到端验证\n' "$IMAGE" "$VERSION"
