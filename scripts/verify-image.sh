#!/bin/sh
# verify-image.sh —— 对**容器镜像**做真实 SMB 端到端验收。
#
# 用法：
#   scripts/verify-image.sh --image stupidsamba:v0.2.0
#   scripts/verify-image.sh --image ... --port 4460   # 换宿主端口（默认 4459）
#   KEEP=1 scripts/verify-image.sh --image ...        # 失败后保留容器便于看日志
#
# 退出码：0 = 全部通过；1 = 有用例失败。
#
# ---------------------------------------------------------------------------
# 为什么需要它（别把它当成 acceptance.sh 的复制品）
# ---------------------------------------------------------------------------
# scripts/acceptance.sh 验的是**宿主上直接跑的二进制**。镜像与它之间隔着一层
# 独立的失败面，acceptance.sh 一个都覆盖不到：
#
#   - scratch 基础镜像里什么都没有。二进制若意外动态链接，宿主上跑得好好的，
#     进了 scratch 立刻 "no such file or directory"（找不到 ld.so，报错信息还
#     具有极强的误导性 —— 看起来像二进制不存在）。
#   - 内置配置 configs/docker.yaml 被烤进镜像。它写错了（路径、字段名、
#     严格 YAML 下的未知字段）镜像就起不来，而这份文件没有任何单测覆盖。
#   - VOLUME /data 是共享路径存在的唯一保证：scratch 里没有 mkdir，
#     配置校验又要求共享目录必须已存在。这条链子断了就是启动即失败。
#   - 端口发布、ENTRYPOINT/CMD 拼接、以非 root 运行时能否绑 445 —— 全是
#     镜像特有的。
#
# 所以「裸包验过了」不等于「镜像可用」。发布双轨形态就得验两轨
# （memory: project_release_docker_image.md）。
#
# ---------------------------------------------------------------------------
# 判据必须可证伪
# ---------------------------------------------------------------------------
# 每个用例都要有明确的**反向对照**或不可伪造的证据，不能只看命令退出码 0：
#   - 写进去的内容要读回来逐字节比对（只看 put 成功等于没验）；
#   - 只读用例要确认写入**被拒**，而不只是确认读取成功；
#   - 客户端缺失时记 skip 并在汇总里列出来，绝不静默当成通过。
set -e

cd "$(dirname "$0")/.."

IMAGE=
# 默认端口取 4470：本项目多 agent 并行时调试端口是连号分配的（4451~446x），
# acceptance.sh 还会用 +1000/+2000/+3000 的偏移，落在 4470 上撞车概率最低。
# 撞上了脚本会明确报「端口已被占用」，不要靠改 sleep 硬扛。
PORT=${SMB_PORT:-4470}
NAME=stupidsamba-verify-$$

# pass/fail/skip 统一签名：<用例名> <说明>。
# 用例名进汇总（短、可 grep），说明只打在当行 —— 汇总里塞长句会没法读。
info() { printf '\033[1;36m==> %s\033[0m\n' "$1"; }
pass() { printf '  \033[1;32mPASS\033[0m  %-16s %s\n' "$1" "$2"; PASSED="$PASSED $1"; }
fail() { printf '  \033[1;31mFAIL\033[0m  %-16s %s\n' "$1" "$2" >&2; FAILED="$FAILED $1"; }
skip() { printf '  \033[1;33mSKIP\033[0m  %-16s %s\n' "$1" "$2"; SKIPPED="$SKIPPED $1"; }
die()  { printf '\033[1;31m错误: %s\033[0m\n' "$1" >&2; exit 1; }

PASSED=; FAILED=; SKIPPED=

while [ $# -gt 0 ]; do
    case "$1" in
        --image) IMAGE=$2; shift 2 ;;
        --port)  PORT=$2; shift 2 ;;
        -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
        *) die "未知参数: $1（--help 看用法）" ;;
    esac
done

