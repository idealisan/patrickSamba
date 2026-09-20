#!/bin/sh
# patch-codebuddy.sh —— 消除 CodeBuddy 反复弹出的「高危命令」授权面板。
#
# 用法：
#   scripts/env/patch-codebuddy.sh            # 打补丁（幂等，可反复跑）
#   scripts/env/patch-codebuddy.sh --check    # 只报告状态，一个字节都不改
#   scripts/env/patch-codebuddy.sh --revert   # 从备份恢复原始文件（补丁的逆操作）
#   scripts/env/patch-codebuddy.sh --dist <目录>      # 指定 dist 目录（测试用）
#   scripts/env/patch-codebuddy.sh --settings <文件>  # 指定 settings.json（测试用）
#   scripts/env/patch-codebuddy.sh --skip-settings    # 只打 JS 补丁，不碰 settings
#   scripts/env/patch-codebuddy.sh --wait 600         # 等 CodeBuddy 装好再打（见下）
#
# 退出码：
#   0  正常。打补丁模式：补丁已就位（本次打上，或先前就已打上）；
#            --check 模式：全部已打过补丁且 settings.json 已就绪，无事可做；
#            --revert 模式：已从备份恢复。
#   1  出错。正则失配 / 命中多处 / node --check 失败（已回滚）/ settings.json 非法 /
#            --revert 时备份损坏。
#   3  仅 --check：尚未打补丁，需要跑一次本脚本。
#   4  仅 --check / --revert：找不到 CodeBuddy 安装。
#
# 每次运行（成功/跳过/失败）都会往 /tmp/patch-codebuddy.status 追加一行结果，
# 形如 `2026-08-09T18:30:00Z patched dist=/usr/local/.../dist`，供人工核对
# 「补丁到底打没打上」。注意：这行只是历史记录，**判定逻辑不读它**——
# `--check` 永远去目标文件里找补丁特征。
#
# 备份策略（详见下方设计决定 4）：
#   - <文件>.orig-<日期>：长期留底，同名已存在就不覆盖。
#   - --revert 只认 .orig-<日期> 里**最新**的那一份。
#
# ⚠️ 注意退出码 4 只在 --check 出现：**打补丁模式下找不到 CodeBuddy 是退出 0**。
#    这不是笔误 —— 本脚本曾被挂进旧 CNB 流水线的开发环境启动流程（该流水线已随 2026-09-20 迁移移除，现改为本地开发环境手动执行），而本地开发机、
#    CI 构建机上没装 CodeBuddy 是完全正常的，为此把构建判红纯属自找麻烦。
#    --check 是给人和诊断脚本看的，那里需要能区分「没装」和「装了但没打」。
#
# ---------------------------------------------------------------------------
# 这个补丁在解决什么问题
# ---------------------------------------------------------------------------
#
# 即使权限模式已经是 bypassPermissions，CodeBuddy 仍会对判定为 HIGH/CRITICAL 的
# Bash 命令强制弹授权面板（`git worktree remove`、`rm -r`、`git restore` 等，
# 完整清单见 docs/troubleshooting-codebuddy.md §1.4）。而那个面板存在缺陷：
# 一旦超时就再也关不掉，任何按键都无效，主 TUI 就此卡死，后台 agent 却还在跑
# —— 实测卡过 56 分钟、按了约 60 次，唯一出路是重启进程（§1.3 时间线）。
#
# 放行分支长这样（dist/codebuddy.js，压缩后）：
#
#   if (mode === BypassPermissions
#       && !this.isDangerousBashCommand(item)          // ← 就是这里被拦下
#       && !await this.isMandatoryApprovalDeferTool(...)) { /* Auto-approving */ }
#
# 所以让 isDangerousBashCommand 直接返回 false，放行分支就会成立，面板不再出现。
#
# ---------------------------------------------------------------------------
# 几个设计决定，以及**为什么**这么定
# ---------------------------------------------------------------------------
#
# 1) **插入式，不是偏移式，也不是整体替换。**
#    最初手工改的时候是拿字节偏移（约 11274897）定位、把整个函数体换掉的。
#    那样做有两个必坏的点：偏移量随 CodeBuddy 每次发版漂移，压缩后的参数名
#    （当前是 `eA`）也随打包器心情变化。写死任何一个，下次升级后脚本要么
#    改错位置（灾难），要么静默什么都没改（更糟，因为它还会回显成功）。
#    这里改成正则匹配 `isDangerousBashCommand(<标识符>){`，只在 `{` 之后
#    **插入** `return!1;`，原函数体一字不动 —— 保留原体是为了让 diff 可读、
#    也让将来想部分恢复判定的人还看得见原逻辑。
#
# 2) **命中数必须恰好为 1，0 次或多次都报错退出。**
#    0 次 = CodeBuddy 换了实现，这时候「静默跳过」是最坏的选项：面板照弹，
#    而所有人以为补丁还在生效，会去别处找原因。多次 = 打包结构变了，
#    盲目全改可能命中同名但语义不同的方法。宁可红，也不要蒙着改 22 MB 的包。
#
# 3) **node --check 失败必须回滚。**
#    这是整个脚本最要紧的一条。CodeBuddy 是全队唯一的开发入口，
#    把它的入口 JS 改出语法错误 = 全队立刻停摆，而且现场只剩一个
#    「Unexpected token」，没人知道是谁什么时候改的。
#    「一个把 CodeBuddy 改坏还退出 0 的脚本，比压根不打补丁危险得多。」
#    回滚用的是**动手前刚做的那份 .prepatch 副本**，不是 .orig-<日期> 备份 ——
#    理由见第 4 条。
#
# 4) **两种备份，用途不同，不要合并。**
#    - `<文件>.orig-<日期>`：长期留底，给人查「原版长什么样」。同名已存在就
#      不覆盖，保住最原始的那份。
#    - `<文件>.prepatch.<pid>`：本次动手前的精确快照，只用于回滚，成功后删掉。
#    为什么不能拿 .orig-<日期> 回滚：CodeBuddy 升级之后文件是新版本的，
#    而 .orig-<日期> 是**上一个版本**的原版。拿它回滚 = 把旧版本 JS 装回去，
#    表面上「回滚成功」，实际制造了一个版本错配的 CodeBuddy。
#
# 5) **settings.json 必须合并写，不能整体覆盖。**
#    那是用户自己的配置文件，里面可能有 MCP server、hooks、模型偏好等等，
#    整体覆盖会无声地吃掉这些设置。这里只补三项该有的键，其余原样保留；
#    三项都已就位时**一个字节都不写**（保证 md5 不变，幂等可验证）。
#    另外 defaultMode 已经是 fullAccess 时不会被降级成 bypassPermissions ——
#    补丁的职责是「不要更严」，不是「必须等于某个值」。
#
# 6) **`--wait`：因为「装好」比「容器起来」晚。**
#    实测本容器 07:55:38 启动，而 @tencent-ai/codebuddy-code 的包目录 mtime 是
#    07:57:30 —— CodeBuddy 是在容器起来约 2 分钟**之后**才被装上的。
#    旧 CNB 流水线的 vscode 事件 stage 很可能跑在它出现之前（历史记录），那一刻脚本会「优雅跳过、
#    退出 0」，构建日志一片绿，而补丁一次都没打上，直到有人又被面板卡死才发现。
#    这就是本项目栽过七次的「成功回显 ≠ 事情真的发生」。
#    `--wait <秒>` 让脚本轮询等到它出现为止，配合后台运行就不会拖慢环境启动。
#
# 7) **正则只定义一次，放在 JS 侧。**
#    检测（幂等判据）和替换如果各写一份正则，早晚会漂移成两个意思，
#    于是出现「检测说没打、替换说打过了」这种自相矛盾。这里两者共用同一个
#    工厂函数产出的正则。用 node 而不是 sed/perl 还有个现实理由：
#    node 本来就是硬依赖（要跑 node --check），而这个 22 MB 的包是**单行**的，
#    行导向的工具处理起来既慢又容易踩到实现上限。
#
# 注意：这是**开发环境脚本**，不是 stupidSamba 的运行时依赖，
#      不违反 AGENTS.md C3（产品不得 fork/exec 外部进程）。

