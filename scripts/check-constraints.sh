#!/bin/sh
# check-constraints.sh —— 校验 AGENTS.md 里的硬性约束（C1 / C3 / C4 / C6 / C8 / C9）。
#
# 用法：scripts/check-constraints.sh
#
# CI 会跑它，agent 提交前也可以本地跑。
#
# 设计要点：**不要用裸文本 grep**。
#   注释里出现 "/etc/passwd" 这种词是完全正常的（约束文档本身就要写它），
#   裸 grep 会把说明性注释当成违规，制造假阳性，逼得大家去改注释而不是改代码。
#   所以：
#     - 能用依赖图判定的（import），就用 `go list -deps`，无法被注释欺骗；
#     - 只能文本判定的（路径字符串、外部命令名），要求命中**双引号字符串字面量内部**。
set -e

cd "$(dirname "$0")/.."
export PATH=/usr/local/go/bin:$PATH
export CGO_ENABLED=0

bad=0
fail() { printf '\033[1;31m错误: %s\033[0m\n' "$1" >&2; bad=1; }
pass() { printf '\033[1;32mOK: %s\033[0m\n' "$1"; }

# ---------------------------------------------------------------- C1 禁止 CGO
#
# 依赖图里出现 runtime/cgo 就说明有人引了 CGO 包。
# 这比 grep 'import "C"' 可靠：间接依赖也能抓到。

deps=$(go list -deps ./... 2>/dev/null)

if printf '%s\n' "$deps" | grep -qx 'runtime/cgo'; then
    fail "依赖图中出现 runtime/cgo，违反 C1（禁止 CGO）"
else
    pass "C1 无 CGO 依赖"
fi

# ---------------------------------------------------------------- C8 认证自成体系
#
# 禁止 import 任何做系统用户/凭据解析的包。

for pkg in os/user; do
    if printf '%s\n' "$deps" | grep -qx "$pkg"; then
        fail "依赖图中出现 $pkg，违反 C8（认证不得依赖操作系统用户管理）"
    fi
done

# 把整行注释**清空**（而不是删掉）再匹配。
# 清空是为了保留行号：删行会让报出来的行号比真实行号小，
# 拿着去翻文件找不到对应代码，等于报了个没法用的位置。
strip_comments() { sed -E 's|^[[:space:]]*(//\|\*\|/\*).*$||' "$1"; }

# 禁止在字符串字面量里出现系统账户数据库路径。
# 要求命中双引号内部，且先剥掉注释 —— 约束文档本身就要写这些路径，
# 一句 `// 禁止读 "/etc/shadow"` 的说明性注释不该把 CI 判红。
#
# **_test.go 豁免**：`/etc/passwd` 是路径穿越攻击的经典目标，
# 安全测试的攻击向量里就该出现它。把测试向量改成别的路径只会削弱测试，
# 属于为了让检查变绿而破坏代码 —— 正好是这个检查要防止的反面。
# 真正的 C8 违规（运行时去读系统账户库）由上面的依赖图检查兜底。
for path in /etc/passwd /etc/shadow /etc/group /etc/krb5.conf; do
    hits=$(
        find . -name '*.go' -not -name '*_test.go' -not -path './.git/*' | while read -r f; do
            strip_comments "$f" | grep -n -- "\"[^\"]*$path" 2>/dev/null | sed "s|^|$f:|"
        done
    )
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        fail "非测试代码的字符串字面量中出现 $path，违反 C8"
    fi
done

# 禁止 NSS / PAM / 系统登录 API 的符号。
for sym in getpwnam getgrnam getpwuid getgrgid pam_authenticate pam_start LogonUserW ODNodeCopyRecord; do
    hits=$(
        find . -name '*.go' -not -path './.git/*' | while read -r f; do
            strip_comments "$f" | grep -n -w -- "$sym" 2>/dev/null | sed "s|^|$f:|"
        done
    )
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        fail "代码中出现 $sym，违反 C8"
    fi
done

[ "$bad" = "0" ] && pass "C8 认证自成体系"

# ---------------------------------------------------------------- C3 禁止外部进程
#
# 运行时代码（internal/、cmd/）不得 fork/exec 外部命令。
# scripts/ 和 _test.go 是开发/测试用的，不受此限。

