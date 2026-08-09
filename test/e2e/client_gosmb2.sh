#!/bin/sh
# 冒烟客户端 3/3 的启动壳：把 go-smb2 客户端跑起来。
#
# 单独一层壳的原因：client_gosmb2/ 是**独立 go module**（故意与主模块分离，
# 免得把客户端库依赖污染 stupidsamba 的 go.mod），必须 cd 进去才能 go run。
#
# 用法/退出码与另外两个客户端脚本一致，见 test/e2e/smoke.sh 的 run_case。
export PATH=/usr/local/go/bin:$PATH
command -v go >/dev/null 2>&1 || exit 77
cd "$(dirname "$0")/client_gosmb2" || exit 1
# 客户端是测试工具，不受 AGENTS.md C1「产物禁用 CGO」约束，但统一关掉更快。
CGO_ENABLED=0 go run . "$@"