[ -n "$IMAGE" ] || die "必须用 --image 指定镜像，例如 --image stupidsamba:v0.2.0"
command -v docker >/dev/null 2>&1 || die "找不到 docker"
docker image inspect "$IMAGE" >/dev/null 2>&1 || die "本地没有镜像 $IMAGE（先跑 scripts/docker-build.sh）"

# ---------------------------------------------------------------------------
# 连接凭据必须与**镜像内置的那份配置**同源
# ---------------------------------------------------------------------------
# configs/docker.yaml 在 9a07fef 里把「开箱即用」的载体从 guest 换成了内置
# 演示账号（原因见该文件头注释：Windows 10/11 默认拒绝不安全的 guest 登录，
# guest 配置对 Windows 用户等于连不上）。
#
# 本脚本当时仍用 smbclient -N 匿名连接，于是镜像明明是好的、用例却报
# NT_STATUS_LOGON_FAILURE —— 一个**假失败**。脚本与配置是两处，改一处必须
# 同步另一处，这正是 configs/docker.yaml 头部那段警告说的同一件事。
SMB_USER=${SMB_USER:-stupidsamba}
SMB_PASS=${SMB_PASS:-stupidsamba}

WORK=$(mktemp -d /tmp/stupidsamba-img.XXXXXX)
VOLUME=$NAME-data

# ---------------------------------------------------------------------------
# 为什么挂**命名卷**而不是 `-v /宿主目录:/data`
# ---------------------------------------------------------------------------
# 绑定挂载的路径是由 **docker daemon** 解析的，不是由本脚本所在的文件系统解析的。
# 只要 daemon 不和脚本在同一个 mount namespace（DinD、socket 接到外层 daemon、
# 远程 DOCKER_HOST —— 本项目的开发容器正是这种情况），`-v $PWD/x:/data` 就会
# 静默挂到 daemon 那一侧一个同名的空目录上：容器里写文件成功、SMB 也读得回来，
# 而脚本回头去看自己那个目录**空空如也**。
# 那样的用例只会稳定地假红，而且看起来像产品 bug（"数据没落到 /data"）。
#
# 命名卷完全在 daemon 侧，谁来跑都一样；要看内容就再起一个带 shell 的容器
# 挂同一个卷去看 —— 这也更接近 NAS 用户的真实用法。
VOLUME_PROBE_IMAGE=${VOLUME_PROBE_IMAGE:-alpine:latest}

cleanup() {
    rc=$?
    if [ "${KEEP:-}" = "1" ]; then
        printf '保留容器 %s、卷 %s、目录 %s（KEEP=1）\n' "$NAME" "$VOLUME" "$WORK"
    else
        docker rm -f "$NAME" >/dev/null 2>&1 || true
        docker volume rm "$VOLUME" >/dev/null 2>&1 || true
    fi
    exit $rc
}
trap cleanup EXIT INT TERM

# ------------------------------------------------------------------ 起容器
#
# 刻意发布在 127.0.0.1 上：内置配置开着 guest 匿名可写，
# 绑到 0.0.0.0 等于在局域网里开一个匿名可写共享（configs/docker.yaml 顶部有同样的告诫）。

info "启动容器 $NAME（$IMAGE，宿主 127.0.0.1:$PORT -> 容器 445）"
docker rm -f "$NAME" >/dev/null 2>&1 || true
if ! run_err=$(docker run -d --name "$NAME" \
        -p "127.0.0.1:$PORT:445" \
        -v "$VOLUME:/data" \
        "$IMAGE" 2>&1); then
    case "$run_err" in
        *"address already in use"*|*"port is already allocated"*)
            die "宿主端口 $PORT 已被占用（多 agent 并行时很常见）。换一个：--port <其他端口>" ;;
        *) die "docker run 失败: $run_err" ;;
    esac
fi