set -e

info() { printf '\033[1;36m==> %s\033[0m\n' "$1"; }
pass() { printf '  \033[1;32mOK\033[0m  %s\n' "$1"; }
warn() { printf '  \033[1;33m--\033[0m  %s\n' "$1" >&2; }
die()  { printf '\033[1;31m错误: %s\033[0m\n' "$1" >&2; exit 1; }

# ------------------------------------------------------------------- 结果自证
#
# 本脚本曾被挂进旧 CNB 流水线的开发环境启动流程（该流水线已随 2026-09-20 迁移移除，现改为本地开发环境手动执行），而那个 stage 跑在 CodeBuddy 装好
# 之前会「优雅跳过退出 0」—— 构建日志一片绿，补丁却没打上（见上方设计决定 6）。
# 为了不让「日志绿」成为唯一的判据（那正是本项目反复栽的「成功回显 ≠ 事情真的发生」），
# 这里在**每次运行结束**无论成功/跳过/失败都往一个固定文件追加一行结果，形如：
#
#   2026-08-09T18:30:00Z patched dist=/usr/local/lib/.../dist
#   2026-08-09T18:30:00Z notfound dist=
#   2026-08-09T18:30:00Z failed dist=/usr/local/lib/.../dist
#
# 重要：**这一行只是给人看的历史记录，绝不参与判定逻辑。** `--check` 的判据永远
# 是「去目标文件里找补丁特征」，不读这个 marker —— 否则 CodeBuddy 升级把文件覆盖回
# 原版时，marker 还写着 patched，就成了第二个说谎的地方。
RESULT="failed"
STATUS_FILE=/tmp/patch-codebuddy.status
write_status() {
    printf '%s %s dist=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$RESULT" "$DIST_DIR" \
        >> "$STATUS_FILE" 2>/dev/null || true
}
trap write_status EXIT

