#!/bin/sh
# acceptance.sh —— 多客户端验收测试驱动（AGENTS.md §3）
#
# 硬性验收门槛：至少三种不同的第三方 SMB 客户端测试通过。
#
# 用法：
#   scripts/acceptance.sh              # 跑全部用例
#   scripts/acceptance.sh smbclient    # 只跑指定用例
#
# 用例名：smbclient / impacket / gosmb2 / mountcifs /
#         dialects / signing / encryption / readonly / authfail / guest
#
# 环境变量：
#   SMB_PORT   主服务端口，默认 4445（非特权，便于无 root 环境跑）
#   SMB_PORT_GUEST   guest 服务端口，默认 SMB_PORT+1000
#   SMB_PORT_STRICT  强签名/强加密服务端口，默认 SMB_PORT+2000
#   SMB_PORT_ENC     强制加密但**允许低方言接入**的服务端口，默认 SMB_PORT+3000
#   SMB_PORT_TAP     线级探针 smbtap 的监听端口，默认 SMB_PORT+4000
#              偏移取 1000/2000 而不是 +1/+2，是因为多 agent 并行开发时
#              大家的调试端口是连号分配的（4461、4462…），+1/+2 会直接
#              撞到隔壁 agent 的服务上 —— 已经真踩过一次。
#   SMB_USER   测试用户，默认 testuser
#   SMB_PASS   测试口令，默认 testpass123
#   KEEP       设为 1 时保留临时目录与服务进程，便于手工调试
set -e

cd "$(dirname "$0")/.."
ROOT=$(pwd)
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

PORT=${SMB_PORT:-4445}
PORT_GUEST=${SMB_PORT_GUEST:-$((PORT + 1000))}
PORT_STRICT=${SMB_PORT_STRICT:-$((PORT + 2000))}
PORT_ENC=${SMB_PORT_ENC:-$((PORT + 3000))}
PORT_TAP=${SMB_PORT_TAP:-$((PORT + 4000))}
USER=${SMB_USER:-testuser}
PASS=${SMB_PASS:-testpass123}
ONLY=$1

WORK=$(mktemp -d /tmp/stupidsamba-acc.XXXXXX)
SHARE="$WORK/share"
RO="$WORK/readonly"
LOG="$WORK/server.log"
LOG_STRICT="$WORK/server-strict.log"
LOG_ENC="$WORK/server-enc.log"
SRVPIDS=""

PASSED=""
FAILED=""

cleanup() {
    for p in $SRVPIDS; do
        if kill -0 "$p" 2>/dev/null; then
            kill "$p" 2>/dev/null || true
            wait "$p" 2>/dev/null || true
        fi
    done
    if [ "$KEEP" = "1" ]; then
        echo "保留临时目录: $WORK"
    else
        rm -rf "$WORK"
    fi
}
trap cleanup EXIT INT TERM

say() { printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"; }
ok()  { printf '\033[1;32m  [PASS] %s\033[0m\n' "$1"; PASSED="$PASSED $1"; }
bad() { printf '\033[1;31m  [FAIL] %s\033[0m\n' "$1"; FAILED="$FAILED $1"; }

# ---------------------------------------------------------------- 准备

say "构建"
go build -o "$WORK/stupidsamba" ./cmd/stupidsamba
echo "  OK"

say "准备共享目录与配置"
mkdir -p "$SHARE/subdir" "$RO"
echo "hello from stupidsamba" > "$SHARE/hello.txt"
head -c 1048576 /dev/urandom > "$SHARE/blob.bin"
echo "nested" > "$SHARE/subdir/nested.txt"
echo "read only content" > "$RO/ro.txt"

# 主服务：不开 guest，不强制签名/加密。用来跑功能面与"该拒绝的要拒绝"。
cat > "$WORK/config.yaml" <<EOF
server:
  name: STUPIDSAMBA
  domain: WORKGROUP
  # impacket 的 SMBConnection 默认走 negotiateSessionWildcard，
  # 会先发 SMB1 多协议协商再升级到 SMB2。impacket 是 AGENTS.md §3
  # 必测矩阵的第 3 项，所以验收环境必须开这个入口。
  # 注意：这里只是**协商入口**，不提供任何 SMB1 文件操作。
  smb1: true
  # 显式写死方言范围，让 dialects 用例的断言有确定的预期
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
  - name: public
    path: $SHARE
    read_only: false
  - name: ro
    path: $RO
    read_only: true
mdns:
  enabled: false
log:
  level: debug
EOF

# guest 服务：验证匿名访问在**显式开启**时可用。
# 单独起一个实例，是因为 allow_guest 是全局开关，
# 而主服务必须保持 allow_guest=false 才能测"匿名应被拒绝"。
cat > "$WORK/config-guest.yaml" <<EOF
server:
  name: STUPIDGUEST
  domain: WORKGROUP
  smb1: true
listen:
  addresses:
    - 127.0.0.1
  port: $PORT_GUEST
auth:
  allow_guest: true
  users:
    - name: $USER
      password: "$PASS"
shares:
  - name: pub
    path: $SHARE
    read_only: false
    guest_ok: true
mdns:
  enabled: false
log:
  level: debug
EOF

# 强制签名 + 强制加密服务：验证 SMB3 安全特性真的被要求，而不只是"支持"。
# 强制加密要求 max_dialect >= 3.0。
cat > "$WORK/config-strict.yaml" <<EOF
server:
  name: STUPIDSTRICT
  domain: WORKGROUP
  smb1: true
  min_dialect: "3.0"
  max_dialect: "3.1.1"
  signing_required: true
  encryption_required: true
listen:
  addresses:
    - 127.0.0.1
  port: $PORT_STRICT
auth:
  allow_guest: false
  users:
    - name: $USER
      password: "$PASS"
shares:
  - name: secure
    path: $SHARE
    read_only: false
mdns:
  enabled: false
log:
  level: debug
EOF

# 强制加密、但 min_dialect 故意放到 2.0.2 的服务。
#
# 这个配置存在的唯一理由是**回归**：曾经的缺口是客户端一句
# `smbclient -m SMB2_10` 把方言压到 3.1.1 以下，加密算法就协商不出来，
# 而 encryption_required 只在"协商出了算法"时才生效 —— 于是强制加密被
# 静默旁路，全程明文，服务端既不拒绝也不告警。
#
# config-strict 里 min_dialect 是 3.0，低方言在**方言选择**那一层就被挡了，
# 根本走不到加密那段代码，测不出这个缺口。必须把门开到 2.0.2，让客户端
# 真的能协商到 2.0.2/2.1，才能验证"加密强制自己"会不会 fail closed。
#
# 服务端启动时会为这个组合打一条 WARN（配置与行为不一致的提示），这是预期的。
cat > "$WORK/config-enc.yaml" <<EOF
server:
  name: STUPIDENC
  domain: WORKGROUP
  smb1: true
  min_dialect: "2.0.2"
  max_dialect: "3.1.1"
  # 刻意不强制签名：把"加密"这一个属性单独隔离出来测，
  # 否则用例失败时分不清是签名挡的还是加密挡的。
  signing_required: false
  encryption_required: true
listen:
  addresses:
    - 127.0.0.1
  port: $PORT_ENC
auth:
  allow_guest: false
  users:
    - name: $USER
      password: "$PASS"
shares:
  - name: secure
    path: $SHARE
    read_only: false
mdns:
  enabled: false
log:
  level: debug
EOF
echo "  OK ($WORK/config.yaml, config-guest.yaml, config-strict.yaml, config-enc.yaml)"

# 反向配置二：只写 encryption_required: true、**不写 min_dialect**（默认 2.0.2）。
# 自 b7ef0ed 起，默认 min_dialect < 3.0 同样 fail fast。这是 audit 特意点名的
# 默认值陷阱——只写 encryption_required 的极简配置现在也必须显式给 min_dialect。
cat > "$WORK/config-enc-default.yaml" <<EOF
server:
  name: STUPIDENCD
  domain: WORKGROUP
  smb1: true
  # 注意：故意不写 min_dialect，让它落到默认 "2.0.2"
  max_dialect: "3.1.1"
  encryption_required: true
listen:
  addresses:
    - 127.0.0.1
  port: $PORT_ENC
auth:
  allow_guest: false
  users:
    - name: $USER
      password: "$PASS"
shares:
  - name: secure
    path: $SHARE
    read_only: false
mdns:
  enabled: false
log:
  level: debug
EOF

# 用 Go 编译一个无外部依赖的端口探针（不依赖 python3 / nc 等，
# 因为这些工具在重启后的环境里经常缺失）
cat > "$WORK/probe.go" <<'GOEOF'
package main
import ("net";"os")
func main(){
  c,err:=net.DialTimeout("tcp",os.Args[1],800*1000*1000)
  if err!=nil{os.Exit(1)}
  c.Close()
}
GOEOF
go build -o "$WORK/probe" "$WORK/probe.go" || { echo "  端口探针编译失败"; exit 1; }

# 线级探针：加密到底有没有生效，只有看网线上的字节才算数。见该文件头部注释。
go build -o "$WORK/smbtap" ./scripts/clients/smbtap || { echo "  smbtap 编译失败"; exit 1; }

need_tool() {
    command -v "$1" >/dev/null 2>&1 || { echo "  跳过：缺少工具 $1"; return 77; }
    return 0
}

# start_server <配置文件> <端口> <日志文件>
# 启动一个实例并等到它真的在监听为止。pid 记进 SRVPIDS 供 cleanup 统一回收。
#
# 这里直接用 `&` 而不是 setsid：服务是本 shell 的**直接子进程**，
# $! 就是监听进程本身，kill 得掉。（AGENTS.md §10.3 里 `$! 不是监听进程`
# 那个坑说的是 `setsid nohup ... &` 的写法，此处不适用。）
start_server() {
    _cfg=$1; _port=$2; _log=$3
    # 先确认端口是空的。不然 bind 会失败、进程立刻退出，而探针照样能连上
    # **别人的**服务，整轮验收就在测别人的实现 —— 这个坑真踩过：
    # 隔壁 agent 的 ssdocs4467 占着端口，我们的服务起不来却一路"绿"到方言断言才暴露。
    if "$WORK/probe" "127.0.0.1:$_port" 2>/dev/null; then
        echo "  端口 127.0.0.1:$_port 已被占用，无法启动验收服务。"
        echo "  占用者：$(fuser "$_port"/tcp 2>&1 | tr -s ' ')"
        echo "  换个端口重跑，例如 SMB_PORT=<其它端口> 或 SMB_PORT_GUEST=/SMB_PORT_STRICT="
        exit 1
    fi
    "$WORK/stupidsamba" -config "$_cfg" > "$_log" 2>&1 &
    _pid=$!
    SRVPIDS="$SRVPIDS $_pid"
    _i=0
    while [ $_i -lt 50 ]; do
        if ! kill -0 "$_pid" 2>/dev/null; then
            echo "  服务启动失败 ($_cfg)，日志："
            sed 's/^/    /' "$_log"
            exit 1
        fi
        if "$WORK/probe" "127.0.0.1:$_port" 2>/dev/null; then
            # 探针连通 ≠ 连的是我们。端口可能被上一次调试遗留的进程占着，
            # 我们自己 bind 失败退出了，探针照样成功 —— 于是整轮验收其实
            # 是在测别人的服务。这个坑真踩过，所以这里必须再确认进程活着。
            if ! kill -0 "$_pid" 2>/dev/null; then
                echo "  127.0.0.1:$_port 上有其他进程在监听，而我们启动失败了。日志："
                sed 's/^/    /' "$_log"
                echo "  提示：用 fuser $_port/tcp 找出占用者"
                exit 1
            fi
            echo "  已监听 127.0.0.1:$_port (pid $_pid)"
            return 0
        fi
        _i=$((_i + 1))
        sleep 0.2
    done
    echo "  服务未在超时内监听 127.0.0.1:$_port，日志："
    sed 's/^/    /' "$_log"
    exit 1
}

say "启动服务"
start_server "$WORK/config.yaml"        "$PORT"        "$LOG"
start_server "$WORK/config-guest.yaml"  "$PORT_GUEST"  "$WORK/server-guest.log"
start_server "$WORK/config-strict.yaml" "$PORT_STRICT" "$LOG_STRICT"
# config-enc / config-enc-default 不再作为常驻服务启动：自 b7ef0ed 起，
# `encryption_required + min_dialect < 3.0` 在**配置校验阶段**就启动报错，
# 不再能跑到 negotiate 的 fail-closed。它们在 t_encryption 的 B 组里被当作
# "应当启动失败"的反向配置来断言（见 expect_config_block）。


SKIPPED=""
skip() { printf '\033[1;33m  [SKIP] %s (%s)\033[0m\n' "$1" "$2"; SKIPPED="$SKIPPED $1"; }

run_client() {
    name=$1
    if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then
        return 0
    fi
    say "$2"
    shift 2
    rc=0
    "$@" || rc=$?
    # 约定：返回 77 表示环境不满足而跳过，不计入通过数（GNU 惯例）
    case "$rc" in
        0)  ok "$name" ;;
        77) skip "$name" "环境不满足" ;;
        *)  bad "$name" ;;
    esac
}

