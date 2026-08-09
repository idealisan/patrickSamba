#!/bin/sh
# save-sh-scenarios.sh —— scripts/save.sh 的行为回归测试（反向验证）。
#
# 用法：
#   test/ci/save-sh-scenarios.sh                 # 测当前的 scripts/save.sh
#   test/ci/save-sh-scenarios.sh <某个save.sh>   # 测指定版本（用于失败对照）
#
# 为什么要有这个脚本：
#   save.sh 是全团队的提交推送入口，它出错的形态几乎都是**静默的**——
#   打印「已推送」但一个 commit 都没出去（写死 refspec main 的那次），
#   或者在 worktree 里空转 120 秒后失败（锁路径写在 $REPO/.git 下的那次）。
#   这类 bug 只靠读代码发现不了，必须真的搭一个 origin 跑一遍。
#
# 设计要点：
#   * 完全离线：用本地**裸仓库**当 origin，不碰真实远端，不需要网络与 token。
#   * 每个场景一个全新的临时目录，互不污染。
#   * 每个断言都检查**远端的实际状态**（git ls-remote / git log origin），
#     而不是只看脚本的退出码与输出——「打印成功」正是我们要防的东西。
#   * 沙箱仓库里只放非 Go 文件，于是 save.sh 的 go vet 段走「没有 Go 包，
#     跳过编译校验」分支，测试不依赖 Go 工具链是否可用。
#
# 失败对照：
#   test/ci/testdata/save-legacy.sh 是修复前的 save.sh 快照（已冻结，勿改）。
#   把它作为参数传进来，场景 B 与 D 必须**失败**——如果它也过了，
#   说明这个测试根本没测到东西。见文件末尾的自我校验说明。

set -e

SCRIPT_UNDER_TEST=${1:-}
if [ -z "$SCRIPT_UNDER_TEST" ]; then
    SCRIPT_UNDER_TEST="$(cd "$(dirname "$0")/../.." && pwd)/scripts/save.sh"
fi
if [ ! -f "$SCRIPT_UNDER_TEST" ]; then
    echo "找不到被测脚本: $SCRIPT_UNDER_TEST" >&2
    exit 1
fi
SCRIPT_UNDER_TEST=$(cd "$(dirname "$SCRIPT_UNDER_TEST")" && pwd)/$(basename "$SCRIPT_UNDER_TEST")

export PATH=/usr/local/go/bin:$PATH
# 让 git 在任何环境下都能提交（容器里常常没配 user.name）
export GIT_AUTHOR_NAME=save-sh-test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=save-sh-test GIT_COMMITTER_EMAIL=test@example.invalid

ROOT=$(mktemp -d)
PASS=0
FAIL=0

log()  { printf '%s\n' "$*"; }
head1() { printf '\n======== %s ========\n' "$*"; }