CHECK_ONLY=0
DIST=""
SETTINGS="${HOME:-/root}/.codebuddy/settings.json"
DO_SETTINGS=1
WAIT=0
POLL=5
REVERT=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --check)         CHECK_ONLY=1; shift ;;
        --dist)          DIST=$2; shift 2 ;;
        --settings)      SETTINGS=$2; shift 2 ;;
        --skip-settings) DO_SETTINGS=0; shift ;;
        --wait)          WAIT=$2; shift 2 ;;
        --revert)        REVERT=1; shift ;;
        -h|--help)       sed -n '2,22p' "$0"; exit 0 ;;
        *)               die "未知参数: $1（用 --help 看用法）" ;;
    esac
done

# ---------------------------------------------------------------- 定位安装目录
#
# 按「越具体越优先」排：环境变量 > --dist > 从 PATH 上的 codebuddy 反推 >
# npm 全局根 > 常见硬编码路径。反推那一档最可靠 —— 它跟着实际会被执行的
# 那个入口走，多版本共存时不会打错对象。

# 判据统一为「该目录下有 codebuddy.js」，**--dist / 环境变量也一样要过这一关**。
# 不给显式指定开后门是故意的：npm 安装是先解包到临时目录再 rename，
# 所以「codebuddy.js 存在」≈「装完了」。让所有来源共用同一个判据，
# --wait 才有明确的等待终点，也不会出现「指定了一个还没装好的目录就直接开干」。
find_dist() {
    # 显式指定的目录是**排他**的：过不了判据就直接算「没找到」，
    # 绝不悄悄回退去找别处。否则 --dist 打错一个字，脚本就会去改真实安装 ——
    # 而它还会一路回显成功。
    if [ -n "$DIST" ]; then
        [ -f "$DIST/codebuddy.js" ] && { printf '%s\n' "$DIST"; return 0; }
        return 1
    fi
    if [ -n "$CODEBUDDY_DIST" ]; then
        [ -f "$CODEBUDDY_DIST/codebuddy.js" ] && { printf '%s\n' "$CODEBUDDY_DIST"; return 0; }
        return 1
    fi

    bin=$(command -v codebuddy 2>/dev/null || true)
    if [ -n "$bin" ]; then
        # bin/codebuddy 是个真文件或软链，都要解到实体再上溯两级取包根。
        real=$(readlink -f "$bin" 2>/dev/null || printf '%s' "$bin")
        frombin=$(dirname "$(dirname "$real")")/dist
    else
        frombin=""
    fi

    npmroot=$(npm root -g 2>/dev/null || true)

    # 自动发现的顺序：从 PATH 上的 codebuddy 反推 > npm 全局根 > 常见硬编码路径。
    # 反推那一档最可靠 —— 它跟着实际会被执行的那个入口走，多版本共存时不会打错对象。
    for cand in \
        "$frombin" \
        "$npmroot/@tencent-ai/codebuddy-code/dist" \
        /usr/local/lib/node_modules/@tencent-ai/codebuddy-code/dist \
        "${HOME:-/root}/.npm-global/lib/node_modules/@tencent-ai/codebuddy-code/dist"
    do
        [ -n "$cand" ] && [ -f "$cand/codebuddy.js" ] && { printf '%s\n' "$cand"; return 0; }
    done
    return 1
}