# ------------------------------------------------------------------ 等就绪
#
# 判据取服务端自己打的「SMB 服务已监听」这一行，不去探 TCP 端口，有两个理由：
#
#   1. 端口通了不等于服务就绪 —— docker 的端口转发在容器进程还没 bind 之前
#      就已经能接受连接（rootless 模式下尤其明显），探到的是转发层不是服务。
#      日志这一行是服务端在 Listen 成功之后才打的，是**语义**就绪信号。
#   2. `/dev/tcp/...` 是 bashism。本文件的 shebang 是 `#!/bin/sh`，
#      在 dash 上那个重定向直接失败；更坏的是 `exec 3>&- 2>/dev/null` 这种
#      写法会把**整个脚本**的 stderr 永久重定向到 /dev/null，之后所有 fail()
#      消息连同非零退出的原因一起消失 —— 本脚本第一版就栽在这上面：
#      smbclient 用例明明失败了，输出里既没有 FAIL 也没有汇总，只剩一个
#      光秃秃的 rc=1。别再把 /dev/tcp 写回来。
READY='SMB 服务已监听'
i=0
while [ "$i" -lt 150 ]; do
    if docker logs "$NAME" 2>&1 | grep -q "$READY"; then break; fi
    if [ "$(docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null)" != "true" ]; then
        printf '\033[1;31m容器已退出，日志如下:\033[0m\n' >&2
        docker logs "$NAME" 2>&1 | sed 's/^/    /' >&2
        die "容器启动失败（内置配置 configs/docker.yaml 可能有问题）"
    fi
    i=$((i + 1)); sleep 0.1
done
if [ "$i" -ge 150 ]; then
    docker logs "$NAME" 2>&1 | sed 's/^/    /' >&2
    die "等待「$READY」超时（15 秒）"
fi

pass "startup" "容器启动并监听"

# ------------------------------------------------------------------ 用例 1：scratch 里二进制真的能跑
#
# 这是 scratch 镜像最容易踩的坑：动态链接的二进制在 scratch 里会报
# "no such file or directory"（找不到 ld.so，而不是找不到二进制）。
# 服务已经监听住了其实就间接证明了这一点，这里再取一次 -version 作为直接证据。

vout=$(docker run --rm "$IMAGE" -version 2>&1) \
    && pass "static-exec" "scratch 中可执行: $vout" \
    || fail "static-exec" "$vout"

# ------------------------------------------------------------------ 用例 2：smbclient 读写往返

if command -v smbclient >/dev/null 2>&1; then
    payload="hello-from-verify-image-$$"
    printf '%s' "$payload" > "$WORK/up.txt"

    # ⚠️ smbclient 4.22 的 -c 不按换行分割命令，多条命令必须用**分号**分隔，
    #    写成多行会得到 "NT_STATUS_NO_SUCH_FILE listing \get" 这类假故障。
    # -U 用内置演示账号（见脚本上文的凭据说明）；-N 匿名会被拒绝。
    out=$(smbclient "//127.0.0.1/public" -p "$PORT" \
              -U "$SMB_USER%$SMB_PASS" -m SMB3 \
              -c "put $WORK/up.txt up.txt; mkdir subdir; ls; get up.txt $WORK/down.txt" 2>&1) || true

    if [ ! -f "$WORK/down.txt" ]; then
        fail "smbclient" "取回文件不存在。smbclient 输出: $out"
    elif [ "$(cat "$WORK/down.txt")" != "$payload" ]; then
        fail "smbclient" "往返内容不一致：写入 '$payload'，读回 '$(cat "$WORK/down.txt")'"
    else
        pass "smbclient" "读写往返内容逐字节一致"
    fi

    # 反向对照：不存在的共享必须被拒。若它也"成功"，说明上面的 PASS 不可信。
    if smbclient "//127.0.0.1/nosuchshare" -p "$PORT" \
            -U "$SMB_USER%$SMB_PASS" -m SMB3 -c "ls" >/dev/null 2>&1; then
        fail "smbclient-neg" "连接不存在的共享 nosuchshare 竟然成功了，说明测试没有鉴别力"
    else
        pass "smbclient-neg" "反向对照：不存在的共享被拒"
    fi

    # ---------------------------------------------------------------- 落盘位置
    #
    # SMB 往返成功只证明"服务端记住了这些字节"，不证明它们落在了 /data。
    # 若共享路径被写错、或数据落进了容器可写层，用户 `docker rm` 之后数据就没了，
    # 而上面的往返用例一个都不会红。所以必须从**卷**这一侧独立看一眼。
    if docker image inspect "$VOLUME_PROBE_IMAGE" >/dev/null 2>&1 \
       || docker pull -q "$VOLUME_PROBE_IMAGE" >/dev/null 2>&1; then
        pout=$(docker run --rm -v "$VOLUME:/data" "$VOLUME_PROBE_IMAGE" \
                   sh -c 'cat /data/up.txt 2>/dev/null; echo; [ -d /data/subdir ] && echo SUBDIR' 2>&1)
        case "$pout" in
            *"$payload"*)
                case "$pout" in
                    *SUBDIR*) pass "volume-persist" "写入的文件与目录确实落在卷 /data 上" ;;
                    *) fail "volume-persist" "卷上有 up.txt 但没有 mkdir 建的 subdir：$pout" ;;
                esac ;;
            *) fail "volume-persist" "卷 $VOLUME 的 /data 里读不到写入的内容：$pout" ;;
        esac
    else
        skip "volume-persist" "拉不到探针镜像 $VOLUME_PROBE_IMAGE，无法从卷侧核对落盘位置"
    fi