# smbclient 便捷封装。
# 注意：smbclient 4.x 的 -c 不按换行拆分命令，必须用语义分隔符 `;`。
# 换行写法会导致 "listing \get" 之类的解析错误（是客户端解析问题，非服务端问题）。
sc() {
    _port=$1; _share=$2; _cmd=$3; shift 3
    smbclient "//127.0.0.1/$_share" -p "$_port" -U "$USER%$PASS" -d1 "$@" -c "$_cmd"
}

# ---------------------------------------------------------------- 客户端 1: smbclient

t_smbclient() {
    need_tool smbclient || return 77
    CMD="ls; get hello.txt $WORK/got-hello.txt; put $WORK/config.yaml uploaded.yaml; mkdir newdir; cd subdir; ls; cd ..; rename uploaded.yaml renamed.yaml; rm renamed.yaml; rmdir newdir"
    sc "$PORT" public "$CMD" -m SMB3 || return 1
    grep -q "hello from stupidsamba" "$WORK/got-hello.txt" || {
        echo "  下载内容不匹配"; return 1; }
    return 0
}

# ---------------------------------------------------------------- 客户端 2: impacket (Python)

t_impacket() {
    command -v python3 >/dev/null 2>&1 || { echo "  跳过：缺少 python3"; return 77; }
    python3 "$ROOT/scripts/clients/impacket_test.py" \
        127.0.0.1 "$PORT" "$USER" "$PASS" public "$WORK"
}

# ---------------------------------------------------------------- 客户端 3: go-smb2 (Go 客户端库)

t_gosmb2() {
    # 独立 Go module，避免把客户端库依赖污染主模块 go.mod
    ( cd "$ROOT/scripts/clients/gosmb2" && \
      go run . "127.0.0.1:$PORT" "$USER" "$PASS" public )
}

# ---------------------------------------------------------------- 客户端 4: mount.cifs (Linux 内核)

t_mountcifs() {
    [ "$(id -u)" = "0" ] || { echo "  跳过：需要 root 才能 mount"; return 77; }
    need_tool mount.cifs || return 77
    MNT="$WORK/mnt"
    mkdir -p "$MNT"
    # 加 timeout 防止容器内因缺少 CAP_SYS_ADMIN 而无限挂起；
    # 挂载失败视为环境能力限制（非服务端缺陷）而跳过，不计入失败——
    # 在具备特权的真实主机上本用例应当通过。
    timeout 25 mount -t cifs "//127.0.0.1/public" "$MNT" \
        -o "port=$PORT,username=$USER,password=$PASS,vers=3.0" 2>/tmp/mnt_err.$$ 
    rc=$?
    if [ $rc -ne 0 ]; then
        echo "  跳过：mount.cifs 失败（rc=$rc，环境能力限制），错误："
        sed 's/^/    /' /tmp/mnt_err.$$ 2>/dev/null | head -5
        return 77
    fi
    rc=0
    ls -la "$MNT" || rc=1
    grep -q "hello from stupidsamba" "$MNT/hello.txt" || rc=1
    # 写 + 大文件（put）
    echo "kernel client write" > "$MNT/kernel.txt" || rc=1
    dd if=/dev/zero of="$MNT/dd.bin" bs=64k count=16 2>/dev/null || rc=1
    # 创建目录（mkdir）
    mkdir "$MNT/mntdir" || rc=1
    [ -d "$MNT/mntdir" ] || rc=1
    # 重命名（rename）：写一个文件再通过 POSIX rename 触发 SMB2 重命名
    echo "to be renamed" > "$MNT/mntdir/f.txt" || rc=1
    mv "$MNT/mntdir/f.txt" "$MNT/mntdir/f_renamed.txt" || rc=1
    [ -f "$MNT/mntdir/f_renamed.txt" ] || rc=1
    [ ! -e "$MNT/mntdir/f.txt" ] || rc=1
    # 清理（rm）
    rm -f "$MNT/mntdir/f_renamed.txt" || rc=1
    rmdir "$MNT/mntdir" || rc=1
    rm -f "$MNT/kernel.txt" "$MNT/dd.bin" || rc=1
    timeout 10 umount "$MNT" 2>/dev/null || rc=1
    return $rc
}

