#!/bin/sh
# test/acceptance-impacket.sh —— impacket（Python 独立 SMB 实现）验收（AGENTS.md §3 第 3 项）
#
# impacket 是与 Samba 完全独立的 Python 协议栈，用它测能发现"只对着 Samba 客户端
# 调通"的实现偏差。本脚本自包含：自己构建并启动一个 stupidsamba 实例，
# 再用 impacket 做 ls / get / put / mkdir / rename / rm。
#
# 依赖（AGENTS.md §10.3 第 3 条）：impacket 用 apt 装，不要用 pip：
#   apt-get install python3-impacket
# 模块缺失时本脚本以 77 退出（与 CI 的 skip 语义一致）。
#
# 用法：
#   test/acceptance-impacket.sh
#
# 约定：退出码 0 = 通过；非 0 = 失败；77 = 环境不具备（缺 python3 / impacket）跳过。
set -e

cd "$(dirname "$0")/.."
ROOT=$(pwd)
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

PORT=${SMB_PORT:-4446}
USER=${SMB_USER:-testuser}
PASS=${SMB_PASS:-testpass123}

WORK=$(mktemp -d /tmp/stupidsamba-imp.XXXXXX)
SHARE="$WORK/share"
LOG="$WORK/server.log"
SRVPIDS=""

cleanup() {
    for p in $SRVPIDS; do
        if kill -0 "$p" 2>/dev/null; then
            kill "$p" 2>/dev/null || true
            wait "$p" 2>/dev/null || true
        fi
    done
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

printf '\n\033[1;36m=== %s ===\033[0m\n' "构建"
go build -o "$WORK/stupidsamba" ./cmd/stupidsamba
echo "  OK"

printf '\n\033[1;36m=== %s ===\033[0m\n' "准备共享目录与配置"
mkdir -p "$SHARE/subdir"
echo "hello from stupidsamba" > "$SHARE/hello.txt"
echo "nested" > "$SHARE/subdir/nested.txt"
# impacket_test.py 需要的大文件（跨多 READ 请求，验证分块与 credit）。
head -c 1048576 /dev/urandom > "$SHARE/blob.bin"

cat > "$WORK/config.yaml" <<EOF
server:
  name: STUPIDSAMBA
  domain: WORKGROUP
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
  - name: public
    path: $SHARE
    read_only: false
mdns:
  enabled: false
log:
  level: info
EOF
echo "  OK"

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

start_server() {
    _cfg=$1; _port=$2; _log=$3
    if "$WORK/probe" "127.0.0.1:$_port" 2>/dev/null; then
        echo "  端口 127.0.0.1:$_port 已被占用，请换端口（如 SMB_PORT=<其它>）。"
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

printf '\n\033[1;36m=== %s ===\033[0m\n' "启动服务"
start_server "$WORK/config.yaml" "$PORT" "$LOG"

printf '\n\033[1;36m=== %s ===\033[0m\n' "客户端 impacket (Python 独立实现)"
command -v python3 >/dev/null 2>&1 || {
    echo "  [SKIP] 缺少 python3（环境不满足，rc=77）"; exit 77; }
python3 -c "import impacket" 2>/dev/null || {
    echo "  [SKIP] 缺少 impacket 模块（apt-get install python3-impacket，rc=77）"; exit 77; }

if python3 "$ROOT/scripts/clients/impacket_test.py" \
        127.0.0.1 "$PORT" "$USER" "$PASS" public "$WORK"; then
    printf '\033[1;32m  [PASS] impacket\033[0m\n'
    echo "  验收通过（impacket 第三方客户端栈）"
    exit 0
else
    printf '\033[1;31m  [FAIL] impacket\033[0m\n'
    echo
    echo "服务端日志（末尾 40 行）:"
    tail -40 "$LOG" | sed 's/^/    /'
    exit 1
fi