else
    skip "smbclient" "未安装（apt-get install smbclient）"
fi

# ------------------------------------------------------------------ 用例 3：impacket（第二种独立客户端栈）

if python3 -c "import impacket" 2>/dev/null; then
    out=$(SMB_HOST=127.0.0.1 SMB_PORT="$PORT" SMB_USER="$SMB_USER" SMB_PASS="$SMB_PASS" \
         python3 - "$PORT" <<'PYEOF' 2>&1
import os
import sys
from impacket.smbconnection import SMBConnection

port = int(sys.argv[1])
# 用内置演示账号登录（见脚本上文的凭据说明）；匿名会被拒。
user = os.environ["SMB_USER"]
password = os.environ["SMB_PASS"]
c = SMBConnection("127.0.0.1", "127.0.0.1", sess_port=port)
c.login(user, password)
shares = [s["shi1_netname"][:-1] for s in c.listShares()]
assert "public" in shares, "共享列表里没有 public: %r" % (shares,)

payload = b"impacket-roundtrip"
import io
c.putFile("public", "impacket.txt", io.BytesIO(payload).read)
buf = io.BytesIO()
c.getFile("public", "impacket.txt", buf.write)
assert buf.getvalue() == payload, "往返内容不一致: %r" % (buf.getvalue(),)

print("dialect=%s shares=%s" % (hex(c.getDialect()), shares))
c.close()
PYEOF
    ) && pass "impacket" "枚举共享 + 读写往返（$out）" \
      || fail "impacket" "$out"
else
    skip "impacket" "未安装（apt-get install python3-impacket）"
fi

# ------------------------------------------------------------------ 用例 4：内置配置的关键约定
#
# 这些是 configs/docker.yaml 承诺过的行为，写死在这里当回归保护。

logs=$(docker logs "$NAME" 2>&1)

# guest 开着时服务端必须打 WARN —— AGENTS.md §8「开启 guest 必须在日志里明确警告」。
case "$logs" in
    *[Gg]uest*) pass "guest-warn" "启动日志有 guest 警告（§8 要求）" ;;
    *) fail "guest-warn" "启动日志里找不到任何 guest 相关字样，§8 的警告要求没兑现" ;;
esac

# mDNS 在桥接网络下应当是关的（configs/docker.yaml 的刻意选择：组播出不了
# docker0 网桥，就算出得去广播的也是容器内的 172.17.x.x，客户端照着连必然失败）。
#
# 判据取**服务端明说自己没启用**的那一行，而不是「日志里没出现 mDNS 启动字样」。
# 后者是不可证伪的：内置配置整个被换掉、mdns 模块被删、日志级别调高……
# 任何一种情况下它都照样"通过"。
case "$logs" in
    *"mDNS 未启用"*) pass "mdns-off" "mDNS 默认关闭（服务端明确记录未启用）" ;;
    *) fail "mdns-off" "日志里没有「mDNS 未启用」这一行，内置配置的 mdns.enabled:false 可能失效" ;;
