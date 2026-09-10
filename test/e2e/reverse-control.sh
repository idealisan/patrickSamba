#!/bin/sh
# reverse-control.sh —— 冒烟套件的**失败对照**实验。
#
# 用法: test/e2e/reverse-control.sh [变异名...]      （不给参数就跑全部）
#
# ---------------------------------------------------------------------------
# 为什么需要这个
# ---------------------------------------------------------------------------
# 「smoke.sh 全绿」本身不是证据。一个 `exit 0` 也全绿。
# 唯一有说服力的说法是：
#
#     我把服务端的 X 改坏了，判据 Y 当场变红；把 X 改回来，Y 又绿了。
#
# 本脚本就是在做这件事：对每个变异（见 test/e2e/mutate.sh）跑一遍 smoke.sh，
# 断言两件事 ——
#
#   ①（整体）套件必须 FAIL。只是变红还不够，因为可能红在别的地方。
#   ②（定点）**预期的那几条判据** ID 必须出现在失败清单里。
#
# ② 是重点。项目里踩过的坑是「探针失败了，但失败原因和被测的东西无关」，
# 于是得到一个看起来正确、实际毫无信息量的结论。定点断言把这种情况挡掉。
#
# 反过来，如果某个变异跑完套件居然还是绿的，那说明**我们的判据是瞎的**，
# 服务端的那条路坏掉我们根本发现不了 —— 这时必须去补判据，而不是删掉这个变异。
# ---------------------------------------------------------------------------

cd "$(dirname "$0")/../.." || exit 1
ROOT=$(pwd)
export PATH=/usr/local/go/bin:$PATH

# 对照端口与常规冒烟错开，免得两边同时跑互相抢端口。
PORT=${SMB_PORT_RC:-4473}

ALL="write-corrupt read-corrupt rename-noop remove-noop doc-noop readdir-hide"
LIST=${*:-$ALL}

# 每个变异**必须**打红的判据（空格分隔）。这些 ID 由 smoke.sh 输出，
# 命名规则 <客户端>/<判据>。三个客户端各自独立走一遍，所以下面按三家都列 ——
# 一个真实的服务端 bug 不该只对某一家客户端可见。
expected_for() {
    case "$1" in
    write-corrupt)
        # 写进磁盘的字节被翻了一位：put 上去的内容对不上。
        echo "smbclient/put-bytes impacket/put-bytes gosmb2/put-bytes" ;;
    read-corrupt)
        # 读出来的字节被翻了一位：get 下来的内容对不上。
        echo "smbclient/get-bytes impacket/get-bytes gosmb2/get-bytes" ;;
    rename-noop)
        # 回了成功但没搬：旧名还在、新名没有。
        echo "smbclient/rename-old-gone smbclient/rename-new-disk \
              impacket/rename-old-gone impacket/rename-new-disk \
              gosmb2/rename-old-gone gosmb2/rename-new-disk" ;;
    remove-noop)
        # LocalFS.Remove 回了成功但没删。
        #
        # 这里的期望清单是**对照实验自己纠正出来的**，过程值得留档：
        # 最初想当然地把六条 rm/rmdir 全列上，结果 smbclient/rm-disk 与
        # impacket/rm-disk 没红。查下去发现删除有两条路径（见 mutate.sh 的
        # remove-noop 注释）：这两家删**文件**走的是 CREATE 带
        # FILE_DELETE_ON_CLOSE 的句柄路径，根本不经过 LocalFS.Remove。
        # 那条路径由下面的 doc-noop 单独盯。
        #
        # 换句话说：判据没问题，是**变异打偏了**。区分这两者是本脚本的核心价值 ——
        # 如果当初图省事把没红的两条从期望里删掉，就会白白错过「删除有两条
        # 代码路径」这个事实，而其中一条将永远没有对照覆盖。
        echo "smbclient/rmdir-disk impacket/rmdir-disk \
              gosmb2/rm-disk gosmb2/rmdir-disk" ;;
    doc-noop)
        # 句柄自持的 delete-on-close 路径被掐断。只有走那条路的两家会红。
        echo "smbclient/rm-disk impacket/rm-disk" ;;
    readdir-hide)
        # 文件还在、能打开能读，只是不出现在目录列表里。
        # 只有客户端脚本内部的 ls 断言能发现 —— 它体现为 client-run 变红。
        echo "smbclient/client-run impacket/client-run gosmb2/client-run" ;;
    *)  echo "" ;;
    esac
}

