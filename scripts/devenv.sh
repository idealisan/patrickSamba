#!/bin/sh
# devenv.sh —— 开发环境削峰配置。**每个 agent 每开一个新 shell 都要先 `. scripts/devenv.sh`。**
#
# 为什么要有这个文件
# ------------------
# 本开发容器只有 2 核 / 4 GiB 内存，且**开不了交换分区**（缺 CAP_SYS_ADMIN，
# `swapon` 直接报「不允许的操作」，`/proc/sys/vm/*` 是只读，cgroup 也没暴露
# `memory.swap.max`）。所以唯一能做的是**限制峰值**。
#
# 崩溃的真实形态（已实测确认，别再猜）：
#   `IOT instruction (core dumped)` = SIGABRT = **进程自己 abort**，
#   这是 V8「JavaScript heap out of memory」的典型现场，**不是**容器 OOM-kill
#   （容器 OOM-kill 是 SIGKILL，不会有 core dump）。
#   实测 node v22 在本容器里的 `heap_size_limit` 是 **2096 MB**，
#   codebuddy 主进程稳态 RSS 约 550 MB，code-server 一系约 450 MB。
#   也就是说：**谁把大块数据塞进 CodeBuddy 的 JS 堆，谁就会把整个会话打崩。**
#
# 于是削峰分两条战线：
#   1. 别把大输出灌进 agent 上下文（见下面「输出纪律」）—— 这条最要命，也最容易犯。
#   2. 别让多个 agent 的 go build / go test 峰值叠在一起（见下面的 flock 串行化）。
#
# 用法
# ----
#   . scripts/devenv.sh          # 在你 worktree 的根目录 source 它
#   gocheck                      # 代替裸的 build+vet+gofmt+test 四连
#   gobuild                      # 只编译
#   golock <任意命令>            # 给任意吃内存的命令加全局串行锁

# ------------------------------------------------------------------ 基础环境

# Go 不在默认 PATH 里（容器重启后也一样，别指望它自己在）。
case ":$PATH:" in
    *:/usr/local/go/bin:*) : ;;
    *) PATH="$PATH:/usr/local/go/bin" ;;
esac
export PATH

# C1 硬性约束：全项目 CGO_ENABLED=0。放这里省得每条命令都写一遍。
export CGO_ENABLED=0

# 编译动作串行。默认 go 会按 CPU 数并行编译，2 核 × 每个编译进程几百 MB，
# 再乘以同时干活的 agent 数，峰值很容易把内存吃干。
export GOFLAGS="-p=1"

# 限制 go 工具链与被编译产物自身的调度并行度。
export GOMAXPROCS=2

# 所有 worktree 共用同一份构建缓存（默认就是 ~/.cache/go-build，这里显式钉住），
# 9 个 agent 各自 worktree 但同一套依赖，命中缓存能省掉大量重复编译。
export GOCACHE="${GOCACHE:-/root/.cache/go-build}"

# ------------------------------------------------------------------ 全局串行锁

# 所有 worktree 共用 /tmp 下的同一把锁，所以跨 agent 生效。
SS_GO_LOCK=/tmp/stupidsamba-go.lock

# golock <命令...> —— 拿到全局锁再执行，避免多个 agent 的重活峰值叠加。
# 等锁最多 20 分钟；等超了直接失败而不是无锁硬闯（硬闯就失去意义了）。
golock() {
    flock -w 1200 "$SS_GO_LOCK" "$@"
}

# gobuild —— 最小可提交门槛：能编译。
gobuild() {
    golock go build ./...
}

# gocheck —— 提交前必须全过的四连。gofmt 有输出即视为失败。
gocheck() {
    golock sh -c '
        set -e
        go build ./...
        go vet ./...
        out=$(gofmt -l .)
        if [ -n "$out" ]; then
            echo "gofmt 未通过，以下文件需要格式化：" >&2
            echo "$out" >&2
            exit 1
        fi
        go test -p 1 ./...
    '
}

# gocross —— 跨平台编译校验（C7）。比 gocheck 更吃内存，单独一条。
gocross() {
    golock sh -c '
        set -e
        for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do
            GOOS=${t%/*} GOARCH=${t#*/} go build ./... || exit 1
            echo "  ok  $t"
        done
    '
}

# ------------------------------------------------------------------ git 削峰

# git 的重打包（gc / repack）是本仓库里除 go 之外最大的内存尖峰，
# 而且 `git gc --aggressive` 尤其凶。把它按住：
#   - windowMemory：单个 delta 窗口的内存上限
#   - threads=1：不要按核数并行，本机只有 2 核，并行收益小、峰值翻倍
#   - bigFileThreshold：超过这个大小的对象不做 delta 压缩（本仓库有 149 MB 的
#     历史快照目录，对它做 delta 纯属自找麻烦）
# 只对本仓库生效（--local），不动全局配置。
if [ -d .git ] || git rev-parse --git-dir >/dev/null 2>&1; then
    git config --local pack.windowMemory 64m
    git config --local pack.threads 1
    git config --local pack.packSizeLimit 512m
    git config --local core.bigFileThreshold 16m
fi

# ------------------------------------------------------------------ 输出纪律（人肉约定，脚本管不了）

# 下面这些没法用脚本强制，但它们是**最主要**的崩溃来源，请自觉遵守：
#
#   1. 永远不要在工具调用里无界地输出大文件。`cat` 一个几 MB 的文件、
#      `git log -p` 不加限制、`go test -v ./...` 全量输出——这些整块进 JS 堆。
#      一律加 `| head -N` / `| tail -N` / `--stat` / `-n 20` 之类的界。
#   2. 读代码用 Read 工具（它有分页），不要用 `cat`。
#   3. 搜索用 Grep 工具并带 `head_limit`，不要 `grep -r` 全量打印。
#   4. 长时间命令用 `run_in_background`，然后**带 filter 或 tail 取结果**，
#      不要把几万行日志整个捞回来。
#   5. 抓包、core dump、二进制产物：只报大小和摘要，不要往上下文里贴内容。
#
# 判断标准很简单：**任何一次工具输出超过几百行，就说明你少加了一个界。**

if [ -n "${PS1:-}" ] || [ "${SS_DEVENV_QUIET:-0}" != "1" ]; then
    printf 'devenv: PATH/CGO/GOFLAGS 已设置；重活请用 gobuild / gocheck / gocross（全局串行锁 %s）\n' "$SS_GO_LOCK"
fi
