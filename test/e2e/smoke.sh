#!/bin/sh
# smoke.sh —— 三客户端端到端冒烟套件（AGENTS.md §3 硬性验收门槛）
#
# 与 scripts/acceptance.sh 的分工：
#   acceptance.sh 是**广度**验收（方言矩阵/签名/加密/guest/只读/认证失败），跑得慢；
#   本脚本是**深度**冒烟：只做最核心的 7 个文件操作，但每个操作的判据都做到
#   「服务端磁盘上的事实」这一级，跑得快，适合每次改动后反复跑、以及进 CI。
#
# ---------------------------------------------------------------------------
# 判据设计（这是本脚本存在的理由，改之前先读完）
# ---------------------------------------------------------------------------
# 教训：曾经有验收用「命令退出码为 0」「grep 到某个子串」当判据，结果漏掉了
# 服务端「返回 STATUS_SUCCESS 但什么都没做」这一整类 bug（同形态事故见
# memory/project_silent_success_failures.md）。
#
# 所以本脚本遵守两条铁律：
#
#   铁律一：**客户端只负责发操作，判据由驱动脚本在服务端磁盘上独立核对。**
#           客户端说「删除成功」不算数，驱动去 $SHARE 下看文件还在不在才算数。
#           这样三个客户端共享同一套判据强度，不受各客户端库诚实程度影响。
#
#   铁律二：**内容比对一律用 cmp 做整字节比对，不用 grep 子串。**
#           子串匹配对「首尾被改了一个字节」「文件被截断」完全无感。
#           载荷是 /dev/urandom 生成的非对齐大小（1 MiB + 4097 B），
#           既跨多个 SMB READ/WRITE 分块，又能暴露 off-by-one。
#
# 每条判据都必须能回答「服务端这块坏了它会不会红」。答案由
# test/e2e/reverse-control.sh 用故意改坏的服务端二进制**实测**给出，
# 不是靠嘴说。新增判据时请同步在那里加一个变异对照。
#
# ---------------------------------------------------------------------------
# 用法
#   test/e2e/smoke.sh                    跑全部三个客户端
#   test/e2e/smoke.sh smbclient          只跑指定客户端（smbclient/impacket/gosmb2）
#
# 环境变量
#   SMB_PORT   监听端口，默认 4463
#   MUTATE     变异名（见 test/e2e/mutate.sh）。设了就用**故意改坏的**服务端跑，
#              供 reverse-control.sh 做失败对照。日常使用不要设。
#   KEEP=1     保留临时目录与服务进程，便于手工调试
#
# 输出契约（reverse-control.sh 依赖，不要随意改）
#   最后两行固定为：
#     SMOKE-FAILED: <空格分隔的失败判据 ID>
#     SMOKE-RESULT: PASS|FAIL
# ---------------------------------------------------------------------------

cd "$(dirname "$0")/../.." || exit 1
ROOT=$(pwd)
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

PORT=${SMB_PORT:-4463}
USER=${SMB_USER:-smoketest}
PASS=${SMB_PASS:-smokepass123}
ONLY=$1

# 二进制名必须 ≤15 字符：超过后内核 comm 字段被截断，pkill -x 静默匹配不到，
# 旧进程占着端口不放，表现和「服务起不起来」一模一样（AGENTS.md §10.3 第 4 条）。
BIN_NAME=sssmoke

WORK=$(mktemp -d /tmp/stupidsamba-smoke.XXXXXX) || exit 1
SHARE="$WORK/share"
LOG="$WORK/server.log"
SRVPID=""

PASSED_N=0
FAILED_IDS=""

cleanup() {
    if [ -n "$SRVPID" ] && kill -0 "$SRVPID" 2>/dev/null; then
        kill "$SRVPID" 2>/dev/null
        # 给优雅退出一点时间，再补一刀。
        _i=0
        while [ $_i -lt 25 ] && kill -0 "$SRVPID" 2>/dev/null; do
            _i=$((_i + 1)); sleep 0.2
        done
        kill -0 "$SRVPID" 2>/dev/null && kill -9 "$SRVPID" 2>/dev/null
    fi
    # 兜底：按**精确进程名**收尾。绝不能用 pkill -f <路径>，那个模式会匹配到
    # 执行它的 shell 自己的命令行，把父 shell 一起杀掉（AGENTS.md §10.3 第 4 条）。
    pkill -x "$BIN_NAME" 2>/dev/null
    if [ "$KEEP" = "1" ]; then
        echo "保留临时目录: $WORK"
    else
        rm -rf "$WORK"
    fi
}
trap cleanup EXIT INT TERM