esac

# ------------------------------------------------------------------ 用例 5：manifest 里不得出现 unknown/unknown 平台
#
# 这是 buildx 默认未禁用 attestation 时的典型产物：推上去的 manifest list 除了
# 真正的 linux/amd64 / linux/arm64 之外，还会多两条 Platform 为 "unknown/unknown"
# 的 attestation-manifest（provenance + SBOM）。它们不是真实可运行架构，却会让
# 某些 registry 客户端把镜像当成「多架构里混进了未知平台」，也违背我们
# 「镜像只含真实可运行架构」的承诺。docker-build.sh 已显式加
# --provenance=false --sbom=false 关掉它们。
#
# 反向对照（falsifiable，必须满足）：
#   - 在 v0.2.0-rc0（未禁用 attestation）的镜像上，本用例必须 FAIL；
#   - 在补上 --provenance=false --sbom=false 重建的镜像上，本用例必须 PASS。
# 一个永远 PASS 的断言和没有断言是一回事——所以这条必须有上述两端的证据。

info "检查 manifest 是否混入 unknown/unknown 平台"
probe="$IMAGE"

tmp_manifest=/tmp/verify-manifest.$$
manifest_ok=0
# 优先问 registry 的 manifest list（我们想验的就是远端产物）；
# 带不出版本信息时退回 docker manifest inspect；都失败则跳过本用例。
#
# ⚠️ 不能直接用「tmp_manifest 非空」当成功标志：inspect 失败时错误文本也会被
# 重定向进同一个文件，于是文件非空 → 落到下面被误判成 pass（假绿洞）。
# 必须用显式 manifest_ok 区分「真拿到 manifest」与「只拿到一段报错」。
if docker buildx imagetools inspect "$probe" >"$tmp_manifest" 2>&1; then
    manifest_ok=1
elif docker manifest inspect "$probe" >"$tmp_manifest" 2>&1; then
    manifest_ok=1
else
    skip "manifest-clean" "imagetools/manifest inspect 无法解析 $probe（本地非 --push 构建？）"
fi

if [ "$manifest_ok" = 1 ]; then
    if grep -q 'unknown/unknown' "$tmp_manifest"; then
        fail "manifest-clean" "manifest 出现 unknown/unknown 平台（未禁用 provenance/SBOM attestation）"
    else
        pass "manifest-clean" "manifest 仅含真实可运行架构（无 unknown/unknown）"
    fi
fi
rm -f "$tmp_manifest"

# ------------------------------------------------------------------ 汇总

printf '\n'
info "汇总"
# ⚠️ 这里全部写成 if 而不是 `[ ... ] && printf`：后者在条件为假时整条命令返回 1，
#    在 `set -e` 下会让脚本**当场以 rc=1 退出**，什么都不打印。
#    本脚本第一版就是这么"失败"的 —— 用例都跑完了，退出码却是 1 且没有汇总。
if [ -n "$PASSED" ]; then
    printf '  通过:%s\n' "$PASSED"
fi
if [ -n "$SKIPPED" ]; then
    printf '  跳过:%s\n' "$SKIPPED"
fi
if [ -n "$FAILED" ]; then
    printf '  \033[1;31m失败:%s\033[0m\n' "$FAILED" >&2
    printf '容器日志：\n' >&2
    docker logs "$NAME" 2>&1 | sed 's/^/    /' >&2
    exit 1
fi

# 跳过的客户端要在汇总里显式提醒，避免「全绿」被读成「全验过」。
if [ -n "$SKIPPED" ]; then
    printf '\033[1;33m注意：上述 SKIP 项未验证，不要当作通过。\033[0m\n'
fi
info "镜像 $IMAGE 端到端验收通过"
