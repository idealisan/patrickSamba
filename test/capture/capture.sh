#!/bin/sh
# 采集真实 Samba 的线上字节，产出 internal/smb/wire/testdata/capture/<场景>/ 下的 golden fixture。
#
# 拓扑：
#
#   真实第三方客户端 ──► captureproxy(:4451) ──► 真实 smbd(:4450)
#   smbclient / gosmb2       逐帧转储               Samba 官方实现
#
# 为什么不用 tcpdump/tshark：需要 root/CAP_NET_RAW，还要自己做 TCP 重组；
# 而 SMB over Direct TCP 是「4 字节大端长度前缀 + 报文」的定长分帧，
# 应用层代理天然拿到干净的完整帧，零权限要求。详见 test/capture/proxy.go 头注释。
#
# 用法（需要本机装了 samba + smbclient，仅采集时需要）：
#
#   ./test/capture/capture.sh              # 采集全部场景
#   ./test/capture/capture.sh query-info   # 只采集指定场景
#
# 采集完的 fixture 会提交进仓库，**跑 go test 时不需要 samba**。
set -eu

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
OUT="$REPO/internal/smb/wire/testdata/capture"

WORK="${CAPTURE_WORK:-/tmp/sambatest}"
SMBD_PORT="${SMBD_PORT:-4450}"
PROXY_PORT="${PROXY_PORT:-4451}"
USER_NAME="testuser"
USER_PASS="testpass123"
SHARE="capture"

PROXY_BIN=/tmp/captureproxy

log() { printf '\n=== %s ===\n' "$*" >&2; }

# --------------------------------------------------------------------------
# 1. 准备参照 smbd（独立目录、非 445 端口，绝不碰系统 samba 状态）
# --------------------------------------------------------------------------
#
# 参数 $1 是 `smb encrypt` 的取值：
#   desired —— 与 SMB3 客户端协商出加密，SESSION_SETUP 之后全是 TRANSFORM_HEADER。
#              只有采 negotiate 的加密 context 时才需要。
#   off     —— 全程明文，报文体才能逐字节比对。数据面场景一律用它。
# 注意：客户端侧的 `--option=client smb encrypt=off` 在 Samba 4.22 上**压不住**
# 服务端的 desired（实测仍然加密），所以只能从服务端关。
setup_smbd() {
	enc="${1:-off}"
	mkdir -p "$WORK"/share/subdir "$WORK"/private "$WORK"/lock "$WORK"/state \
		"$WORK"/cache "$WORK"/run "$WORK"/log "$WORK"/ncalrpc

	cat >"$WORK/smb.conf" <<EOF
[global]
	workgroup = WORKGROUP
	server string = stupidsamba-capture-ref
	netbios name = CAPTUREREF
	server min protocol = SMB2_02
	server max protocol = SMB3_11
	smb ports = $SMBD_PORT
	private dir = $WORK/private
	lock directory = $WORK/lock
	state directory = $WORK/state
	cache directory = $WORK/cache
	pid directory = $WORK/run
	ncalrpc dir = $WORK/ncalrpc
	usershare path =
	log file = $WORK/log/smbd.log
	log level = 1
	disable spoolss = yes
	load printers = no
	printing = bsd
	printcap name = /dev/null
	passdb backend = tdbsam
	security = user
	map to guest = never
	smb encrypt = $enc
	server signing = auto
	interfaces = 127.0.0.1
	bind interfaces only = yes
	disable netbios = yes
	multicast dns register = no

[$SHARE]
	path = $WORK/share
	read only = no
	guest ok = no
	valid users = $USER_NAME
EOF

	# 参照服务器需要一个宿主用户（这是 Samba 的要求，与本项目 C8「认证自成体系」无关，
	# stupidSamba 自己永远不读 /etc/passwd）。
	id "$USER_NAME" >/dev/null 2>&1 || useradd -M -s /usr/sbin/nologin "$USER_NAME"
	printf '%s\n%s\n' "$USER_PASS" "$USER_PASS" |
		smbpasswd -c "$WORK/smb.conf" -a -s "$USER_NAME" >/dev/null

	printf 'hello stupidsamba capture\n' >"$WORK/share/hello.txt"
	head -c 4096 /dev/urandom >"$WORK/share/blob.bin"
	for i in 1 2 3; do printf 'file %s\n' "$i" >"$WORK/share/f$i.dat"; done
	chown -R "$USER_NAME" "$WORK/share"

	pkill -f "smbd .*$WORK/smb.conf" 2>/dev/null || true
	sleep 1
	smbd -F --debug-stdout -s "$WORK/smb.conf" --debuglevel=1 >"$WORK/log/stdout.log" 2>&1 &
	SMBD_PID=$!
	sleep 2
	smbclient "//127.0.0.1/$SHARE" -U "$USER_NAME%$USER_PASS" -p "$SMBD_PORT" \
		-m SMB3 -c 'ls' >/dev/null 2>&1 ||
		{ echo "参照 smbd 起不来，看 $WORK/log/stdout.log" >&2; exit 1; }
	log "参照 smbd 就绪 pid=$SMBD_PID port=$SMBD_PORT"
}

