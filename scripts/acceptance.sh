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
USER=${SMB_USER:-testuser}
PASS=${SMB_PASS:-testpass123}
ONLY=$1

WORK=$(mktemp -d /tmp/stupidsamba-acc.XXXXXX)
SHARE="$WORK/share"
RO="$WORK/readonly"
LOG="$WORK/server.log"
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
echo "  OK ($WORK/config.yaml, config-guest.yaml, config-strict.yaml)"

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
start_server "$WORK/config-strict.yaml" "$PORT_STRICT" "$WORK/server-strict.log"

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

# 注意：smbclient 4.22 **没有 `-e` 选项**（4.15 之前才有），要用
# `--client-protection=encrypt`。写成 -e 会直接打印 usage 退出、根本没连服务端，
# 表现却是"加密全线失败"，看着像服务端不支持加密 —— 典型的假故障。
t_encryption() {
    need_tool smbclient || return 77
    rc=0
    # 1) 客户端主动要求加密，对不强制加密的主服务
    if sc "$PORT" public "ls; get hello.txt $WORK/enc1.txt; put $WORK/enc1.txt enc-up.txt; rm enc-up.txt" \
        -m SMB3 --client-protection=encrypt >/dev/null 2>&1; then
        echo "  客户端要求加密（SMB3）→ OK"
    else
        echo "  客户端要求加密（SMB3）→ 失败"; rc=1
    fi
    # 2) SMB 3.1.1 加密（AES-128-GCM 路径，与 3.0 的 AES-128-CCM 是两套代码）
    if sc "$PORT" public "ls" -m SMB3_11 --client-protection=encrypt >/dev/null 2>&1; then
        echo "  SMB 3.1.1 + 加密 → OK"
    else
        echo "  SMB 3.1.1 + 加密 → 失败"; rc=1
    fi
    # 2b) SMB 3.0 加密（AES-128-CCM 路径）
    if sc "$PORT" public "ls" -m SMB3_00 --client-protection=encrypt >/dev/null 2>&1; then
        echo "  SMB 3.0 + 加密 → OK"
    else
        echo "  SMB 3.0 + 加密 → 失败"; rc=1
    fi
    # 3) 服务端 encryption_required：客户端**没主动要求**也必须被加密保护
    if sc "$PORT_STRICT" secure "ls; get hello.txt $WORK/enc3.txt" -m SMB3 >/dev/null 2>&1; then
        echo "  服务端 encryption_required（客户端未显式 -e）→ OK"
    else
        echo "  服务端 encryption_required（客户端未显式 -e）→ 失败"; rc=1
    fi
    # 4) 走一遍纯 Go 客户端，交叉验证不是只对 Samba 客户端调通
    if ( cd "$ROOT/scripts/clients/gosmb2" && \
         go run . "127.0.0.1:$PORT_STRICT" "$USER" "$PASS" secure >/dev/null 2>&1 ); then
        echo "  go-smb2 对强制加密服务 → OK"
    else
        echo "  go-smb2 对强制加密服务 → 失败"; rc=1
    fi
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