exechits=$(
    find ./internal ./cmd -name '*.go' -not -name '*_test.go' 2>/dev/null | while read -r f; do
        strip_comments "$f" | grep -n -E 'os/exec|exec\.Command' 2>/dev/null | sed "s|^|$f:|"
    done
)
if [ -n "$exechits" ]; then
    printf '%s\n' "$exechits"
    fail "运行时代码中出现 os/exec，违反 C3（禁止依赖外部进程）"
else
    pass "C3 运行时无外部进程调用"
fi

# ---------------------------------------------------------------- C4 mDNS 不得走系统服务

dbushits=$(
    find ./internal ./cmd -name '*.go' -not -name '*_test.go' 2>/dev/null | while read -r f; do
        strip_comments "$f" | grep -n -E 'godbus|dbus\.|avahi|/var/run/avahi' 2>/dev/null | sed "s|^|$f:|"
    done
)
if [ -n "$dbushits" ]; then
    printf '%s\n' "$dbushits"
    fail "运行时代码中出现 D-Bus/avahi 调用，违反 C4（mDNS 必须自实现）"
else
    pass "C4 mDNS 无系统服务依赖"
fi

# ------------------------------- C9 操作系统只作为「文件系统 + 套接字」提供方
#
# 由来
# ----
# 项目所有者复核 AGENTS.md §10.3 时提出：
#   「这个项目还依赖 namespace？依赖 linux 自身的 cifs 或者 mount？这不可接受，
#     绝不应该依赖操作系统提供的任何相关机制。操作系统只被当作一个普通的
#     文件系统提供方。」
# 核查结论是**产品代码本来就是干净的** —— §10.3 讲的是「测试工具 mount.cifs
# 在本容器跑不起来」，那是测试环境限制，不是产品依赖。
# 但这条界线当时**没有任何门禁在守**，全靠人肉 grep 证明。下一次谁把
# syscall.Mount 加进来，CI 一声不吭。本段就是把那句口头承诺变成机器校验。
#
# 划线
# ----
# 本软件自己实现整个 SMB 协议栈，只向操作系统要两样东西：
#   1. 普通的文件读写（open/read/write/stat 这一层）；
#   2. 普通的 TCP/UDP 套接字（含 UDP 组播）。
# 除此以外的内核机制一概不用：不挂载文件系统、不进出命名空间、不 chroot、
# 不读系统的名字解析与网络配置文件。
#
# 明确**不**在禁止之列（写在这里免得后人误伤）：
#   - net.Listen / net.ListenUDP / net.ListenMulticastUDP 等套接字操作；
#   - internal/mdns 在 224.0.0.251:5353 与 [ff02::fb]:5353 上自己收发组播报文 ——
#     这是 C4 明确**要求**的做法（自己实现，不许调 avahi），不是违规；
#   - os.Open / os.Stat / os.Rename 这类普通文件操作。
#
# 负向验证见 test/ci/negative-verify.sh 第 5 节 —— 没做过负向验证的门禁一律不算数。

c9bad=0
c9fail() {
    c9bad=1
    fail "$1"
}

# 作用域：运行时代码（internal/、cmd/）的非测试文件。
# scripts/ 与 _test.go 是开发/测试用的，不受此限
# （测试里出现 mount.cifs 这类字样是正常的，见 §10.3）。
c9scope() { find ./internal ./cmd -name '*.go' -not -name '*_test.go' 2>/dev/null; }

# --- (a) 挂载 / 命名空间 / chroot 类系统调用符号
#
# 用 grep -E 加词边界，且先剥掉整行注释：约束文档本身就要写这些词，
# 一句 `// 禁止 syscall.Mount` 的说明性注释不该把 CI 判红
# —— 那只会逼大家去改注释而不是改代码。
for sym in \
    syscall.Mount syscall.Unmount syscall.Chroot syscall.PivotRoot \
    unix.Mount unix.Unmount unix.Setns unix.Unshare unix.Chroot unix.PivotRoot \
    CLONE_NEWNS CLONE_NEWUSER CLONE_NEWNET CLONE_NEWPID CLONE_NEWUTS CLONE_NEWIPC; do
    # 点号在正则里是通配符，必须转义，否则 syscall.Mount 会匹配到 syscallXMount。
    pat=$(printf '%s' "$sym" | sed 's|\.|\\.|g')
    hits=$(
        c9scope | while read -r f; do
            strip_comments "$f" |
                grep -n -E -- "(^|[^A-Za-z0-9_])$pat([^A-Za-z0-9_]|$)" 2>/dev/null |
                sed "s|^|$f:|"
        done
    )
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        c9fail "运行时代码中出现 $sym，违反 C9（不得使用挂载/命名空间等操作系统机制）"
    fi