teardown_smbd() {
	[ -n "${SMBD_PID:-}" ] && kill "$SMBD_PID" 2>/dev/null || true
}

# --------------------------------------------------------------------------
# 2. 采集一个场景：起代理 → 跑客户端 → 代理空闲自退（退出时才写 manifest）
# --------------------------------------------------------------------------
# 用法：capture <场景名> <客户端命令...>
capture() {
	scen="$1"
	shift
	log "场景 $scen"
	"$PROXY_BIN" -listen "127.0.0.1:$PROXY_PORT" -upstream "127.0.0.1:$SMBD_PORT" \
		-out "$OUT" -scenario "$scen" -idle 4s &
	proxy_pid=$!
	sleep 0.5
	"$@" >/dev/null 2>&1 || echo "  (客户端返回非 0，帧照样已记录)" >&2
	wait "$proxy_pid" 2>/dev/null || true
}

sc() { # smbclient 连 share
	smbclient "//127.0.0.1/$SHARE" -U "$USER_NAME%$USER_PASS" -p "$PROXY_PORT" "$@"
}

# --------------------------------------------------------------------------
run_scenario() {
	case "$1" in
	negotiate-smb202)
		# SMB 2.0.2：无 negotiate context，NegotiateResponse 定长，最干净的基准。
		capture negotiate-smb202 sc -m SMB2_02 -c 'ls'
		;;
	negotiate-smb210)
		# SMB 2.1：多了 LARGE_MTU / leasing 能力位。
		capture negotiate-smb210 sc -m SMB2_10 -c 'ls'
		;;
	negotiate-smb311)
		# SMB 3.1.1：带 negotiate context（preauth integrity / encryption / signing），
		# 8 字节对齐规则、最后一个 context 不补尾 pad —— 这些是最容易写错的地方。
		# 这一档**服务端开加密**，才能拿到完整的 SMB2_ENCRYPTION_CAPABILITIES context；
		# 加密要到 SESSION_SETUP 成功之后才生效，所以 NEGOTIATE 帧本身仍是明文。
		setup_smbd desired
		capture negotiate-smb311 sc -m SMB3_11 -c 'quit'
		setup_smbd off
		;;
	session-setup)
		# NTLMSSP 三段握手包在 SPNEGO 里：negotiate → challenge(STATUS_MORE_PROCESSING_REQUIRED) → auth。
		capture session-setup sc -m SMB3 -c 'quit'
		;;
	tree-connect)
		# smbclient -L 会连 IPC$ 并用 IOCTL/FSCTL_PIPE_TRANSCEIVE 走 srvsvc NetShareEnumAll，
		# 顺带把 DCERPC bind/request 的真实字节采下来（交给 auth 模块做 srvsvc 参考）。
		capture tree-connect smbclient -L "//127.0.0.1" -p "$PROXY_PORT" \
			-U "$USER_NAME%$USER_PASS" -m SMB3
		;;
	create-read-write)
		printf 'stupidsamba write path fixture payload\n' >/tmp/capture-upload.txt
		capture create-read-write sc -m SMB3 \
			-c 'put /tmp/capture-upload.txt uploaded.txt; get uploaded.txt /tmp/capture-download.txt; rm uploaded.txt'
		;;
	query-directory)
		# ls 走 FileIdBothDirectoryInformation(0x25)，重点看 8 字节对齐与最后一项 NextEntryOffset=0。
		capture query-directory sc -m SMB3 -c 'ls *; ls subdir\*'
		;;
	query-info)
		# allinfo 会打一串 QUERY_INFO：FileAllInformation / FileStreamInformation 等。
		capture query-info sc -m SMB3 -c 'allinfo hello.txt; ls'
		;;
	set-info)
		# rename → SET_INFO/FileRenameInformation（注意文件名不带前导反斜杠）；
		# setmode → FileBasicInformation；rm → FileDispositionInformation。
		printf 'set-info fixture\n' >/tmp/capture-si.txt
		capture set-info sc -m SMB3 \
			-c 'put /tmp/capture-si.txt si.txt; rename si.txt si2.txt; setmode si2.txt +r; setmode si2.txt -r; rm si2.txt'
		;;
	gosmb2)
		# 第三方 Go 客户端库（hirochachacha/go-smb2），与 smbclient 是两套独立实现，
		# 两边都能对上才说明我们的理解不是被单一实现带偏的。
		capture gosmb2 "$REPO/scripts/clients/gosmb2/gosmb2client" \
			"127.0.0.1:$PROXY_PORT" "$USER_NAME" "$USER_PASS" "$SHARE"
		;;
	*)
		echo "未知场景: $1" >&2
		exit 1
		;;
	esac
}

ALL="negotiate-smb202 negotiate-smb210 negotiate-smb311 session-setup tree-connect create-read-write query-directory query-info set-info gosmb2"

CGO_ENABLED=0 "$GO" build -o "$PROXY_BIN" "$REPO/test/capture"
trap teardown_smbd EXIT
setup_smbd off

if [ $# -gt 0 ]; then
	for s in "$@"; do run_scenario "$s"; done
else
	for s in $ALL; do run_scenario "$s"; done
fi

log "采集完成，产物在 $OUT"
