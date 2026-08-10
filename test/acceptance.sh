#!/bin/sh
# test/acceptance.sh —— 第三方 SMB 客户端验收（AGENTS.md §3 客户端矩阵）
#
# 覆盖 smbclient（Samba 官方 CLI）与 impacket（Python 独立实现）两种客户端。
# go-smb2 第三栈见 test/integration/gosmb2_test.go（可进 CI 的纯 Go 集成测试）。
#
# 用法：
#   test/acceptance.sh              # 跑 smbclient + impacket（mount.cifs 跳过）
#   test/acceptance.sh smbclient    # 只跑 smbclient
#   test/acceptance.sh impacket     # 只跑 impacket
#   test/acceptance.sh mountcifs    # 只跑 mount.cifs（本容器永远跳过，返回 77）
#
# 约定（GNU 惯例）：
#   退出码 0  = 通过；非零 = 失败；77 = 环境不具备（缺工具/容器限制）跳过。
#   工具缺失时对应用例返回 77，不计入失败；只有当"三种客户端栈"里
#   该有的栈因环境缺失而无法验证时，才整体以 77 退出（CI 视为 skip）。
#
# 环境铁律（AGENTS.md §10.3）：
#   - smbclient 4.22 的 `-c` 多命令**用分号分隔**，换行会解析错（"listing \get"）。
#   - mount.cifs 在本容器永远跑不通（非初始 user namespace 内核禁止无 FS_USERNS_MOUNT
#     的文件系统挂载），直接 skip(77)，绝不让它失败。
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
SRVPIDS=""

PASSED=""
FAILED=""
SKIPPED=""

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

say() { printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"; }
ok()  { printf '\033[1;32m  [PASS] %s\033[0m\n' "$1"; PASSED="$PASSED $1"; }
bad() { printf '\033[1;31m  [FAIL] %s\033[0m\n' "$1"; FAILED="$FAILED $1"; }
skip() { printf '\033[1;33m  [SKIP] %s (%s)\033[0m\n' "$1" "$2"; SKIPPED="$SKIPPED $1"; }

# ---------------------------------------------------------------- 构建 + 准备
say "构建"
go build -o "$WORK/stupidsamba" ./cmd/stupidsamba
echo "  OK"

say "准备共享目录与配置"
mkdir -p "$SHARE/subdir"
echo "hello from stupidsamba" > "$SHARE/hello.txt"
echo "nested" > "$SHARE/subdir/nested.txt"
# impacket_test.py 需要的大文件（跨多 READ 请求，验证分块与 credit）。
head -c 1048576 /dev/urandom > "$SHARE/blob.bin"

cat > "$WORK/config.yaml" <<EOF
server:
  name: STUPIDSAMBA
  domain: WORKGROUP
  # impacket 的 SMBConnection 默认走 negotiateSessionWildcard：先发 SMB1
  # 多协议协商再升级到 SMB2，所以验收环境必须开这个入口（只接受协商入口，
  # 不提供任何 SMB1 文件操作）。
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

# 纯 Go 无外部依赖端口探针（不依赖 nc/python3，避免重启后工具缺失）。
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

# start_server <配置文件> <端口> <日志文件>
# 直接在子 shell 用 `&` 启动（本 shell 直接子进程，$! 即监听进程，kill 得掉）。
# 先确认端口为空，避免 bind 失败却探测到隔壁 agent 的服务。
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

say "启动服务"
start_server "$WORK/config.yaml" "$PORT" "$LOG"

# run_client <名字> <描述> <函数>
# 支持 ONLY 过滤；函数返回 0=通过，77=跳过，其它=失败。
run_client() {
    name=$1; _desc=$2; shift 2
    if [ -n "$ONLY" ] && [ "$ONLY" != "$name" ]; then
        return 0
    fi
    say "$_desc"
    rc=0
    "$@" || rc=$?
    case "$rc" in
        0)  ok "$name" ;;
        77) skip "$name" "环境不满足" ;;
        *)  bad "$name" ;;
    esac
}

# ---------------------------------------------------------------- 客户端 1: smbclient
t_smbclient() {
    command -v smbclient >/dev/null 2>&1 || { echo "  跳过：缺少 smbclient"; return 77; }
    # 注意：smbclient 4.x 的 -c 不按换行拆分命令，必须用语义分隔符 `;`。
    CMD="ls; get hello.txt $WORK/got-hello.txt; put $WORK/config.yaml uploaded.yaml; mkdir newdir; rename uploaded.yaml renamed.yaml; rm renamed.yaml; rmdir newdir"
    smbclient "//127.0.0.1/public" -p "$PORT" -U "$USER%$PASS" -d1 -m SMB3 -c "$CMD" || return 1
    grep -q "hello from stupidsamba" "$WORK/got-hello.txt" || {
        echo "  下载内容不匹配"; return 1; }
    return 0
}

# ---------------------------------------------------------------- 客户端 2: impacket (Python)
t_impacket() {
    command -v python3 >/dev/null 2>&1 || { echo "  跳过：缺少 python3"; return 77; }
    # AGENTS.md §10.3 第 3 条：impacket 用 apt 装（python3-impacket），不要用 pip。
    python3 -c "import impacket" 2>/dev/null || {
        echo "  跳过：缺少 impacket 模块（apt-get install python3-impacket）"; return 77; }
    python3 "$ROOT/scripts/clients/impacket_test.py" \
        127.0.0.1 "$PORT" "$USER" "$PASS" public "$WORK" || return 1
    return 0
}

# ---------------------------------------------------------------- 客户端 4: mount.cifs (跳过)
t_mountcifs() {
    echo "  跳过：mount.cifs 在本容器永远跑不通（AGENTS.md §10.3 第 2 条："
    echo "        非初始 user namespace 内核禁止无 FS_USERNS_MOUNT 的文件系统挂载）。"
    echo "        在具备特权的真实主机上本用例应当通过，这里按环境限制 skip(77)。"
    return 77
}

run_client smbclient  "客户端 smbclient (Samba 官方 CLI)"        t_smbclient
run_client impacket   "客户端 impacket (Python 独立实现)"        t_impacket
run_client mountcifs  "客户端 mount.cifs (Linux 内核, 本环境跳过)" t_mountcifs

# ---------------------------------------------------------------- 汇总
say "结果汇总"
echo "  通过:$PASSED"
echo "  失败:$FAILED"
[ -n "$SKIPPED" ] && echo "  跳过:$SKIPPED"

if [ -n "$FAILED" ]; then
    echo
    echo "服务端日志（末尾 40 行）:"
    tail -40 "$LOG" | sed 's/^/    /'
    exit 1
fi

# AGENTS.md §3 门槛：需 smbclient / impacket / go-smb2 三家第三方栈通过。
# 本脚本覆盖前两家；go-smb2 由 test/integration/gosmb2_test.go 覆盖。
if [ -z "$ONLY" ]; then
    n=0; missing=""
    for c in smbclient impacket gosmb2; do
        # gosmb2 由集成测试负责，这里只统计本脚本能直接验证的两家；
        # 若只跑单用例（ONLY 指定），放宽门槛，不以缺 go-smb2 判失败。
        [ "$c" = "gosmb2" ] && continue
        case " $PASSED " in
            *" $c "*) n=$((n + 1)) ;;
            *)        missing="$missing $c" ;;
        esac
    done
    if [ "$n" -lt 2 ]; then
        echo "  验收未达标：smbclient/impacket 两家需全过，缺:$missing"
        exit 1
    fi
    echo "  验收通过（第三方客户端栈 $n/2 家 + go-smb2 见集成测试）"
else
    echo "  验收通过"
fi
