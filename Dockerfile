# stupidSamba 官方容器镜像。
#
# ============================================================================
# 这个 Dockerfile 里**一条 RUN 都没有**，这是刻意的，有两个理由。
# ============================================================================
#
# 理由一：多架构构建不需要 QEMU 模拟。
#   跨架构构建之所以慢、之所以经常在 CI 里超时，是因为 `RUN` 里的命令要在
#   目标架构的 rootfs 里执行，非本机架构就得走 binfmt_misc + QEMU 用户态模拟。
#   本文件只有 COPY，目标 rootfs 里从头到尾没有任何指令被执行，
#   于是 buildx 出双架构 manifest list 是**纯文件搬运**。
#   实测：linux/amd64 + linux/arm64 两个平台一起构建耗时 0.5 秒。
#
# 理由二：镜像里的二进制必须是**已经过发布自检**的那一份。
#   `scripts/build-release.sh` 对每个产物做了五项自检：CGO_ENABLED 真的是 0、
#   -trimpath 真的生效、GOOS/GOARCH 真的是目标平台、-ldflags 注入的版本号真的
#   读得出来、linux 产物 ldd 实证静态链接。若在这里 `RUN go build` 重编一遍，
#   镜像里的二进制就成了整个发布物里**唯一没被自检过**的东西，而且与裸包里的
#   那份不是同一个文件（构建时间、构建环境都不同）。
#   所以二进制由 scripts/docker-build.sh 预先摆进 dist/docker/ 再 COPY 进来。
#
# 构建方式（不要直接 docker build，上下文需要先摆好）：
#   scripts/docker-build.sh --tag v0.2.0
#
# ============================================================================
# 基础镜像选 scratch 的理由
# ============================================================================
#   distroless/static 比 scratch 多带三样东西，本项目一样都用不上：
#     - CA 证书：本服务不发起任何出网 TLS 连接（它是 SMB **服务端**），用不上。
#     - tzdata：没有 zoneinfo 时 Go 自动回落 UTC，服务端日志用 UTC 反而更合适。
#     - /etc/passwd 里的 nonroot 用户：AGENTS.md C8 要求认证完全自成体系，
#       账户只来自本软件的 YAML，**不读宿主 /etc/passwd**，所以镜像里根本
#       不需要存在任何系统用户。
#   产品代码里也没有任何 os.TempDir / MkdirTemp 调用（已 grep 核实），
#   不需要 /tmp。于是 scratch 是完整且最小的选择：镜像 = 一个静态二进制 + 一份配置。

FROM scratch

# TARGETARCH 由 buildx 按 --platform 自动注入（amd64 / arm64），
# 不需要也不应该在外面传。参见 docs.docker.com 的「Automatic platform ARGs」。
ARG TARGETARCH

# 下面三个由 scripts/docker-build.sh 传入，只用于 OCI 标签。
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="stupidSamba" \
      org.opencontainers.image.description="纯 Go 实现的极简 SMB/CIFS 文件共享服务器，自带进程内 mDNS/DNS-SD 广播与 Apple SMB 扩展" \
      org.opencontainers.image.source="https://github.com/idealisan/patrickSamba" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY dist/docker/linux_${TARGETARCH}/stupidsamba /usr/local/bin/stupidsamba

# 内置试用配置。开着 guest，只为了让 `docker run` 一条命令能跑起来；
# 生产环境请用 -v 挂载自己的配置覆盖它。理由与用法见 configs/docker.yaml 顶部注释。
COPY configs/docker.yaml /etc/stupidsamba/config.yaml

# 声明 /data 有两个作用：一是它就是内置配置里那个共享的路径，二是不挂载任何东西
# 直接 `docker run` 时，Docker 会为它建一个匿名卷并**创建挂载点目录** ——
# scratch 里没有 mkdir，这是让 /data 存在的办法（配置校验要求共享路径必须已存在）。
VOLUME ["/data"]

# SMB over Direct TCP。445 是特权端口，容器内默认以 root 运行故可绑定；
# 以非 root 运行时的做法见 docs/docker.md。
EXPOSE 445/tcp

# mDNS/DNS-SD。只在 --network host 下才有意义（桥接网络里组播出不了网桥），
# 内置配置默认把 mdns 关掉了，这里声明出来是为了让 `docker inspect` 能看到
# 本服务会用到这个端口。
EXPOSE 5353/udp

ENTRYPOINT ["/usr/local/bin/stupidsamba"]
CMD ["-config", "/etc/stupidsamba/config.yaml"]