# ---------------------------------------------------------------- 方言矩阵

# smbclient 的 -m 设的是**客户端上限**；服务端会挑双方都支持的最高方言，
# 所以 `-m SMB2_02` 等价于把协商结果钉在 2.0.2。
# 每个方言都真的跑一次 读 + 写 + 删，只协商成功不算数；
# 同时从服务端日志断言**真正协商出来的方言号**，避免"客户端悄悄降级/升级了
# 我们却以为测到了"这种自欺。
t_dialects() {
    need_tool smbclient || return 77
    rc=0
    for pair in SMB2_02:2.0.2 SMB2_10:2.1 SMB3_00:3.0 SMB3_02:3.0.2 SMB3_11:3.1.1; do
        d=${pair%%:*}
        want=${pair#*:}
        mark=$(wc -l < "$LOG")
        out=$(sc "$PORT" public "ls; get hello.txt $WORK/d-$d.txt; put $WORK/d-$d.txt dl-$d.txt; rm dl-$d.txt" -m "$d" 2>&1) || {
            echo "  $d: 失败"; echo "$out" | sed 's/^/      /' | head -8; rc=1; continue; }
        if ! grep -q "hello from stupidsamba" "$WORK/d-$d.txt" 2>/dev/null; then
            echo "  $d: 下载内容不匹配"; rc=1; continue
        fi
        if tail -n +$((mark + 1)) "$LOG" | grep -q "dialect=$want "; then
            echo "  $d: 协商为 $want，ls/get/put/rm OK"
        else
            echo "  $d: 操作成功但服务端协商的方言不是 $want，实际："
            tail -n +$((mark + 1)) "$LOG" | grep -o "dialect=[0-9.]*" | head -2 | sed 's/^/      /'
            rc=1
        fi
    done
    # impacket 是独立协议栈，能显式指定方言并把协商结果读回来，
    # 用它做一遍跨栈的交叉验证（它不支持 3.0.2，原因见该脚本注释）
    if command -v python3 >/dev/null 2>&1; then
        python3 "$ROOT/scripts/clients/impacket_dialects.py" 127.0.0.1 "$PORT" "$USER" "$PASS" public || rc=1
    else
        echo "  （缺 python3，跳过 impacket 跨栈方言断言）"
    fi
    return $rc
}

# ---------------------------------------------------------------- 签名

t_signing() {
    need_tool smbclient || return 77
    rc=0
    # 1) 主服务（不强制签名）：客户端**要求**签名也必须能连上并正常读写
    if sc "$PORT" public "ls; get hello.txt $WORK/sign1.txt" -m SMB3 \
        --option='clientsigning=required' >/dev/null 2>&1; then
        echo "  客户端强制签名 → OK"
    else
        echo "  客户端强制签名 → 失败"; rc=1
    fi
    # 2) SMB 2.0.2 下的签名（HMAC-SHA256 路径，与 SMB3 的 AES-CMAC 是两套代码）
    if sc "$PORT" public "ls" -m SMB2_02 --option='clientsigning=required' >/dev/null 2>&1; then
        echo "  SMB 2.0.2 + 强制签名 → OK"
    else
        echo "  SMB 2.0.2 + 强制签名 → 失败"; rc=1
    fi
    # 3) 强制签名的服务（PORT_STRICT，同时也强制加密）
    if sc "$PORT_STRICT" secure "ls; get hello.txt $WORK/sign3.txt" -m SMB3 >/dev/null 2>&1; then
        echo "  服务端 signing_required → OK"
    else
        echo "  服务端 signing_required → 失败"; rc=1
    fi
    return $rc
}

# ---------------------------------------------------------------- 加密
#
# 这一组用例的判据**不是**"smbclient 能读到文件内容"。
#
# 那个判据对**明文旁路**完全无感：服务端没加密、客户端也没加密，内容照样读得到，
# 用例照样绿。真实发生过 —— encryption_required: true 的服务被
# `smbclient -m SMB2_10` 一句降级成全程明文，验收脚本一声不吭。
#
# 所以正向判定改成看网线上的字节：客户端连 smbtap（透明 TCP 中继），
# 由它按 Direct TCP 帧边界统计 ProtocolId，断言
#   - 出现 SMB2 TRANSFORM_HEADER（0xFD 'S' 'M' 'B'，MS-SMB2 §2.2.41），且
#   - 明文帧里除 NEGOTIATE(0x00) / SESSION_SETUP(0x01) 外没有任何命令
#     （这两个按 MS-SMB2 §3.3.4.1.4 本来就不能加密）。
# 第二条比第一条严格：只加密了一部分流量的实现同样会被抓出来。
#
# 探针本身的有效性做过反向对照：对不加密的会话，它报出
# c2s_plain_postauth_cmds=0x3,0x4,0x5,0x6,0x8,0xe,0x10
#（TREE_CONNECT/TREE_DISCONNECT/CREATE/CLOSE/READ/QUERY_DIRECTORY/QUERY_INFO），
# 即它确实抓得住明文，不是恒绿的摆设。
#
# 注意：smbclient 4.22 **没有 `-e` 选项**（4.15 之前才有），要用
# `--client-protection=encrypt`。写成 -e 会直接打印 usage 退出、根本没连服务端，
# 表现却是"加密全线失败"，看着像服务端不支持加密 —— 典型的假故障。

# repval <报告文件> <键>  —— 从 smbtap 的 key=value 报告里取值
repval() { sed -n "s/^$2=//p" "$1" 2>/dev/null; }

# tap_begin <目标端口> <报告文件> <探针stdout>  —— 起 smbtap 并等它 bind 成功。
# 必须等就绪再放客户端进来，否则会撞 connection refused，看起来像服务端拒绝了连接。
TAP_PID=""
tap_begin() {
    _tp=$1; _trep=$2; _tout=$3
    rm -f "$_trep"
    "$WORK/smbtap" -listen "127.0.0.1:$PORT_TAP" -target "127.0.0.1:$_tp" \
        -report "$_trep" -conns 1 -timeout 60s > "$_tout" 2>&1 &
    TAP_PID=$!
    _i=0
    while [ $_i -lt 100 ]; do
        grep -q "smbtap ready" "$_tout" 2>/dev/null && return 0
        kill -0 "$TAP_PID" 2>/dev/null || return 1   # 探针已经死了（多半是端口被占）
        _i=$((_i + 1)); sleep 0.1
    done
    return 1
}

# tap_end <报告文件>  —— 等报告落盘。
# 报告是连接结束后才写的；`wait` 在 dash 下不保证文件已可见，轮询更稳。
#
# 拿不到报告时必须**主动把探针杀掉**：否则它会一直占着 PORT_TAP 直到 60s
# 超时，后续每个用例的 tap_begin 都会撞 "address already in use"，
# 一个真故障会级联成一整屏假故障（实测过）。
tap_end() {
    _trep=$1
    _i=0
    while [ $_i -lt 100 ] && [ ! -f "$_trep" ]; do
        _i=$((_i + 1)); sleep 0.1
    done
    if [ ! -f "$_trep" ]; then
        [ -n "$TAP_PID" ] && kill "$TAP_PID" 2>/dev/null
        wait "$TAP_PID" 2>/dev/null || true
        TAP_PID=""
        return 1
    fi
    TAP_PID=""
    return 0
}

# tap_assert_encrypted <方言标签> <报告文件>  —— 线级加密断言（A~D 共用同一套判据）。
# 两条都要满足：既有 TRANSFORM 帧，且明文帧里不含任何业务命令。
# 第二条比第一条严格：只加密了一部分流量的实现同样会被抓出来。
tap_assert_encrypted() {
    _lbl=$1; _r=$2
    _atx=$(repval "$_r" c2s_transform)
    _arx=$(repval "$_r" s2c_transform)
    _aleak=$(repval "$_r" c2s_plain_postauth_cmds)
    _aleakrx=$(repval "$_r" s2c_plain_postauth_cmds)
    if [ "${_atx:-0}" -eq 0 ] || [ "${_arx:-0}" -eq 0 ]; then
        echo "    $_lbl: 线上没有 TRANSFORM_HEADER（c2s=$_atx s2c=$_arx）—— 根本没加密"
        return 1
    fi
    if [ -n "$_aleak" ] || [ -n "$_aleakrx" ]; then
        echo "    $_lbl: 有加密帧，但仍有业务命令走明文 c2s=[$_aleak] s2c=[$_aleakrx]"
        return 1
    fi
    TAP_TX=$_atx; TAP_RX=$_arx
    return 0
}

# enc_case <服务端口> <共享> <服务端日志> <方言> <期望 reject|encrypt> <期望cipher|-> [额外 smbclient 参数...]
#
# 全程经由 smbtap 转发，结束后按线级统计断言。返回 0 表示该档符合预期。
enc_case() {
    _p=$1; _sh=$2; _slog=$3; _d=$4; _expect=$5; _wantcipher=$6
    shift 6

    _tag="$_p-$_d"
    _rep="$WORK/tap-$_tag.txt"
    _tout="$WORK/tap-$_tag.out"
    _cliout="$WORK/cli-$_tag.out"
    _mark=$(wc -l < "$_slog")

    if ! tap_begin "$_p" "$_rep" "$_tout"; then
        echo "    $_d: smbtap 起不来（探针本身出问题了）"
        sed 's/^/      /' "$_tout" | head -5
        return 1
    fi

    _cli=0
    smbclient "//127.0.0.1/$_sh" -p "$PORT_TAP" -U "$USER%$PASS" -d1 -m "$_d" "$@" \
        -c "ls; get hello.txt $WORK/enc-$_tag.txt" > "$_cliout" 2>&1 || _cli=1

    if ! tap_end "$_rep"; then
        echo "    $_d: smbtap 未产出报告（探针本身出问题了）"
        sed 's/^/      /' "$_tout" | head -5
        return 1
    fi

    _leak=$(repval "$_rep" c2s_plain_postauth_cmds)
    _leakrx=$(repval "$_rep" s2c_plain_postauth_cmds)

    if [ "$_expect" = reject ]; then
        if [ "$_cli" -eq 0 ]; then
            echo "    $_d: 期望被拒，实际**连上了** —— 强制加密被方言降级绕过"
            return 1
        fi
        # 被拒还不够：必须确认没有任何业务命令以明文走过网线
        if [ -n "$_leak" ] || [ -n "$_leakrx" ]; then
            echo "    $_d: 虽然报错，但明文里出现了业务命令 c2s=[$_leak] s2c=[$_leakrx]"
            return 1
        fi
        # 还不够：得确认「被拒」这件事真的发生在**服务端**。
        # 客户端因为参数写错、库缺失之类的原因自己死掉时，退出码同样非 0、
        # 线上同样没有业务命令 —— 不加这一条，那种情况会被当成"拒绝成功"
        # 打出 OK（脚本开头 §"smbclient 4.22 没有 -e 选项"那段警告的正是这种假故障）。
        _st=$(grep -o 'NT_STATUS_[A-Z_]*' "$_cliout" | head -1)
        if [ -z "$_st" ]; then
            echo "    $_d: 客户端失败了但没给出任何 NT_STATUS —— 它可能根本没连到服务端："
            tail -3 "$_cliout" | sed 's/^/      /'
            return 1
        fi
        if [ "$(repval "$_rep" conns)" = "0" ]; then
            echo "    $_d: 线级探针记录到 0 个连接 —— 客户端没走到服务端，这不算拒绝"
            return 1
        fi
        echo "    $_d: 被拒绝（$_st），线上无明文业务命令 → OK"
        return 0
    fi

    # _expect = encrypt
    if [ "$_cli" -ne 0 ]; then
        echo "    $_d: 期望连上并加密，实际失败："
        tail -2 "$_cliout" | sed 's/^/      /'
        return 1
    fi
    if ! grep -q "hello from stupidsamba" "$WORK/enc-$_tag.txt" 2>/dev/null; then
        echo "    $_d: 连上了但下载内容不匹配"
        return 1
    fi
    tap_assert_encrypted "$_d" "$_rep" || return 1
    if [ "$_wantcipher" != "-" ] &&
       ! tail -n +$((_mark + 1)) "$_slog" | grep -q "cipher=$_wantcipher"; then
        echo "    $_d: 服务端协商出的 cipher 不是 $_wantcipher，实际："
        tail -n +$((_mark + 1)) "$_slog" | grep -o "cipher=[0-9]*" | head -2 | sed 's/^/      /'
        return 1
    fi
    echo "    $_d: 加密帧 c2s=$TAP_TX s2c=$TAP_RX，明文仅协商/认证，cipher=$_wantcipher → OK"
    return 0
}

# expect_config_block <配置文件> <端口> <错误里必须出现的子串>
#
# 与 start_server 相反：这个配置**应当启动失败**。我们用它来断言
# `encryption_required + min_dialect < 3.0` 在配置校验阶段就 fail fast
# （b7ef0ed 之后是硬错误，不是 WARN）。判定：
#   1. 进程应该在几秒内退出（没退出 = 配置被接受了 = 失败）；
#   2. 退出前的 stderr 必须包含指定的错误子串（点名 server.min_dialect）。
# 端口用 PORT_ENC（这里不跑常驻服务，专留给"应当起不来的配置"做隔离）。
expect_config_block() {
    _cfg=$1; _port=$2; _need=$3
    if "$WORK/probe" "127.0.0.1:$_port" 2>/dev/null; then
        echo "    端口 $_port 被占用，无法验证启动失败：$(fuser "$_port"/tcp 2>&1 | tr -s ' ')"
        return 1
    fi
    _out="$WORK/cfgfail-$(basename "$_cfg" .yaml).txt"
    "$WORK/stupidsamba" -config "$_cfg" > "$_out" 2>&1 &
    _pid=$!
    _i=0
    while kill -0 "$_pid" 2>/dev/null; do
        _i=$((_i + 1)); [ $_i -ge 50 ] && break; sleep 0.1
    done
    if kill -0 "$_pid" 2>/dev/null; then
        echo "    配置本应启动失败，但进程还活着（端口 $_port）"; kill "$_pid" 2>/dev/null || true
        return 1
    fi
    if grep -q "$_need" "$_out"; then
        echo "    配置层拦截 OK：启动报错且点名 '$_need'"
        return 0
    fi
    echo "    配置未如预期报错（缺 '$_need'）："; sed 's/^/      /' "$_out" | head -5
    return 1
}

t_encryption() {
    need_tool smbclient || return 77
    rc=0

    # 探针端口必须是空的，否则整轮加密验收会在测别人的服务
    if "$WORK/probe" "127.0.0.1:$PORT_TAP" 2>/dev/null; then
        echo "  smbtap 端口 127.0.0.1:$PORT_TAP 被占用：$(fuser "$PORT_TAP"/tcp 2>&1 | tr -s ' ')"
        echo "  换端口重跑：SMB_PORT_TAP=<其它端口>"
        return 1
    fi

    # ---- A. 服务端不强制，客户端主动要求加密 ----
    # 3.1.1 走 AES-128-GCM(cipher=2)，3.0 走 AES-128-CCM(cipher=1)，是两套代码。
    echo "  A. 客户端主动要求加密（服务端未强制，$PORT）"
    enc_case "$PORT" public "$LOG" SMB3_11 encrypt 2 --client-protection=encrypt || rc=1
    enc_case "$PORT" public "$LOG" SMB3_00 encrypt 1 --client-protection=encrypt || rc=1

    # ---- B. encryption_required + min_dialect < 3.0：配置层 fail fast ----
    # 曾经的缺口是"方言降级绕过加密"——客户端 -m SMB2_10 把方言压到 3.1.1 以下，
    # 协商不出加密算法，加密强制静默失效、全程明文。修复把它彻底堵死：
    #   - negotiate 阶段对 2.x 直接 ACCESS_DENIED（见 internal/server/encryption_test.go）；
    #   - 配置校验阶段对 encryption_required + min_dialect < 3.0 直接启动报错。
    # 因此"允许低方言接入"的配置本身已经起不来了——这正是我们想要的：
    # 把矛盾在启动期一次性暴露，而不是让用户在运行期看到"连不上"去瞎猜。
    # 这里用两个反向配置断言它确实起不来、且错误点名 server.min_dialect。
    echo "  B. encryption_required + min_dialect < 3.0 配置层拦截"
    expect_config_block "$WORK/config-enc.yaml"          "$PORT_ENC" "server.min_dialect" || rc=1
    expect_config_block "$WORK/config-enc-default.yaml"  "$PORT_ENC" "server.min_dialect" || rc=1

    # ---- C. encryption_required + min_dialect 3.0（合法配置）：端到端拒绝 + 加密 ----
    # 合法配置下，低方言在**方言选择层**被拒（无公共方言 → NOT_SUPPORTED），
    # 与 negotiate 的 ACCESS_DENIED 是两道独立的防线。
    echo "  C. encryption_required + min_dialect 3.0（$PORT_STRICT）—— 合法配置下的拒绝与加密"
    enc_case "$PORT_STRICT" secure "$LOG_STRICT" SMB2_02 reject  - || rc=1
    enc_case "$PORT_STRICT" secure "$LOG_STRICT" SMB2_10 reject  - || rc=1
    enc_case "$PORT_STRICT" secure "$LOG_STRICT" SMB3_00 encrypt 1 || rc=1
    enc_case "$PORT_STRICT" secure "$LOG_STRICT" SMB3_02 encrypt 1 || rc=1
    enc_case "$PORT_STRICT" secure "$LOG_STRICT" SMB3_11 encrypt 2 || rc=1

    # ---- D. 换一个协议栈交叉验证，别只对 Samba 客户端调通 ----
    #
    # 判据必须与 A~C 一致：看网线，不看"客户端有没有报错"。
    #
    # 这里原先只判断 `go run .` 的退出码。实测过它是个**摆设**：
    # 把同一条命令指向完全不加密的主服务（4445），退出码照样是 0，
    # 而线级报告是 c2s_transform=0 + 10 条明文业务命令
    # （0x2,0x3,0x4,0x5,0x6,0x8,0x9,0xe,0x10,0x11）。也就是说
    # "go-smb2 对强制加密服务 → OK" 这行字在纯明文会话下也会照打，
    # 正好又犯了本节开头 §"判据不是能读到内容" 警告的那个错。
    echo "  D. go-smb2 对强制加密服务（线级判定）"
    _drep="$WORK/tap-gosmb2.txt"
    _dout="$WORK/tap-gosmb2.out"
    _dcliout="$WORK/cli-gosmb2.out"
    if ! tap_begin "$PORT_STRICT" "$_drep" "$_dout"; then
        echo "    go-smb2: smbtap 起不来（探针本身出问题了）"
        sed 's/^/      /' "$_dout" | head -5
        rc=1
    else
        _dcli=0
        ( cd "$ROOT/scripts/clients/gosmb2" && \
          go run . "127.0.0.1:$PORT_TAP" "$USER" "$PASS" secure ) > "$_dcliout" 2>&1 || _dcli=1
        if ! tap_end "$_drep"; then
            echo "    go-smb2: smbtap 未产出报告（探针本身出问题了）"
            sed 's/^/      /' "$_dout" | head -5
            rc=1
        elif [ "$_dcli" -ne 0 ]; then
            echo "    go-smb2: 连接强制加密服务失败："
            tail -3 "$_dcliout" | sed 's/^/      /'
            rc=1
        elif tap_assert_encrypted "go-smb2" "$_drep"; then
            echo "    go-smb2: 加密帧 c2s=$TAP_TX s2c=$TAP_RX，明文仅协商/认证 → OK"
        else
            rc=1
        fi
    fi

    # ---- 诚实声明：negotiate 层 "Cipher==0 仍 ACCESS_DENIED" 这一档 ----
    # 在本环境的可达客户端里**无法端到端触发**：smbclient 4.22 即使
    # --client-protection=off 也会照常宣告 CAP_ENCRYPTION（会话照常成功），
    # 而 impacket 走 SMB1 通配入口 "SMB 2.???" 会被放行到真正的 SMB2 协商、
    # 协商出 3.0+加密亦成功。所以这一档（以及 SMB1 仅含 "SMB 2.002" 的入口）
    # 由 internal/server 的单元测试覆盖（audit 的 encryption_paths_test.go，
    # 每条都做过负向实验），不在客户端端到端矩阵里硬凑。上述 A~D 覆盖的是
    # 配置层 fail fast 与方言层拒绝两条确实可达的路径。
    return $rc
}

# ---------------------------------------------------------------- 只读共享

t_readonly() {
    need_tool smbclient || return 77
    rc=0
    # 读要能读
    if sc "$PORT" ro "ls; get ro.txt $WORK/ro-got.txt" -m SMB3 >/dev/null 2>&1 &&
       grep -q "read only content" "$WORK/ro-got.txt" 2>/dev/null; then
        echo "  只读共享可读 → OK"
    else
        echo "  只读共享可读 → 失败"; rc=1
    fi
    # 写必须被拒。注意不能只看退出码：要确认是**权限类**拒绝，
    # 而不是别的错误（比如路径不存在）蒙混过关。
    out=$(sc "$PORT" ro "put $WORK/ro-got.txt should-fail.txt" -m SMB3 2>&1 || true)
    if echo "$out" | grep -qE "ACCESS_DENIED|MEDIA_WRITE_PROTECTED"; then
        echo "  只读共享写入被拒 → OK ($(echo "$out" | grep -oE 'NT_STATUS_[A-Z_]+' | head -1))"
    else
        echo "  只读共享写入**没有被拒**，实际输出："; echo "$out" | sed 's/^/      /' | head -5
        rc=1
    fi
    # 建目录也必须被拒
    out=$(sc "$PORT" ro "mkdir shouldfail" -m SMB3 2>&1 || true)
    if echo "$out" | grep -qE "ACCESS_DENIED|MEDIA_WRITE_PROTECTED"; then
        echo "  只读共享建目录被拒 → OK"
    else
        echo "  只读共享建目录**没有被拒**，实际输出："; echo "$out" | sed 's/^/      /' | head -5
        rc=1
    fi
    return $rc
}

# ---------------------------------------------------------------- 该拒绝的要拒绝

t_authfail() {
    need_tool smbclient || return 77
    rc=0
    # 1) 错误口令
    out=$(smbclient "//127.0.0.1/public" -p "$PORT" -U "$USER%wrong-password" -m SMB3 -d1 -c ls 2>&1 || true)
    if echo "$out" | grep -qE "LOGON_FAILURE|ACCESS_DENIED"; then
        echo "  错误口令被拒 → OK"
    else
        echo "  错误口令**没有被拒**："; echo "$out" | sed 's/^/      /' | head -5; rc=1
    fi
    # 2) 不存在的用户
    out=$(smbclient "//127.0.0.1/public" -p "$PORT" -U "nosuchuser%whatever" -m SMB3 -d1 -c ls 2>&1 || true)
    if echo "$out" | grep -qE "LOGON_FAILURE|ACCESS_DENIED"; then
        echo "  不存在的用户被拒 → OK"
    else
        echo "  不存在的用户**没有被拒**："; echo "$out" | sed 's/^/      /' | head -5; rc=1
    fi
    # 3) allow_guest=false 时匿名必须被拒（默认不开 guest，AGENTS.md §8）
    out=$(smbclient "//127.0.0.1/public" -p "$PORT" -N -m SMB3 -d1 -c ls 2>&1 || true)
    if echo "$out" | grep -qE "LOGON_FAILURE|ACCESS_DENIED"; then
        echo "  allow_guest=false 时匿名被拒 → OK"
    else
        echo "  allow_guest=false 时匿名**没有被拒**："; echo "$out" | sed 's/^/      /' | head -5; rc=1
    fi
    # 4) 不存在的共享
    out=$(smbclient "//127.0.0.1/nosuchshare" -p "$PORT" -U "$USER%$PASS" -m SMB3 -d1 -c ls 2>&1 || true)
    if echo "$out" | grep -qE "BAD_NETWORK_NAME|OBJECT_NAME_NOT_FOUND"; then
        echo "  不存在的共享被拒 → OK"
    else
        echo "  不存在的共享**没有被正确拒绝**："; echo "$out" | sed 's/^/      /' | head -5; rc=1
    fi
    return $rc
}

# ---------------------------------------------------------------- guest

t_guest() {
    need_tool smbclient || return 77
    rc=0
    # allow_guest=true + guest_ok 的共享上，匿名（-N）应当能读写
    if smbclient "//127.0.0.1/pub" -p "$PORT_GUEST" -N -m SMB3 -d1 \
        -c "ls; get hello.txt $WORK/guest-got.txt" >/dev/null 2>&1 &&
       grep -q "hello from stupidsamba" "$WORK/guest-got.txt" 2>/dev/null; then
        echo "  guest 匿名读取 → OK"
    else
        echo "  guest 匿名读取 → 失败"; rc=1
    fi
    # 同一个服务上，带正确口令的具名用户也必须照常可用
    if smbclient "//127.0.0.1/pub" -p "$PORT_GUEST" -U "$USER%$PASS" -m SMB3 -d1 \
        -c "ls" >/dev/null 2>&1; then
        echo "  guest 服务上的具名用户 → OK"
    else
        echo "  guest 服务上的具名用户 → 失败"; rc=1
    fi
    # 开 guest 必须留下明确的启动告警（AGENTS.md §8）
    if grep -qi "guest" "$WORK/server-guest.log"; then
        echo "  启动日志含 guest 告警 → OK"
    else
        echo "  启动日志**没有** guest 告警（AGENTS.md §8 要求）"; rc=1
    fi
    return $rc
}

run_client smbclient  "客户端 smbclient (Samba 官方 CLI)"        t_smbclient
run_client impacket   "客户端 impacket (Python 独立实现)"        t_impacket
run_client gosmb2     "客户端 hirochachacha/go-smb2 (Go 客户端)" t_gosmb2
run_client mountcifs  "客户端 mount.cifs (Linux 内核客户端)"     t_mountcifs
run_client dialects   "方言矩阵 2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1" t_dialects
run_client signing    "SMB 签名"                                  t_signing
run_client encryption "SMB3 加密"                                 t_encryption
run_client readonly   "只读共享"                                  t_readonly
run_client authfail   "认证与授权的拒绝路径"                      t_authfail
run_client guest      "guest 匿名访问"                            t_guest

# ---------------------------------------------------------------- 汇总

say "结果汇总"
echo "  通过:$PASSED"
echo "  失败:$FAILED"
[ -n "$SKIPPED" ] && echo "  跳过:$SKIPPED"

if [ -n "$FAILED" ]; then
    echo
    echo "服务端日志（末尾 60 行）:"
    tail -60 "$LOG" | sed 's/^/    /'
    exit 1
fi

# AGENTS.md §3 的门槛是"三种**不同的第三方客户端栈**通过"，
# 所以这里只数这三家，不把方言/签名等附加用例算进去 —— 否则加用例
# 反而会让门槛变松。
if [ -z "$ONLY" ]; then
    n=0
    missing=""
    for c in smbclient impacket gosmb2; do
        case " $PASSED " in
            *" $c "*) n=$((n + 1)) ;;
            *)        missing="$missing $c" ;;
        esac
    done
    if [ "$n" -lt 3 ]; then
        echo "  验收未达标：需要 smbclient/impacket/gosmb2 三家全过，缺:$missing"
        exit 1
    fi
    echo "  验收通过（第三方客户端栈 $n 家 + 附加用例全绿）"
else
    echo "  验收通过"
fi