ok()   { PASS=$((PASS + 1)); printf '  [PASS] %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '  [FAIL] %s\n' "$*"; }
check() { # check <描述> <期望> <实际>
    if [ "$2" = "$3" ]; then ok "$1 (= $3)"; else bad "$1 期望=[$2] 实际=[$3]"; fi
}

# 建一个「origin 裸仓库 + 一个工作副本」的沙箱，回声工作副本路径。
# $1 = 场景名
mksandbox() {
    _name=$1
    _dir="$ROOT/$_name"
    mkdir -p "$_dir"
    git init -q --bare "$_dir/origin.git"
    git -c init.defaultBranch=main init -q "$_dir/work"
    (
        cd "$_dir/work"
        # 沙箱是 /tmp 下的一次性仓库，作者是假的，全局的提交签名配置在这里
        # 只会让 git commit 直接 403 失败。**仅**在沙箱内关掉，
        # 不影响真实仓库的签名策略（本脚本不碰任何全局配置）。
        git config commit.gpgsign false
        git config tag.gpgsign false
        git remote add origin "$_dir/origin.git"
        mkdir -p scripts
        cp "$SCRIPT_UNDER_TEST" scripts/save.sh
        chmod +x scripts/save.sh
        echo seed > seed.txt
        git add -A
        git commit -q -m "chore: seed"
        git branch -M main
        git push -q -u origin main
    )
    printf '%s' "$_dir"
}

# 在裸仓库里查某分支的提交条数（0 = 分支不存在）
remote_count() { # <origin.git> <branch>
    git -C "$1" rev-list --count "refs/heads/$2" 2>/dev/null || echo 0
}
remote_has() { # <origin.git> <branch>
    git -C "$1" rev-parse --verify -q "refs/heads/$2" >/dev/null 2>&1 && echo yes || echo no
}

###############################################################################
head1 "被测脚本: $SCRIPT_UNDER_TEST"

###############################################################################
# 场景 A：分支在远端不存在时的首次推送
#
# 期望：退出 0，远端出现该分支，且带上本次提交。
###############################################################################
head1 "场景 A —— 首次推送（远端尚无此分支）"
DIR=$(mksandbox a)
cd "$DIR/work"
git switch -q -c qa/feat-a
echo hello-a > a.txt
set +e
OUT=$(sh scripts/save.sh "test: 场景A" a.txt 2>&1); RC=$?
set -e
log "$OUT" | sed 's/^/    /'
log "    exit=$RC"
check "退出码" 0 "$RC"
check "远端已创建 qa/feat-a" yes "$(remote_has "$DIR/origin.git" qa/feat-a)"
check "远端 qa/feat-a 提交数" 2 "$(remote_count "$DIR/origin.git" qa/feat-a)"
check "远端最新提交信息" "test: 场景A" \
    "$(git -C "$DIR/origin.git" log -1 --format=%s refs/heads/qa/feat-a 2>/dev/null)"
# 反向对照：不能顺手把 main 也推了（写死 refspec 的老 bug 就是这个形态）
check "未污染远端 main（仍只有 seed 一条）" 1 "$(remote_count "$DIR/origin.git" main)"

###############################################################################
# 场景 B：远端分支已有新提交 → non-fast-forward
#
# 这是核心场景。修复前的版本在这里必然失败：它 rebase 到 main，
# 既拿不到 origin/qa/feat-b 上那条提交，又改写了本分支历史，
# 6 次重试全部空转。
#
# 期望（修复后）：退出 0；远端**同时**保留双方的提交（4 条 = seed + 共同基点 +
# 别人的 + 我的），即没有任何一方的工作被丢掉。
###############################################################################
head1 "场景 B —— 远端有新提交导致 non-fast-forward"
DIR=$(mksandbox b)
cd "$DIR/work"
git switch -q -c qa/feat-b
echo base > b-base.txt
git add -A && git commit -q -m "test: 分支共同基点"
git push -q -u origin qa/feat-b

# 另一个人（另一个克隆）往同一个分支推了一条
git clone -q "$DIR/origin.git" "$DIR/other"
(
    cd "$DIR/other"
    git config commit.gpgsign false
    git switch -q qa/feat-b
    echo from-teammate > teammate.txt
    git add -A && git commit -q -m "test: 队友的提交"
    git push -q origin qa/feat-b
)
REMOTE_BEFORE=$(git -C "$DIR/origin.git" rev-parse refs/heads/qa/feat-b)

# 我在本地也提交一条，此时 push 必然 non-fast-forward
echo mine > b-mine.txt
set +e
OUT=$(sh scripts/save.sh "test: 场景B 我的提交" b-mine.txt 2>&1); RC=$?
set -e
log "$OUT" | sed 's/^/    /'
log "    exit=$RC"
check "退出码" 0 "$RC"
check "远端 qa/feat-b 提交数（双方都在）" 4 "$(remote_count "$DIR/origin.git" qa/feat-b)"
check "远端最新提交是我的" "test: 场景B 我的提交" \
    "$(git -C "$DIR/origin.git" log -1 --format=%s refs/heads/qa/feat-b 2>/dev/null)"
# 关键断言：队友那条提交必须还是**祖先**，即历史被追加而不是被顶掉
if git -C "$DIR/origin.git" merge-base --is-ancestor "$REMOTE_BEFORE" refs/heads/qa/feat-b 2>/dev/null; then
    ok "队友的提交仍是远端分支的祖先（没有被覆盖）"
else
    bad "队友的提交不再是祖先 —— 历史被改写/丢失"
fi
if git -C "$DIR/origin.git" log --format=%s refs/heads/qa/feat-b | grep -q '^test: 队友的提交$'; then
    ok "队友的提交内容仍在远端"
else
    bad "队友的提交在远端消失了"
fi

###############################################################################
# 场景 C：分离 HEAD
#
# 期望：拒绝推送并退出 1，远端不产生任何新分支/新提交。
###############################################################################
head1 "场景 C —— 分离 HEAD"
DIR=$(mksandbox c)
cd "$DIR/work"
git checkout -q --detach
echo detached > c.txt
set +e
OUT=$(sh scripts/save.sh "test: 场景C" c.txt 2>&1); RC=$?
set -e
log "$OUT" | sed 's/^/    /'
log "    exit=$RC"
check "退出码（应拒绝）" 1 "$RC"
if printf '%s' "$OUT" | grep -q '分离 HEAD'; then
    ok "给出了可读的「分离 HEAD」提示"
else
    bad "没有给出分离 HEAD 提示，用户无从判断原因"
fi
check "远端 main 未被动过" 1 "$(remote_count "$DIR/origin.git" main)"
check "远端分支总数仍为 1" 1 "$(git -C "$DIR/origin.git" for-each-ref --format='%(refname)' refs/heads | wc -l | tr -d ' ')"

###############################################################################
# 场景 D：在 git worktree 里运行
#
# AGENTS.md §7.3 强制每人一个 worktree，所以这是**主要**使用场景。
# worktree 里 `<工作树>/.git` 是文件不是目录，修复前的版本用
# `mkdir "$REPO/.git/…lock"` 上锁，必然 ENOTDIR，acquire() 空转 120 秒后失败。
#
# 期望（修复后）：退出 0 且**很快**返回；锁落在共享的 .git 里。
# 为了不让失败对照真的干等 120 秒，这里用 timeout 收口。
###############################################################################
head1 "场景 D —— 在 git worktree 中运行"
DIR=$(mksandbox d)
WT="$DIR/wt"
git -C "$DIR/work" worktree add -q "$WT" -b qa/feat-d >/dev/null 2>&1
cd "$WT"
echo hello-d > d.txt
START=$(date +%s)
set +e
OUT=$(timeout 45 sh scripts/save.sh "test: 场景D" d.txt 2>&1); RC=$?
set -e
ELAPSED=$(( $(date +%s) - START ))
log "$OUT" | sed 's/^/    /'
log "    exit=$RC  耗时=${ELAPSED}s"
check "退出码" 0 "$RC"
check "远端已创建 qa/feat-d" yes "$(remote_has "$DIR/origin.git" qa/feat-d)"
if [ "$ELAPSED" -lt 30 ]; then
    ok "worktree 下未卡在锁上（${ELAPSED}s < 30s）"
else
    bad "worktree 下疑似卡在锁等待（${ELAPSED}s）"
fi

###############################################################################
printf '\n======== 汇总 ========\n'
printf '  PASS=%d  FAIL=%d\n' "$PASS" "$FAIL"
printf '  沙箱目录（未清理，便于事后取证）: %s\n' "$ROOT"
#
# 自我校验（务必在改动 save.sh 后手动做一次）：
#   test/ci/save-sh-scenarios.sh test/ci/testdata/save-legacy.sh
# 修复前的版本必须在场景 B 与场景 D **FAIL**。若它也全绿，
# 说明这个测试没有真正覆盖到被修的 bug，本文件需要先修好再谈通过。
#
# 注：沙箱目录**故意不删**（AGENTS.md 禁止在脚本里写 rm -r 类命令，
# 且保留现场便于复盘）。它在 /tmp 下，容器重启自然消失。
###############################################################################
[ "$FAIL" -eq 0 ]