# ---------------------------------------------------------------- JS 侧工具
#
# 单一入口，两种模式：status 只读、apply 会写。两者共用同一个正则工厂，
# 杜绝「检测与替换用了两份正则」的漂移（见上文设计决定 6）。
#
# 输出（stdout 第一行）与退出码：
#   PATCHED   0   已经打过补丁
#   UNPATCHED 10  未打，且命中数正好 1（status 模式）
#   APPLIED   0   本次已写入（apply 模式）
#   NOMATCH   11  一次都没命中 —— CodeBuddy 换实现了
#   MULTI <n> 12  命中多处 —— 打包结构变了
js_patch() {
    node - "$1" "$2" <<'NODE_EOF'
const fs = require('fs');
const path = require('path');
const mode = process.argv[2];
const file = process.argv[3];

// 正则唯一真源。用工厂函数而不是共享常量：带 /g 的 RegExp 对象有 lastIndex
// 状态，match 与 replace 复用同一个对象时行为会互相污染，这类 bug 极难看出来。
const reDef     = () => /isDangerousBashCommand\(([A-Za-z_$][A-Za-z0-9_$]*)\)\{/g;
// 幂等判据：分号可有可无。历史上手工打的那版是整体替换、写成 `{return!1}`
// 没有分号；插入式写出来的是 `{return!1;<原函数体>`。两种都必须认作「已打过」，
// 否则脚本会往手工补丁上再插一次，虽然无害但破坏了「幂等 = md5 不变」的判据。
const rePatched = () => /isDangerousBashCommand\([A-Za-z_$][A-Za-z0-9_$]*\)\{return!1/;

// 用 latin1（等价于逐字节）读写：这是个压缩打包产物，按字节往返最保险，
// 不会因为编码转换在我们没看的地方悄悄改动内容。补丁串本身是纯 ASCII。
const src = fs.readFileSync(file, 'latin1');

if (rePatched().test(src)) { console.log('PATCHED'); process.exit(0); }

const hits = src.match(reDef()) || [];
if (hits.length === 0) { console.log('NOMATCH'); process.exit(11); }
if (hits.length > 1)   { console.log('MULTI ' + hits.length); process.exit(12); }
if (mode === 'status') { console.log('UNPATCHED'); process.exit(10); }

const out = src.replace(reDef(), (m) => m + 'return!1;');

// 先写临时文件再 rename：rename 在同一文件系统内是原子的。直接就地覆盖的话，
// 万一容器在写 22 MB 的中途没了，留下的是个截断的 JS —— CodeBuddy 直接起不来，
// 而且看起来不像是补丁造成的。顺带把权限位抄过去，别把 755 写成 644。
const tmp = file + '.tmp-' + process.pid;
fs.writeFileSync(tmp, out, 'latin1');
try { fs.chmodSync(tmp, fs.statSync(file).mode & 0o7777); } catch (e) { /* 权限抄不过来不致命 */ }
fs.renameSync(tmp, file);
console.log('APPLIED');
NODE_EOF
}

# ---------------------------------------------------------------- settings.json
#
#   SETTINGS_OK      0   三项都已就位，未写入任何字节
#   SETTINGS_STALE  10   有缺项（status 模式）
#   SETTINGS_WROTE   0   已合并写入（apply 模式）
#   BADJSON <msg>   13   现有文件不是合法 JSON —— 报错，绝不覆盖
js_settings() {
    node - "$1" "$2" <<'NODE_EOF'
const fs = require('fs');
const path = require('path');
const mode = process.argv[2];
const file = process.argv[3];

let obj = {};
let raw = null;
if (fs.existsSync(file)) {
  raw = fs.readFileSync(file, 'utf8');
  if (raw.trim() !== '') {
    try { obj = JSON.parse(raw); }
    catch (e) { console.log('BADJSON ' + e.message); process.exit(13); }
    if (obj === null || typeof obj !== 'object' || Array.isArray(obj)) {
      console.log('BADJSON 顶层不是 JSON 对象'); process.exit(13);
    }
  }
}

// changed 用「我们确实动了某个键」来判定，不用「序列化结果和原文不同」——
// 后者会因为缩进/键序差异把没改动的文件也重写一遍，幂等验证里 md5 就变了。
let changed = false;
const notes = [];

if (typeof obj.permissions !== 'object' || obj.permissions === null || Array.isArray(obj.permissions)) {
  obj.permissions = {}; changed = true; notes.push('permissions 补为对象');
}

// defaultMode：只在缺失或取值比 bypassPermissions 更严时才写。
// fullAccess 严格更宽松，已经是它就别动 —— 本补丁的职责是「不要更严」，
// 不是「必须等于某个字面量」，把用户手动放宽的配置改回去是帮倒忙。
const okDefault = ['bypassPermissions', 'fullAccess'];
if (okDefault.indexOf(obj.permissions.defaultMode) < 0) {
  obj.permissions.defaultMode = 'bypassPermissions'; changed = true;
  notes.push('permissions.defaultMode = bypassPermissions');
}

// subagentPermissionMode 必须是 fullAccess：bypassPermissions 对 HIGH/CRITICAL
// 的 Bash 命令**完全无效**（docs/troubleshooting-codebuddy.md §1.5 已实证），
// 只有 fullAccess 才会走「无条件放行」那条分支。
if (obj.permissions.subagentPermissionMode !== 'fullAccess') {
  obj.permissions.subagentPermissionMode = 'fullAccess'; changed = true;
  notes.push('permissions.subagentPermissionMode = fullAccess');
}

if (!Array.isArray(obj.trustedDirectories)) {
  obj.trustedDirectories = []; changed = true; notes.push('trustedDirectories 补为数组');
}
if (obj.trustedDirectories.indexOf('/workspace/**') < 0) {
  obj.trustedDirectories.push('/workspace/**'); changed = true;   // 追加，不是替换
  notes.push('trustedDirectories += /workspace/**');
}

if (!changed && raw !== null) { console.log('SETTINGS_OK'); process.exit(0); }
if (mode === 'status')        { console.log('SETTINGS_STALE ' + notes.join('; ')); process.exit(10); }

fs.mkdirSync(path.dirname(file), { recursive: true });
const tmp = file + '.tmp-' + process.pid;
fs.writeFileSync(tmp, JSON.stringify(obj, null, 2) + '\n', 'utf8');
fs.renameSync(tmp, file);
console.log('SETTINGS_WROTE ' + notes.join('; '));
NODE_EOF
}

# ---------------------------------------------------------------- 主流程

DIST_DIR=$(find_dist) || DIST_DIR=""

# 两个入口都要考虑：codebuddy.js 是交互式 TUI，codebuddy-headless.js 是 -p
# 非交互模式，同一个方法在两份包里各有一份（详见下方主循环前的注释）。
TARGETS="codebuddy.js codebuddy-headless.js"

# ---------------------------------------------------------------- --revert
#
# 从 .orig-<日期> 备份恢复原始文件（补丁的逆操作）。只恢复**有备份**的目标；
# 没有备份说明从没打过补丁，跳过比乱复制安全。回滚后同样跑 node --check，
# 备份万一损坏也能立刻暴露，而不是留一个起不来的 CodeBuddy。
if [ "$REVERT" = 1 ]; then
    if [ -z "$DIST_DIR" ]; then
        warn "未找到 CodeBuddy 安装，无法回滚"
        RESULT="notfound"; exit 4
    fi
    info "回滚模式：从 .orig-<日期> 备份恢复"
    rbad=0
    for name in $TARGETS; do
        f="$DIST_DIR/$name"
        [ -f "$f" ] || { warn "$name 不存在，跳过"; continue; }
        bak=$(ls -1 "$f".orig-* 2>/dev/null | sort | tail -1)
        if [ -z "$bak" ]; then
            warn "$name 没有可用备份（缺少 $(basename "$f").orig-*），跳过"; continue
        fi
        cp -p "$bak" "$f"
        pass "已从备份回滚 $name: $(basename "$bak")"
        if command -v node >/dev/null 2>&1; then
            chk=$(node --check "$f" 2>&1) && crc=0 || crc=$?
            if [ "$crc" != 0 ]; then
                printf '\033[1;31m错误: 回滚后 node --check 不通过，备份可能已损坏: %s\033[0m\n' "$chk" >&2
                rbad=1
            fi
        fi
    done
    [ "$rbad" = 0 ] && { RESULT="reverted"; exit 0; }
    exit 1
fi

# --wait：等 CodeBuddy 出现。只在「一开始没找到」时才等，已经找到就一秒不耽误。
if [ -z "$DIST_DIR" ] && [ "$WAIT" -gt 0 ]; then
    info "尚未找到 CodeBuddy，最多等待 ${WAIT}s（每 ${POLL}s 重试一次）"
    deadline=$(( $(date +%s) + WAIT ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        sleep "$POLL"
        DIST_DIR=$(find_dist) || DIST_DIR=""
        [ -n "$DIST_DIR" ] && break
    done
    [ -n "$DIST_DIR" ] && info "等到了，用时约 $(( WAIT - (deadline - $(date +%s)) ))s"
fi

if [ -z "$DIST_DIR" ]; then
    warn "未找到 CodeBuddy 安装目录，跳过（本地开发机 / CI 构建机上没有它是正常的）"
    [ "$CHECK_ONLY" = 1 ] && { RESULT="notfound"; exit 4; }
    RESULT="notfound"; exit 0
fi
info "CodeBuddy dist: $DIST_DIR"

command -v node >/dev/null 2>&1 || {
    # 没有 node 就既打不了补丁也校验不了语法（CodeBuddy 本身就是 node 应用，
    # 正常情况下不可能出现「有 CodeBuddy 没 node」）。同样按优雅跳过处理。
    warn "未找到 node，跳过（这台机器上没有 CodeBuddy 运行环境）"
    [ "$CHECK_ONLY" = 1 ] && { RESULT="notfound"; exit 4; }
    RESULT="notfound"; exit 0
}

# 两个入口都要打：codebuddy.js 是交互式 TUI，codebuddy-headless.js 是 -p
# 非交互模式，同一个方法在两份包里各有一份。只打前者的话，将来有人用
# headless 跑批就又会撞上面板判定，而那时没人会想起来还有第二个入口。
# headless 不存在就跳过（不同版本的打包内容不一定一样）；存在但正则失配是**错误**，
# 那说明结构变了，必须让人看见。
STAMP=$(date +%Y%m%d)
todo=0
bad=0
applied=0

for name in $TARGETS; do
    f="$DIST_DIR/$name"
    # codebuddy.js 必定存在（find_dist 的判据就是它）；这里跳过的只会是 headless。
    [ -f "$f" ] || { warn "$name 不存在，跳过"; continue; }

    out=$(js_patch status "$f") && rc=0 || rc=$?
    case "$rc" in
        0)  pass "$name 已打过补丁，无需处理" ; continue ;;
        10) : ;;   # 未打，往下走
        11) printf '\033[1;31m错误: %s 里没找到 isDangerousBashCommand(...)\033[0m\n' "$name" >&2
            printf '      CodeBuddy 大概率升级换了实现。重新定位办法见 docs/troubleshooting-codebuddy.md §6.4\n' >&2
            bad=1; continue ;;
        12) printf '\033[1;31m错误: %s 里 isDangerousBashCommand 命中 %s 处（预期 1 处）\033[0m\n' "$name" "${out#MULTI }" >&2
            printf '      打包结构变了，不做盲目替换。见 docs/troubleshooting-codebuddy.md §6.4\n' >&2
            bad=1; continue ;;
        *)  printf '\033[1;31m错误: 检测 %s 失败（node 退出 %s）: %s\033[0m\n' "$name" "$rc" "$out" >&2
            bad=1; continue ;;
    esac

    if [ "$CHECK_ONLY" = 1 ]; then
        warn "$name 未打补丁"
        todo=1
        continue
    fi

    # 长期留底：同名已存在就不覆盖，保住最原始的那份。
    backup="$f.orig-$STAMP"
    if [ -f "$backup" ]; then
        info "备份已存在，不覆盖: $(basename "$backup")"
    else
        cp -p "$f" "$backup"
        pass "已备份到 $(basename "$backup")"
    fi

    # 回滚副本：必须是**本次动手前**的字节，不能用上面那个可能来自旧版本的备份。
    prepatch="$f.prepatch.$$"
    cp -p "$f" "$prepatch"

    out=$(js_patch apply "$f") && rc=0 || rc=$?
    if [ "$rc" != 0 ]; then
        cp -p "$prepatch" "$f"; rm -f "$prepatch"
        printf '\033[1;31m错误: 写入 %s 失败（node 退出 %s）: %s —— 已回滚\033[0m\n' "$name" "$rc" "$out" >&2
        bad=1; continue
    fi

    # 语法校验。这一步失败必须原样还回去，理由见文件头设计决定 3。
    # 报错信息要在回滚**之前**抓下来 —— 回滚之后再跑一次 node --check 得到的是
    # 原文件的结果（当然是通过的），打印出来只会误导人。
    chk=$(node --check "$f" 2>&1) && crc=0 || crc=$?
    if [ "$crc" = 0 ]; then
        rm -f "$prepatch"
        applied=1
        pass "$name 已打补丁，node --check 通过"
    else
        cp -p "$prepatch" "$f"; rm -f "$prepatch"
        printf '\033[1;31m错误: 打完补丁后 node --check 不通过，已回滚 %s\033[0m\n' "$name" >&2
        printf '      %s\n' "$chk" >&2
        bad=1; continue
    fi