RC=0
SUMMARY=""

# client_ran 报告某个客户端在基线里**真的执行过**判据。
#
# smoke.sh 对缺席的客户端打印 `[SKIP] <client>/* (环境缺少该客户端)`，
# 对跑过的打印 `[PASS]/[FAIL] <client>/<判据>`。二者互斥且都出现在基线里，
# 所以「没被 SKIP 且至少有一条判据」= 真跑了。
client_ran() {
    [ -n "$1" ] || return 1
    printf '%s\n' "$BASE_PLAIN" | grep -q "\[SKIP\] $1/" && return 1
    printf '%s\n' "$BASE_PLAIN" | grep -qE "\[(PASS|FAIL)\] $1/"
}

printf '\n\033[1;36m########## 冒烟判据失败对照实验 ##########\033[0m\n'

# ---------------------------------------------------------------- 基线
#
# 先证明没变异时套件是绿的。少了这一步，「变异后变红」什么也说明不了 ——
# 万一它本来就是红的呢。这是对照实验的另一半，不能省。
printf '\n\033[1;36m===== 基线（无变异，必须 PASS）=====\033[0m\n'
BASE_OUT=$(SMB_PORT=$PORT sh "$ROOT/test/e2e/smoke.sh" 2>&1)
BASE_RESULT=$(echo "$BASE_OUT" | sed -n 's/^SMOKE-RESULT: //p')
# BASE_PLAIN 是剥掉 ANSI 颜色码的基线输出，下面所有文本匹配都基于它。
#
# 不剥会踩到一个很隐蔽的坑：smoke.sh 打印的是
#   \033[1;32m  [PASS]\033[0m smbclient/get-bytes
# 颜色复位码夹在 `[PASS]` 与判据 ID **之间**，于是 `grep -E '\[PASS\] smbclient/'`
# 永远匹配不上 —— 表现为「每个客户端都被判成缺席」，而肉眼看着满屏都是 PASS。
BASE_PLAIN=$(printf '%s\n' "$BASE_OUT" | sed "s/$(printf '\033')\[[0-9;]*m//g")
# 一个客户端都没跑起来时，后面的对照实验全是空转：变异再怎么改坏服务端，
# 也不会有任何判据变红，而脚本会得出「判据瞎了」以外的结论 —— 那是假的。
# 所以这里把「零客户端」当成硬失败，而不是当成「全部通过」。
if ! printf '%s\n' "$BASE_PLAIN" | grep -qE "\[(PASS|FAIL)\] [a-z0-9]+/"; then
    printf '\033[1;31m  [BAD] 基线里没有任何客户端真的跑了判据 —— 对照实验无意义\033[0m\n'
    echo "$BASE_OUT" | tail -25 | sed 's/^/      /'
    SUMMARY="$SUMMARY\n  [BAD]  基线            无客户端执行，对照实验作废"
    RC=1
fi
if [ "$BASE_RESULT" = "PASS" ]; then
    printf '\033[1;32m  [OK] 基线 PASS\033[0m\n'
    SUMMARY="$SUMMARY\n  [OK]   基线            套件 PASS"
else
    printf '\033[1;31m  [BAD] 基线不是 PASS（结果=%s），后面的对照全部无意义\033[0m\n' "$BASE_RESULT"
    echo "$BASE_OUT" | tail -25 | sed 's/^/      /'
    SUMMARY="$SUMMARY\n  [BAD]  基线            套件未通过，对照实验作废"
    RC=1
fi

# ---------------------------------------------------------------- 逐个变异

