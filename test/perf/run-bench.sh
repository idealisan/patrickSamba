#!/bin/sh
# run-bench.sh —— stupidSamba 性能基准的一键驱动（P0 统一度量）。
#
# 用法：
#   sh test/perf/run-bench.sh <共享根目录> [signing|encryption] [基准程序额外参数...]
#
# 它做五件事：构建服务端 → 生成配置（4455 端口、mdns 关闭）→ 启动并等监听就绪
# → 跑 large(1/16 并发)+small 场景 → 收尾杀进程。
# 结果 JSON/SUMMARY 全部打到 stdout，重定向到文件即为一份可对比记录。
#
# 环境变量：
#   PERF_OUT   工作产物目录（默认 /tmp/opencode/perf-w2）
#   PERF_MODE  基准场景（默认 all；透传给 perf-bench -mode）
set -eu

REPO=$(cd "$(dirname "$0")/../.." && pwd)
SHARE_ROOT=${1:?用法: run-bench.sh <共享根目录> [signing|encryption] [额外参数...]}
SECURITY=${2:-signing}
shift 2 || true

OUT=${PERF_OUT:-/tmp/opencode/perf-w2}
PORT=4455
BIN="$OUT/stupidsamba${PORT}"
CFG="$OUT/cfg-${SECURITY}.yaml"

export PATH=$PATH:/usr/local/go/bin
mkdir -p "$OUT"

echo "== 构建 $(date '+%F %T')"
(cd "$REPO" && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X main.version=perf -X main.commit=$(git rev-parse --short HEAD)" \
    -o "$BIN" ./cmd/stupidsamba)
(cd "$REPO" && CGO_ENABLED=0 go build -o "$OUT/perf-bench" ./test/perf)

case "$SECURITY" in
  signing)    SEC_LINES="signing_required: true";;
  encryption) SEC_LINES="encryption_required: true";;
  *) echo "未知安全模式: $SECURITY" >&2; exit 2;;
esac

cat > "$CFG" <<EOF
filesystem_mode: auto
server:
  name: PERFSRV
  min_dialect: "3.1.1"
  max_dialect: "3.1.1"
  ${SEC_LINES}
listen:
  addresses:
    - 127.0.0.1
  port: ${PORT}
auth:
  users:
    - name: perf
      password: "perfpass123"
shares:
  - name: public
    path: ${SHARE_ROOT}
    read_only: false
mdns:
  enabled: false
log:
  level: warn
EOF

"$BIN" -config "$CFG" -check

if fuser "${PORT}/tcp" 2>/dev/null; then
  echo "端口 ${PORT} 已被占用，拒绝启动（先处理旧进程）" >&2
  exit 1
fi

echo "== 启动服务端 ${SECURITY} 模式 $(date '+%F %T')"
setsid nohup "$BIN" -config "$CFG" >> "$OUT/server-${SECURITY}.log" 2>&1 < /dev/null &
i=0
until (exec 3<>/dev/tcp/127.0.0.1/${PORT}) 2>/dev/null; do
  i=$((i+1))
  if [ "$i" -gt 50 ]; then
    echo "服务端 10s 内未监听 ${PORT}，日志尾部：" >&2
    tail -20 "$OUT/server-${SECURITY}.log" >&2
    exit 1
  fi
  sleep 0.2
done
exec 3>&- 2>/dev/null || true

echo "== 跑基准 $(date '+%F %T')  mode=${PERF_MODE:-all}"
"$OUT/perf-bench" -server "127.0.0.1:${PORT}" -user perf -pass perfpass123 \
    -mode "${PERF_MODE:-all}" -tag "${SECURITY}-$(git -C "$REPO" rev-parse --short HEAD)" "$@"
RC=$?

echo "== 收尾 $(date '+%F %T')"
pkill -x "stupidsamba${PORT}" || true
sleep 0.5
exit $RC