say()  { printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"; }
pass() { printf '\033[1;32m  [PASS]\033[0m %s\n' "$1"; PASSED_N=$((PASSED_N + 1)); }
fail() { printf '\033[1;31m  [FAIL]\033[0m %s — %s\n' "$1" "$2"; FAILED_IDS="$FAILED_IDS $1"; }
skip() { printf '\033[1;33m  [SKIP]\033[0m %s (%s)\n' "$1" "$2"; }
die()  { printf '\033[1;31m致命: %s\033[0m\n' "$1"; echo "SMOKE-FAILED: setup"; echo "SMOKE-RESULT: FAIL"; exit 1; }

# ---------------------------------------------------------------- 构建

say "构建"

BUILD_FLAGS=""
if [ -n "$MUTATE" ]; then
    OVERLAY=$(sh "$ROOT/test/e2e/mutate.sh" "$MUTATE" "$WORK") || die "变异 $MUTATE 生成失败"
    BUILD_FLAGS="-overlay $OVERLAY"
    printf '\033[1;35m  *** 变异模式: %s（这是失败对照，不是正常运行）***\033[0m\n' "$MUTATE"
fi

# shellcheck disable=SC2086
go build $BUILD_FLAGS -o "$WORK/$BIN_NAME" ./cmd/stupidsamba || die "go build 失败"

# 端口探针。用 Go 写是因为本容器的 sh 是 dash，没有 /dev/tcp。
cat > "$WORK/probe.go" <<'GOEOF'
package main

import (
	"net"
	"os"
	"time"
)

func main() {
	c, err := net.DialTimeout("tcp", os.Args[1], 800*time.Millisecond)
	if err != nil {
		os.Exit(1)
	}
	c.Close()
}
GOEOF
go build -o "$WORK/probe" "$WORK/probe.go" || die "端口探针构建失败"
echo "  OK"

# ---------------------------------------------------------------- 共享目录与配置

say "准备共享目录"
mkdir -p "$SHARE/subdir" || die "创建共享目录失败"
echo "nested" > "$SHARE/subdir/nested.txt"

# ---------------------------------------------------------------------------
# 夹具设计：**每个操作一份专属夹具，谁都不吃谁的证据。**
#
# 第一版这里栽过一个大跟头，记下来免得重蹈：当时让客户端 put 出 up-P.bin，
# 再把它 rename 走、再 rm 掉，最后才统一核对。结果 put/rename 的判据全红 ——
# 不是服务端有问题，是**证据被后面的操作自己毁掉了**（文件确实被搬走删掉了，
# 于是「put 出来的文件应当在」永远不成立）。这类自毁夹具是端到端测试里
# 最常见的假红来源。
#
# 现在的规矩：
#   - 会被搬走的（rename）、会被删掉的（rm/rmdir），夹具由**驱动预先在磁盘上
#     造好**，客户端只负责搬/删。这样「它不见了」是一条**非空断言** ——
#     东西本来实实在在存在过。
#   - 只有 put 的产物由客户端创造，且创造完不再被任何操作碰。
#
# 载荷大小取 1 MiB + 4097：既跨多个 READ/WRITE 分块（暴露分块与 credit 处理），
# 又不是 4K/64K 的整数倍（暴露 off-by-one 与尾块处理）。
# ---------------------------------------------------------------------------
PAYLOAD_SIZE=$((1048576 + 4097))
for p in smbclient impacket gosmb2; do
    # get 的源：躺在服务端磁盘上，客户端下载后驱动做整字节比对。
    head -c "$PAYLOAD_SIZE" /dev/urandom > "$SHARE/seed-$p.bin" || die "生成 seed 失败"
    # put 的源：躺在 workdir，客户端上传后驱动比对服务端磁盘上的落地文件。
    head -c "$PAYLOAD_SIZE" /dev/urandom > "$WORK/up-$p.bin"    || die "生成 up 失败"
    # rename 的源：驱动直接在服务端磁盘上造好，workdir 里留一份参考副本，
    # 用来验证「搬完之后内容一个字节都没变」。
    head -c "$PAYLOAD_SIZE" /dev/urandom > "$WORK/mov-$p.bin"   || die "生成 mov 失败"
    cp "$WORK/mov-$p.bin" "$SHARE/mov-$p.bin"                   || die "布置 mov 失败"
    # rm / rmdir 的对象：同样预先造好，客户端删完之后「不见了」才有意义。
    head -c 1024 /dev/urandom > "$SHARE/del-$p.bin"             || die "布置 del 失败"
    mkdir -p "$SHARE/rmd-$p"                                    || die "布置 rmd 失败"
done
echo "  载荷大小 $PAYLOAD_SIZE 字节；每客户端 4 份夹具"

cat > "$WORK/config.yaml" <<EOF
server:
  name: SMOKESRV
  domain: WORKGROUP
  # impacket 的 SMBConnection 默认走 negotiateSessionWildcard（先发 SMB1
  # 多协议协商再升级到 SMB2），不开这个入口它连不上。这里只是**协商入口**，
  # 不提供任何 SMB1 文件操作。
  smb1: true
  min_dialect: "2.0.2"
  max_dialect: "3.1.1"
listen:
  addresses:
    - 127.0.0.1
  port: $PORT
auth:
  allow_guest: false
  users:
    - name: $USER
      password: "$PASS"
shares:
  - name: smoke
    path: $SHARE
    read_only: false
mdns:
  enabled: false
log:
  level: info
EOF

# ---------------------------------------------------------------- 启动服务

say "启动服务 (127.0.0.1:$PORT)"

# 先确认端口是空的。不然我们 bind 失败、进程立刻退出，而探针照样能连上
# **别人的**服务 —— 整轮冒烟就在测别人的实现。这个坑项目里真踩过。
if "$WORK/probe" "127.0.0.1:$PORT" 2>/dev/null; then
    echo "  端口 127.0.0.1:$PORT 已被占用："
    fuser "$PORT"/tcp 2>&1 | sed 's/^/    /'
    die "换个端口重跑：SMB_PORT=<其它端口> $0"
fi

# 直接用 & 让服务成为本 shell 的**直接子进程**，$! 就是监听进程本身。
# （§10.3 第 5 条那个「$! 不是监听进程」的坑说的是 setsid nohup 的写法。）
"$WORK/$BIN_NAME" -config "$WORK/config.yaml" > "$LOG" 2>&1 &
SRVPID=$!

i=0
while :; do
    if ! kill -0 "$SRVPID" 2>/dev/null; then
        sed 's/^/    /' "$LOG"
        die "服务启动即退出"
    fi
    if "$WORK/probe" "127.0.0.1:$PORT" 2>/dev/null; then
        break
    fi
    i=$((i + 1))
    [ $i -ge 50 ] && { sed 's/^/    /' "$LOG"; die "服务未在 10s 内监听 $PORT"; }
    sleep 0.2
done
echo "  已监听 (pid $SRVPID)"

# ---------------------------------------------------------------- 判据原语
#
# 全部作用在**服务端磁盘**上，与客户端说了什么无关。

# same_bytes <id> <文件A> <文件B> —— 整字节比对
same_bytes() {
    if [ ! -f "$2" ]; then fail "$1" "缺少文件 $2"; return 1; fi
    if [ ! -f "$3" ]; then fail "$1" "缺少文件 $3"; return 1; fi
    if cmp -s "$2" "$3"; then
        pass "$1"
    else
        fail "$1" "字节不一致 ($(wc -c < "$2") vs $(wc -c < "$3") 字节；首个差异: $(cmp "$2" "$3" 2>&1 | head -1))"
        return 1
    fi
}

exists_file() {
    if [ -f "$2" ]; then pass "$1"; else fail "$1" "服务端磁盘上没有文件 $2"; return 1; fi
}
exists_dir() {
    if [ -d "$2" ]; then pass "$1"; else fail "$1" "服务端磁盘上没有目录 $2"; return 1; fi
}
absent() {
    if [ ! -e "$2" ]; then pass "$1"; else fail "$1" "$2 仍存在于服务端磁盘（服务端很可能只回了 SUCCESS 却没真做）"; return 1; fi
}

# ---------------------------------------------------------------- 客户端驱动
#
# 约定：每个客户端脚本收到统一参数
#   <host> <port> <user> <pass> <share> <workdir> <前缀>
# 并按同一顺序执行 7 个操作。它自己只对**目录枚举**做断言（磁盘看不出
# 客户端有没有看见），其余判据一律由本驱动在下面独立核对。

run_case() {
    P=$1          # 前缀，同时是客户端名
    shift
    if [ -n "$ONLY" ] && [ "$ONLY" != "$P" ]; then
        return 0
    fi
    say "客户端: $P"

    rc=0
    timeout 120 "$@" 127.0.0.1 "$PORT" "$USER" "$PASS" smoke "$WORK" "$P" || rc=$?
    if [ "$rc" = 77 ]; then
        skip "$P/*" "环境缺少该客户端"
        return 0
    fi
    if [ "$rc" != 0 ]; then
        fail "$P/client-run" "客户端脚本退出码 $rc（协议层就没跑通，下面的磁盘判据仅供参考）"
    else
        pass "$P/client-run"
    fi

    # --- 判据 2: get 下来的字节必须与服务端磁盘上的种子文件完全一致
    same_bytes "$P/get-bytes" "$SHARE/seed-$P.bin" "$WORK/got-$P.bin"

    # --- 判据 3: put 上去的字节必须原样落在服务端磁盘上
    #     这一条最值钱：它管的是「服务端收下了但写歪了/写少了」。
    same_bytes "$P/put-bytes" "$WORK/up-$P.bin" "$SHARE/up-$P.bin"

    # --- 判据 4: mkdir 必须在磁盘上真的建出目录
    exists_dir "$P/mkdir-disk" "$SHARE/dir-$P"

    # --- 判据 5: rename 必须真的搬动 —— 旧名消失、新名出现、且内容一字未改。
    #     夹具是驱动预先放好的，所以「旧名消失」是非空断言。
    absent      "$P/rename-old-gone" "$SHARE/mov-$P.bin"
    exists_file "$P/rename-new-disk" "$SHARE/dir-$P/moved-$P.bin"
    same_bytes  "$P/rename-bytes"    "$WORK/mov-$P.bin" "$SHARE/dir-$P/moved-$P.bin"

    # --- 判据 6/7: 删除必须真的从磁盘上消失（夹具同样是预先放好的）
    absent "$P/rm-disk"    "$SHARE/del-$P.bin"
    absent "$P/rmdir-disk" "$SHARE/rmd-$P"
}

run_case smbclient sh      "$ROOT/test/e2e/client_smbclient.sh"
run_case impacket  python3 "$ROOT/test/e2e/client_impacket.py"
run_case gosmb2    sh      "$ROOT/test/e2e/client_gosmb2.sh"

# ---------------------------------------------------------------- 优雅关服务

say "关闭服务"
kill "$SRVPID" 2>/dev/null
i=0
while [ $i -lt 50 ] && kill -0 "$SRVPID" 2>/dev/null; do
    i=$((i + 1)); sleep 0.2
done
if kill -0 "$SRVPID" 2>/dev/null; then
    fail "server/graceful-exit" "收到 SIGTERM 后 10s 内没退出"
    kill -9 "$SRVPID" 2>/dev/null
else
    pass "server/graceful-exit"
fi
SRVPID=""

# 服务端日志里不该有 panic —— 有的话就算所有判据都绿也必须红。
if grep -q "panic:" "$LOG" 2>/dev/null; then
    fail "server/no-panic" "服务端日志里出现 panic"
    grep -n -A5 "panic:" "$LOG" | head -20 | sed 's/^/    /'
else
    pass "server/no-panic"
fi

# ---------------------------------------------------------------- 汇总

say "汇总"
echo "  通过 $PASSED_N 项"
if [ -n "$FAILED_IDS" ]; then
    printf '\033[1;31m  失败:%s\033[0m\n' "$FAILED_IDS"
fi
echo "SMOKE-FAILED:$FAILED_IDS"
if [ -n "$FAILED_IDS" ]; then
    echo "SMOKE-RESULT: FAIL"
    exit 1
fi
echo "SMOKE-RESULT: PASS"
exit 0