for m in $LIST; do
    printf '\n\033[1;36m===== 变异: %s（必须 FAIL）=====\033[0m\n' "$m"

    OUT=$(MUTATE="$m" SMB_PORT=$PORT sh "$ROOT/test/e2e/smoke.sh" 2>&1)
    RESULT=$(echo "$OUT" | sed -n 's/^SMOKE-RESULT: //p')
    GOTFAIL=$(echo "$OUT" | sed -n 's/^SMOKE-FAILED://p')

    if [ -z "$RESULT" ]; then
        printf '\033[1;31m  [BAD] 套件没有输出 SMOKE-RESULT，多半是没跑起来\033[0m\n'
        echo "$OUT" | tail -25 | sed 's/^/      /'
        SUMMARY="$SUMMARY\n  [BAD]  $m 套件未产出结果"
        RC=1
        continue
    fi

    # ① 整体必须红
    if [ "$RESULT" != "FAIL" ]; then
        printf '\033[1;31m  [BAD] 服务端已被改坏，套件却仍然 %s —— 我们的判据是瞎的！\033[0m\n' "$RESULT"
        SUMMARY="$SUMMARY\n  [BAD]  $m 改坏了也没红（判据缺失）"
        RC=1
        continue
    fi
    printf '  套件按预期 FAIL，实际红的判据:%s\n' "$GOTFAIL"

    # ② 必须红在**预期的那几条**上
    #
    # 只校验**本次真的跑了**的客户端那几条。
    #
    # 原因（实测踩到）：期望清单按三家客户端写死，而 impacket 在本容器装了也
    # 未必能被 python3 找到（/usr/local/bin 抢占 python3 那个坑，见仓库最近的
    # 文档），smoke.sh 会 [SKIP] 掉这一家。此时 impacket/* 那几条**永远不会红**，
    # 于是六个变异全部判成「红在别处」，实验整体红 —— 看起来像判据瞎了，
    # 实际是一家客户端缺席。把缺席当失败，等于让这道门禁的结果取决于构建机
    # 装没装 python 包，这既不稳定也掩盖真正该报警的信号。
    #
    # 处理方式：缺席的客户端**警告但不判红**（覆盖缺口要看得见），跑了的客户端
    # 一律严格 —— 它那份期望一条都不能少。
    MISSING=""
    SKIPPED_WANT=""
    for want in $(expected_for "$m"); do
        client=${want%%/*}
        if ! client_ran "$client"; then
            case " $SKIPPED_WANT " in
                *" $client "*) : ;;
                *) SKIPPED_WANT="$SKIPPED_WANT $client" ;;
            esac
            continue
        fi
        case " $GOTFAIL " in
            *" $want "*) : ;;
            *) MISSING="$MISSING $want" ;;
        esac
    done
    if [ -n "$SKIPPED_WANT" ]; then
        printf '\033[1;33m  [WARN] 客户端缺席，这几家的对照未覆盖:%s\033[0m\n' "$SKIPPED_WANT"
        printf '\033[1;33m         （覆盖缺口；装上后这里的期望会自动生效）\033[0m\n'
        SUMMARY="$SUMMARY\n  [WARN] $m 客户端缺席未覆盖:$SKIPPED_WANT"
    fi
    if [ -n "$MISSING" ]; then
        printf '\033[1;31m  [BAD] 红是红了，但预期的判据没红:%s\033[0m\n' "$MISSING"
        printf '\033[1;31m        （红在别处 = 对照实验没证明想证明的东西）\033[0m\n'
        SUMMARY="$SUMMARY\n  [BAD]  $m 红在别处，预期判据未触发"
        RC=1
    else
        printf '\033[1;32m  [OK] 预期判据全部按预期变红\033[0m\n'
        SUMMARY="$SUMMARY\n  [OK]   $m 预期判据全部变红"
    fi
done

printf '\n\033[1;36m########## 对照实验汇总 ##########\033[0m'
printf '%b\n' "$SUMMARY"
if [ $RC -eq 0 ]; then
    printf '\033[1;32m\n结论: 冒烟判据具备可证伪性 —— 每个变异都被定点抓住。\033[0m\n'
else
    printf '\033[1;31m\n结论: 存在抓不住的变异，判据需要补强（见上面的 [BAD]）。\033[0m\n'
fi
exit $RC