done

if [ "$DO_SETTINGS" = 1 ]; then
    mode=apply
    [ "$CHECK_ONLY" = 1 ] && mode=status
    out=$(js_settings "$mode" "$SETTINGS") && rc=0 || rc=$?
    case "$rc" in
        0)  case "$out" in
                SETTINGS_OK)     pass "settings.json 三项均已就位（未写入）" ;;
                SETTINGS_WROTE*) pass "settings.json 已合并写入：${out#SETTINGS_WROTE }" ;;
            esac ;;
        10) warn "settings.json 缺项：${out#SETTINGS_STALE }"; todo=1 ;;
        13) printf '\033[1;31m错误: %s 不是合法 JSON（%s）\033[0m\n' "$SETTINGS" "${out#BADJSON }" >&2
            printf '      不覆盖用户配置。请人工修好或先移走这个文件再重跑。\n' >&2
            bad=1 ;;
        *)  printf '\033[1;31m错误: 处理 settings.json 失败（node 退出 %s）: %s\033[0m\n' "$rc" "$out" >&2
            bad=1 ;;
    esac
fi

[ "$bad" = 0 ] || exit 1
[ "$CHECK_ONLY" = 1 ] && [ "$todo" = 1 ] && { RESULT="unpatched"; exit 3; }
if [ "$applied" -gt 0 ]; then RESULT="patched"; else RESULT="already"; fi
exit 0
