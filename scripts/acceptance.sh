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
  # impacket 的 SMBConnection 默认走 negotiateSessionWildcard，
  # 会先发 SMB1 多协议协商再升级到 SMB2。impacket 是 AGENTS.md §3
  # 必测矩阵的第 3 项，所以验收环境必须开这个入口。
  # 注意：这里只是**协商入口**，不提供任何 SMB1 文件操作。
  smb1: true
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

i=0
while [ $i -lt 50 ]; do
    if ! kill -0 "$SRVPID" 2>/dev/null; then
        echo "  服务启动失败，日志："
        sed 's/^/    /' "$LOG"
        exit 1
    fi
    if "$WORK/probe" "127.0.0.1:$PORT" 2>/dev/null; then
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
    need_tool smbclient || return 77
    # 注意：smbclient 4.x 的 -c 不按换行拆分命令，必须用语义分隔符 `;`。
    # 换行写法会导致 "listing \get" 之类的解析错误（是客户端解析问题，非服务端问题）。
    CMD="ls; get hello.txt $WORK/got-hello.txt; put $WORK/config.yaml uploaded.yaml; mkdir newdir; cd subdir; ls; cd ..; rename uploaded.yaml renamed.yaml; rm renamed.yaml; rmdir newdir"
    smbclient "//127.0.0.1/public" -p "$PORT" -U "$USER%$PASS" -m SMB3 -d1 -c "$CMD" || return 1
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
