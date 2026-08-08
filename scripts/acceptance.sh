#!/bin/sh
# acceptance.sh —— 多客户端验收测试驱动（AGENTS.md §3）
#
# 硬性验收门槛：至少三种不同的第三方 SMB 客户端测试通过。
#
# 用法：
#   scripts/acceptance.sh              # 跑全部客户端
#   scripts/acceptance.sh smbclient    # 只跑指定客户端
#
# 环境变量：
#   SMB_PORT   服务端口，默认 4445（非特权，便于无 root 环境跑）
#   SMB_USER   测试用户，默认 testuser
#   SMB_PASS   测试口令，默认 testpass123
#   KEEP       设为 1 时保留临时目录与服务进程，便于手工调试
set -e

cd "$(dirname "$0")/.."
ROOT=$(pwd)
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

PORT=${SMB_PORT:-4445}
USER=${SMB_USER:-testuser}
PASS=${SMB_PASS:-testpass123}
ONLY=$1

WORK=$(mktemp -d /tmp/stupidsamba-acc.XXXXXX)
SHARE="$WORK/share"
LOG="$WORK/server.log"
SRVPID=""

PASSED=""
FAILED=""

cleanup() {
    if [ -n "$SRVPID" ] && kill -0 "$SRVPID" 2>/dev/null; then
        kill "$SRVPID" 2>/dev/null || true
        wait "$SRVPID" 2>/dev/null || true
    fi
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
mkdir -p "$SHARE/subdir"
echo "hello from stupidsamba" > "$SHARE/hello.txt"
head -c 1048576 /dev/urandom > "$SHARE/blob.bin"
echo "nested" > "$SHARE/subdir/nested.txt"

cat > "$WORK/config.yaml" <<EOF
server:
  name: STUPIDSAMBA
  domain: WORKGROUP
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
mdns:
  enabled: false
log:
  level: debug
EOF
echo "  OK ($WORK/config.yaml)"

say "启动服务"
"$WORK/stupidsamba" -config "$WORK/config.yaml" > "$LOG" 2>&1 &
SRVPID=$!

i=0
while [ $i -lt 50 ]; do
    if ! kill -0 "$SRVPID" 2>/dev/null; then
        echo "  服务启动失败，日志："
        sed 's/^/    /' "$LOG"
        exit 1
    fi
    # 探测端口（python3 是测试环境已有的工具，非运行时依赖）
    if python3 -c "import socket,sys; s=socket.socket(); s.settimeout(0.5); sys.exit(s.connect_ex(('127.0.0.1',$PORT)))" 2>/dev/null; then
        break
    fi
    i=$((i + 1))
    sleep 0.2
done
if [ $i -ge 50 ]; then
    echo "  服务未在超时内监听 127.0.0.1:$PORT，日志："
    sed 's/^/    /' "$LOG"
    exit 1
fi
echo "  已监听 127.0.0.1:$PORT (pid $SRVPID)"

SKIPPED=""
skip() { printf '\033[1;33m  [SKIP] %s (%s)\033[0m\n' "$1" "$2"; SKIPPED="$SKIPPED $1"; }

run_client() {
    name=$1
    if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then
        return 0
    fi
    say "客户端 $2"
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

# ---------------------------------------------------------------- 客户端 1: smbclient

t_smbclient() {
    smbclient "//127.0.0.1/public" -p "$PORT" -U "$USER%$PASS" -m SMB3 -d1 -c '
        ls
        get hello.txt '"$WORK"'/got-hello.txt
        put '"$WORK"'/config.yaml uploaded.yaml
        mkdir newdir
        cd subdir
        ls
        cd ..
        rename uploaded.yaml renamed.yaml
        rm renamed.yaml
        rmdir newdir
    ' || return 1
    grep -q "hello from stupidsamba" "$WORK/got-hello.txt" || {
        echo "  下载内容不匹配"; return 1; }
    return 0
}

# ---------------------------------------------------------------- 客户端 2: impacket (Python)

t_impacket() {
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
    MNT="$WORK/mnt"
    mkdir -p "$MNT"
    mount -t cifs "//127.0.0.1/public" "$MNT" \
        -o "port=$PORT,username=$USER,password=$PASS,vers=3.0" || return 1
    rc=0
    ls -la "$MNT" || rc=1
    grep -q "hello from stupidsamba" "$MNT/hello.txt" || rc=1
    echo "kernel client write" > "$MNT/kernel.txt" || rc=1
    dd if=/dev/zero of="$MNT/dd.bin" bs=64k count=16 2>/dev/null || rc=1
    rm -f "$MNT/kernel.txt" "$MNT/dd.bin" || rc=1
    umount "$MNT" || rc=1
    return $rc
}

run_client smbclient "smbclient (Samba 官方 CLI)"        t_smbclient
run_client impacket  "impacket (Python 独立实现)"        t_impacket
run_client gosmb2    "hirochachacha/go-smb2 (Go 客户端)" t_gosmb2
run_client mountcifs "mount.cifs (Linux 内核客户端)"     t_mountcifs

# ---------------------------------------------------------------- 汇总

say "结果汇总"
echo "  通过:$PASSED"
echo "  失败:$FAILED"
[ -n "$SKIPPED" ] && echo "  跳过:$SKIPPED"

n=0
for _ in $PASSED; do n=$((n + 1)); done

if [ -n "$FAILED" ]; then
    echo
    echo "服务端日志（末尾 60 行）:"
    tail -60 "$LOG" | sed 's/^/    /'
    exit 1
fi

if [ "$n" -lt 3 ] && [ -z "$ONLY" ]; then
    echo "  验收未达标：需要至少 3 种客户端通过，当前 $n 种"
    exit 1
fi

echo "  验收通过（$n 种客户端）"
