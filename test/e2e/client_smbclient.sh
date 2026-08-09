#!/bin/sh
# 冒烟客户端 1/3：smbclient（Samba 官方 CLI，AGENTS.md §3 矩阵第 1 项）
#
# 用法: client_smbclient.sh <host> <port> <user> <pass> <share> <workdir> <前缀>
# 退出码: 0 成功 / 1 失败 / 77 环境缺该工具（驱动会记为 SKIP）
#
# 本脚本只负责**发操作**并对目录枚举做断言。文件内容、磁盘副作用的判据
# 一律由 test/e2e/smoke.sh 在服务端磁盘上独立核对 —— 客户端说成功不算数。
#
# ⚠️ smbclient 4.x 的 -c **不按换行拆分命令**，多条命令必须用分号分隔。
#    写成多行会得到 `NT_STATUS_NO_SUCH_FILE listing \get` 这种**假故障**，
#    看着像服务端 bug，其实是测试脚本自己的问题（AGENTS.md §10.3 第 8 条）。
#    本脚本索性一次只发一条命令，失败时能精确定位到是哪个操作。

HOST=$1; PORT=$2; USER=$3; PASS=$4; SHARE=$5; WORK=$6; P=$7

command -v smbclient >/dev/null 2>&1 || exit 77

FAILED=0

# sc <说明> <一条 smbclient 命令>
# smbclient 在命令失败时**不总是**返回非零，所以这里额外扫描输出里的
# NT_STATUS_ 错误串。两个判据取或，漏一个都可能把失败当成功。
sc() {
    _what=$1; _cmd=$2
    _out=$(smbclient "//$HOST/$SHARE" -p "$PORT" -U "$USER%$PASS" -m SMB3 -d0 -c "$_cmd" 2>&1)
    _rc=$?
    if [ $_rc -ne 0 ] || echo "$_out" | grep -q "NT_STATUS_[A-Z_]*" ; then
        echo "  [smbclient] $_what 失败 (rc=$_rc):"
        echo "$_out" | sed 's/^/      /'
        FAILED=1
        return 1
    fi
    SC_OUT=$_out
    return 0
}

# --- 1. 目录枚举。磁盘看不出「客户端有没有看见」，所以这条只能在客户端断言。
if sc "ls" "ls"; then
    for want in "seed-$P.bin" "mov-$P.bin" "del-$P.bin" "rmd-$P" "subdir"; do
        echo "$SC_OUT" | grep -q "$want" || {
            echo "  [smbclient] 目录列表里没有 $want:"; echo "$SC_OUT" | sed 's/^/      /'; FAILED=1; }
    done
    # 反向断言：不存在的名字绝不能出现。防的是「列目录把请求里的名字原样回显」
    # 这类实现，那种实现能让上面所有正向断言全绿。
    if echo "$SC_OUT" | grep -q "nosuch-$P.bin"; then
        echo "  [smbclient] 目录列表里出现了根本不存在的 nosuch-$P.bin"; FAILED=1
    fi
fi

# --- 2. 下载。落到 workdir，由驱动做整字节比对。
sc "get" "get seed-$P.bin $WORK/got-$P.bin"

# --- 3. 上传。上传出来的 up-$P.bin 之后不再被任何操作碰，避免自毁证据。
sc "put" "put $WORK/up-$P.bin up-$P.bin"

# --- 4. 建目录。
sc "mkdir" "mkdir dir-$P"

# --- 5. 重命名（跨目录）。smbclient 用反斜杠作路径分隔符。
#     mov-$P.bin 是驱动预先放在共享目录里的，不是这里 put 出来的。
sc "rename" "rename mov-$P.bin dir-$P\moved-$P.bin"

# --- 6. 删文件（对象由驱动预先放好）。
sc "rm" "rm del-$P.bin"

# --- 7. 删目录（对象由驱动预先放好，是空目录）。
sc "rmdir" "rmdir rmd-$P"

exit $FAILED
