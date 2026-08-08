#!/bin/sh
# check-constraints.sh —— 校验 AGENTS.md 里的硬性约束（C1 / C3 / C8）。
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

# 禁止在字符串字面量里出现系统账户数据库路径。
# 要求命中双引号内部，避免误伤说明性注释。
#
# **_test.go 豁免**：`/etc/passwd` 是路径穿越攻击的经典目标，
# 安全测试的攻击向量里就该出现它。把测试向量改成别的路径只会削弱测试，
# 属于为了让检查变绿而破坏代码 —— 正好是这个检查要防止的反面。
# 真正的 C8 违规（运行时去读系统账户库）由上面的依赖图检查兜底。
for path in /etc/passwd /etc/shadow /etc/group /etc/krb5.conf; do
    hits=$(grep -rn --include='*.go' -- "\"[^\"]*$path" . | grep -v '_test\.go:' || true)
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        fail "非测试代码的字符串字面量中出现 $path，违反 C8"
    fi
done

# 禁止 NSS / PAM / 系统登录 API 的符号。
# 先剥掉整行注释再匹配，避免误伤。
strip_comments() { grep -v -E '^[[:space:]]*(//|\*|/\*)' "$1"; }

for sym in getpwnam getgrnam getpwuid getgrgid pam_authenticate pam_start LogonUserW ODNodeCopyRecord; do
    hits=$(
        find . -name '*.go' -not -path './.git/*' | while read -r f; do
            strip_comments "$f" | grep -nH -w -- "$sym" 2>/dev/null | sed "s|^|$f:|"
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
        strip_comments "$f" | grep -nH -E 'os/exec|exec\.Command' 2>/dev/null | sed "s|^|$f:|"
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
        strip_comments "$f" | grep -nH -E 'godbus|dbus\.|avahi|/var/run/avahi' 2>/dev/null | sed "s|^|$f:|"
    done
)
if [ -n "$dbushits" ]; then
    printf '%s\n' "$dbushits"
    fail "运行时代码中出现 D-Bus/avahi 调用，违反 C4（mDNS 必须自实现）"
else
    pass "C4 mDNS 无系统服务依赖"
fi

# ---------------------------------------------------------------- C6 禁止 AGPL 依赖

if grep -rn 'macos-fuse-t/go-smb2' --include='go.mod' --include='*.go' . ; then
    fail "引入了 AGPL-3.0 的 macos-fuse-t/go-smb2，违反依赖政策（只可阅读参考，不得 import）"
else
    pass "无 AGPL 依赖"
fi

exit "$bad"