done

# --- (b) 系统名字解析 / 网络配置文件的字符串字面量
#
# 要求命中**双引号字符串字面量内部**，理由同上。
# /etc/passwd、/etc/shadow、/etc/group、/etc/krb5.conf 由上面的 C8 段负责，这里不重复。
for path in /etc/resolv.conf /etc/nsswitch.conf /etc/hosts /proc/net/; do
    hits=$(
        c9scope | while read -r f; do
            strip_comments "$f" | grep -n -- "\"[^\"]*$path" 2>/dev/null | sed "s|^|$f:|"
        done
    )
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        c9fail "运行时代码的字符串字面量中出现 $path，违反 C9（不得读取系统名字解析/网络配置）"
    fi
done

# --- (c) Windows 动态库加载：系统 DLL 白名单校验
#
# internal/vfs/sys_windows.go 里有一行 windows.NewLazySystemDLL("kernel32.dll")，
# **它不算违规**，理由必须写清楚，否则后人只会看到「有个例外」而不知道边界在哪：
#
#   Windows **没有稳定的系统调用号**（每个版本都会变，微软从不承诺兼容），
#   微软官方承诺的 ABI 边界就是 DLL 导出函数。Go runtime 自身的 os/net/time
#   全部通过 NewLazySystemDLL 调用 kernel32/ntdll/ws2_32。
#   这是**平台调用约定本身**，不是「依赖第三方动态库」——
#   把我们这一行删掉，产物照样会加载 kernel32。
#   它与「依赖 avahi 守护进程」「依赖 cifs 内核驱动」是完全不同性质的东西：
#   后者是**可以不依赖**的外部服务，前者是平台 ABI，绕不过去。
#
# 所以豁免不是「不检查」，而是「检查参数是不是系统 DLL 白名单里的」：
#   - 白名单只有 kernel32 / ntdll / ws2_32 / advapi32 这几个系统 DLL；
#   - 加载任何**非白名单** DLL 一律违规（那才是真的依赖外部动态库，违反 C2）；
#   - 只豁免 NewLazySystemDLL。NewLazyDLL / LoadLibrary 一律违规：
#     它们不走 System32 的安全加载路径，即便参数是 kernel32.dll 也存在
#     DLL 劫持风险，而且我们没有任何理由用它们。
#   - 参数不是字面量（变量、拼接）同样判红：白名单校验的前提是参数可静态判定。
dllhits=$(
    c9scope | while read -r f; do
        strip_comments "$f" |
            grep -n -E -- 'NewLazySystemDLL|NewLazyDLL|LoadLibrary' 2>/dev/null |
            grep -v -iE -- 'NewLazySystemDLL\("(kernel32|ntdll|ws2_32|advapi32)\.dll"\)' |
            sed "s|^|$f:|"
    done
)
if [ -n "$dllhits" ]; then
    printf '%s\n' "$dllhits"
    c9fail "运行时代码加载了非系统 DLL（或用了 NewLazyDLL/LoadLibrary），违反 C9/C2"
fi

if [ "$c9bad" = "0" ]; then
    pass "C9 操作系统仅作为文件系统与套接字提供方"
fi

# ---------------------------------------------------------------- C6 禁止 AGPL 依赖
#
# 用依赖图判定（go list -m all），**不要**用裸文本 grep 扫 .go 文件 —— 否则
# test/integration/gosmb2_test.go 里那句「绝不能用 AGPL-3.0 的 macos-fuse-t/go-smb2」
# 的说明性注释会被当成违规，制造假阳性，把 tag_push 卡死在发版前
# （v0.3.0 首次打 tag 就栽在这：gate_constraints 红掉，Release/附件全没产出）。
# 真实依赖只可能出现在模块图里；注释里写这个词完全正常（约束文档本身就要写它），
# 见本脚本开头的「设计要点」。C1 段的 go list -deps 已经证明该环境 go list 可用。

if go list -m all 2>/dev/null | grep -q 'macos-fuse-t/go-smb2'; then
    fail "go.mod 依赖图中出现 AGPL-3.0 的 macos-fuse-t/go-smb2，违反依赖政策（只可阅读参考，不得 import）"
else
    pass "无 AGPL 依赖"
fi

exit "$bad"
