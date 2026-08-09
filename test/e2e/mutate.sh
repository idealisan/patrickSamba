#!/bin/sh
# mutate.sh —— 为「失败对照」生成故意改坏的服务端源码 overlay。
#
# 用法: mutate.sh <变异名> <工作目录>
#   成功时把 overlay JSON 的路径打到 stdout（其余输出一律走 stderr），
#   调用方拿它喂给 `go build -overlay <path>`。
#
# ---------------------------------------------------------------------------
# 这东西是干什么的
# ---------------------------------------------------------------------------
# 「测试全绿」本身不构成任何证据 —— 一个什么都不检查的测试也全绿。
# 有说服力的说法是：「我把服务端的 X 改坏了，判据 Y 当场变红」。
# 本脚本负责生产那个「改坏的服务端」，test/e2e/reverse-control.sh 负责跑对照。
#
# 关键设计：**绝不修改产品源码**。用 `go build -overlay` 在构建期把某个 .go
# 文件临时替换成改坏的副本，产品目录 internal/ 一个字节都不动，
# 也不需要 git stash/restore 之类容易把别人工作搞丢的操作。
#
# ---------------------------------------------------------------------------
# 锚点会失效，这是设计的一部分
# ---------------------------------------------------------------------------
# 变异靠在产品源码里匹配一行「锚点」实现。产品代码重构后锚点会匹配不上。
# 那时本脚本**必须大声失败**，绝不能静默生成一份没改动的副本 ——
# 那会让对照实验变成假阴性：变异根本没生效，套件当然还是绿的，
# 而我们会误判成「判据不管用」。所以每次变异后都强制校验标记确实出现了。
# 锚点失效时的正确做法：读一下那个函数，把锚点更新到新的写法上。
# ---------------------------------------------------------------------------

NAME=$1
WORK=$2

cd "$(dirname "$0")/../.." || exit 1
ROOT=$(pwd)

err() { printf '%s\n' "$*" >&2; }

if [ -z "$NAME" ] || [ -z "$WORK" ]; then
    err "用法: mutate.sh <变异名> <工作目录>"
    err "可用变异: $(sh "$0" --list 2>&1)"
    exit 2
fi

if [ "$NAME" = "--list" ]; then
    echo "write-corrupt read-corrupt rename-noop remove-noop readdir-hide"
    exit 0
fi

# 每个变异声明：改哪个文件、用哪条 sed、坏了会让谁变红。
# 注意所有替换文本里都带 `// MUTATION`，下面靠它校验变异确实生效了。
case "$NAME" in
write-corrupt)
    SRC="internal/vfs/local_handle.go"
    # 落盘前翻转最后一个字节。返回的 n 仍是完整长度，客户端看到的是
    # 一次**完全成功**的写入 —— 只有整字节比对才能发现。
    # 对照目标: <client>/put-bytes、<client>/rename-bytes
    EXPR='s|n, err := h.f.WriteAt(p, off)|__mut := append([]byte(nil), p...); if len(__mut) > 0 { __mut[len(__mut)-1] ^= 0xFF }; n, err := h.f.WriteAt(__mut, off) // MUTATION|'
    ;;
read-corrupt)
    SRC="internal/vfs/local_handle.go"
    # 读上来之后翻转最后一个字节。长度、状态码全对，只有内容错。
    # 对照目标: <client>/get-bytes
    EXPR='s|n, err := h.f.ReadAt(p, off)|n, err := h.f.ReadAt(p, off); if n > 0 { p[n-1] ^= 0xFF } // MUTATION|'
    ;;
rename-noop)
    SRC="internal/vfs/local.go"
    # 直接返回成功，什么都不搬。这是「回了 STATUS_SUCCESS 但事情没发生」
    # 的教科书形态，也是本项目反复踩过的那一类。
    # 对照目标: <client>/rename-old-gone、<client>/rename-new-disk
    EXPR='s|^func (l \*LocalFS) Rename(oldPath, newPath string, replace bool) error {$|&\n\treturn nil // MUTATION|'
    ;;
remove-noop)
    SRC="internal/vfs/local.go"
    # 同上，删除变成空操作。
    # 对照目标: <client>/rm-disk
    EXPR='s|^func (l \*LocalFS) Remove(p string) error {$|&\n\treturn nil // MUTATION|'
    ;;
readdir-hide)
    SRC="internal/vfs/local_handle.go"
    # 目录枚举里把种子文件藏起来。文件本身还在，能打开、能读 ——
    # 所以只有「客户端确实在列表里看见了它」这条断言会红。
    # 对照目标: <client>/client-run（客户端脚本内部的 ls 断言）
    EXPR='s|out = append(out, DirEntry{Name: name, Attr: \*a})|if len(name) < 5 or_marker name[:5] != "seed-" { out = append(out, DirEntry{Name: name, Attr: *a}) } // MUTATION|'
    # Go 里没有 `or_marker`，下面会替换成 `||`。sed 的替换文本里直接写 `||`
    # 没问题，但这里用占位符是为了让这条表达式在任何 shell 引号规则下都稳。
    POST='s|or_marker|\|\||'
    ;;
*)
    err "未知变异: $NAME"
    err "可用变异: $(sh "$0" --list)"
    exit 2
    ;;
esac

[ -f "$ROOT/$SRC" ] || { err "变异源文件不存在: $SRC（产品代码挪位置了？请更新 mutate.sh）"; exit 1; }

MUTDIR="$WORK/mutated"
mkdir -p "$MUTDIR" || exit 1
# 文件名带上变异名：overlay 只关心「原路径 -> 替换路径」的映射，替换文件叫什么
# 无所谓，但带上名字后同一个工作目录里多个变异不会互相覆盖，排查时也一眼看得出。
DST="$MUTDIR/$NAME.$(basename "$SRC")"

sed "$EXPR" "$ROOT/$SRC" > "$DST" || { err "sed 执行失败"; exit 1; }
if [ -n "$POST" ]; then
    sed "$POST" "$DST" > "$DST.tmp" && mv "$DST.tmp" "$DST" || { err "sed 后处理失败"; exit 1; }
fi

# 锚点校验 —— 见文件头「锚点会失效」。这一步不能省。
if ! grep -q "// MUTATION" "$DST"; then
    err "变异 '$NAME' 的锚点在 $SRC 里没匹配上，产品代码大概重构过了。"
    err "请打开该文件，把 mutate.sh 里对应的 sed 锚点更新到新写法。"
    err "（继续下去会生成一份**没改动**的副本，对照实验会假阴性 —— 故此处直接失败。）"
    exit 1
fi
if cmp -s "$ROOT/$SRC" "$DST"; then
    err "变异 '$NAME' 生成的副本与原文件完全一致，变异未生效。"
    exit 1
fi

OVERLAY="$WORK/overlay-$NAME.json"
printf '{"Replace":{"%s":"%s"}}\n' "$ROOT/$SRC" "$DST" > "$OVERLAY" || exit 1

err "  变异 '$NAME' 已生成: $SRC -> $DST"
echo "$OVERLAY"
