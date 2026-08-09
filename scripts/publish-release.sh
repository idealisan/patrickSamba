#!/bin/sh
# publish-release.sh —— 手工把四平台产物发布成 CNB Release（默认标记为 prerelease）。
#
# 为什么要有这个脚本：
#   自动流水线（.cnb.yml 的 tag_push）第一次跑起来大概率有坑，而"能让人下载到"
#   这件事不该被流水线调试卡住。这条手工路径与流水线用的是同一套 CNB Open API
#   语义（git:release 内置任务本质上也是调它），所以先手工走通，再去补流水线。
#
# 用法：
#   scripts/publish-release.sh --tag v0.1.0-test              # 构建 + 建 prerelease + 传附件
#   scripts/publish-release.sh --tag v0.1.0 --no-prerelease   # 正式版
#   scripts/publish-release.sh --tag v0.1.0-test --skip-build # 复用已有 dist/
#   scripts/publish-release.sh --tag v0.1.0-test --dry-run    # 只看要做什么，不写任何东西
#   scripts/publish-release.sh --tag v0.1.0-test --delete     # 删掉这个 Release（清理试验）
#   scripts/publish-release.sh --tag v0.1.0-test --delete --delete-tag  # 连 tag 一起删
#
# 凭据：
#   只从环境变量 $CNB_TOKEN 读取，脚本任何路径都不会把它打印出来、写进文件或塞进 URL。
#   不要在本脚本里加 `set -x`，也不要给 curl 加 -v —— 那会把 Authorization 头打到日志里。
#
# CNB Open API（路径与字段名取自 cnb-cli 内置的接口定义，已实测）：
#   POST   /{repo}/-/releases                                  创建版本（prerelease 布尔字段）
#   GET    /{repo}/-/releases/tags/{tag}                       按 tag 查版本
#   DELETE /{repo}/-/releases/{release_id}                     删除版本
#   POST   /{repo}/-/releases/{release_id}/asset-upload-url    取预签名上传地址
#   PUT    <upload_url>                                        裸流上传文件本体
#   POST   <verify_url>                                        确认上传（不确认则附件不落地）
#   DELETE /{repo}/-/git/tags/{tag}                            删除标签
#
# 注意：创建版本时若 tag 不存在，CNB 会基于 target_commitish 自动建 tag。
#       也就是说跑一次试验会在仓库里留下一个真 tag，清理时要带 --delete-tag。
set -e

cd "$(dirname "$0")/.."
DIST=dist

info() { printf '\033[1;36m==> %s\033[0m\n' "$1"; }
pass() { printf '  \033[1;32mOK\033[0m  %s\n' "$1"; }
warn() { printf '  \033[1;33m--\033[0m  %s\n' "$1" >&2; }
die()  { printf '\033[1;31m错误: %s\033[0m\n' "$1" >&2; exit 1; }

# ------------------------------------------------------------------ 参数

TAG=
NAME=
NOTES_FILE=CHANGELOG.md
PRERELEASE=true
TARGET=
SKIP_BUILD=0
DRY_RUN=0
DO_DELETE=0
DO_DELETE_TAG=0

while [ $# -gt 0 ]; do
    case "$1" in
        --tag)          TAG=$2; shift 2 ;;
        --name)         NAME=$2; shift 2 ;;
        --notes-file)   NOTES_FILE=$2; shift 2 ;;
        --target)       TARGET=$2; shift 2 ;;
        --prerelease)   PRERELEASE=true; shift ;;
        --no-prerelease) PRERELEASE=false; shift ;;
        --skip-build)   SKIP_BUILD=1; shift ;;
        --dry-run)      DRY_RUN=1; shift ;;
        --delete)       DO_DELETE=1; shift ;;
        --delete-tag)   DO_DELETE_TAG=1; shift ;;
        -h|--help)      sed -n '2,40p' "$0"; exit 0 ;;
        *)              die "未知参数: $1（--help 看用法）" ;;
    esac
done

[ -n "$TAG" ] || die "必须用 --tag 指定标签名，例如 --tag v0.1.0"
[ -n "$NAME" ] || NAME=$TAG

# ------------------------------------------------------------------ 环境

# 这几个变量在 CNB 流水线运行时由平台注入；本开发容器里也有，所以手工也能跑。
ENDPOINT=${CNB_API_ENDPOINT:-https://api.cnb.cool}
SLUG=${CNB_REPO_SLUG:-}
[ -n "$SLUG" ] || die "环境变量 CNB_REPO_SLUG 未设置（形如 org/repo）"
[ -n "${CNB_TOKEN:-}" ] || die "环境变量 CNB_TOKEN 未设置，无法调用 CNB Open API"

for c in curl jq; do
    command -v "$c" >/dev/null 2>&1 || die "缺少命令 $c，本脚本依赖它解析/构造 JSON"
done

TMP=$(mktemp -d)
cleanup() { [ -n "$TMP" ] && rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

RESP=$TMP/resp.json

# api <METHOD> <URL 或 /path> [body 文件] —— 回显 HTTP 状态码，响应体落到 $RESP。
# 绝不回显 token；出错时只打状态码与响应体。
api() {
    _m=$1; _u=$2; _b=${3:-}
    case "$_u" in
        http://*|https://*) : ;;
        *) _u="$ENDPOINT$_u" ;;
    esac
    if [ -n "$_b" ]; then
        curl -sS -X "$_m" \
            -H "Authorization: Bearer $CNB_TOKEN" \
            -H "Content-Type: application/json" \
            -H "Accept: application/json" \
            --data-binary @"$_b" \
            -o "$RESP" -w '%{http_code}' "$_u"
    else
        curl -sS -X "$_m" \
            -H "Authorization: Bearer $CNB_TOKEN" \
            -H "Accept: application/json" \
            -o "$RESP" -w '%{http_code}' "$_u"
    fi
}

api_err() {
    printf '\033[1;31m%s 失败（HTTP %s）\033[0m\n' "$1" "$2" >&2
    head -c 2000 "$RESP" >&2; printf '\n' >&2
    exit 1
}

# release_id_of <tag> —— 查到回显 id，查不到回显空串。
release_id_of() {
    _code=$(api GET "/$SLUG/-/releases/tags/$1")
    case "$_code" in
        200) jq -r '.id // empty' "$RESP" ;;
        404) : ;;
        *)   api_err "查询 Release" "$_code" ;;
    esac
}

# ------------------------------------------------------------------ 删除模式

if [ "$DO_DELETE" = "1" ]; then
    rid=$(release_id_of "$TAG")
    if [ -z "$rid" ]; then
        warn "tag $TAG 上没有 Release，无需删除"
    elif [ "$DRY_RUN" = "1" ]; then
        info "[dry-run] 将删除 Release $TAG（id=$rid）"
    else
        code=$(api DELETE "/$SLUG/-/releases/$rid")
        case "$code" in
            200|204) pass "已删除 Release $TAG（id=$rid）" ;;
            *) api_err "删除 Release" "$code" ;;
        esac
    fi

    if [ "$DO_DELETE_TAG" = "1" ]; then
        if [ "$DRY_RUN" = "1" ]; then
            info "[dry-run] 将删除 tag $TAG"
        else
            code=$(api DELETE "/$SLUG/-/git/tags/$TAG")
            case "$code" in
                200|204) pass "已删除 tag $TAG" ;;
                404)     warn "tag $TAG 不存在" ;;
                *)       api_err "删除 tag" "$code" ;;
            esac
        fi
    fi
    exit 0
fi

# ------------------------------------------------------------------ 构建产物

if [ "$SKIP_BUILD" = "1" ]; then
    info "跳过构建，直接使用现有 $DIST/"
else
    info "构建 $TAG 的四平台产物"
    VERSION=$TAG scripts/build-release.sh
fi

# 产物清单：只发压缩包和校验和，不发裸二进制（裸二进制没有随包文档）。
ASSETS=$(ls "$DIST"/*.tar.gz "$DIST"/*.zip 2>/dev/null || true)
[ -n "$ASSETS" ] || die "$DIST/ 下没有找到任何 .tar.gz/.zip，先跑 scripts/build-release.sh"
[ -f "$DIST/SHA256SUMS" ] && ASSETS="$ASSETS
$DIST/SHA256SUMS"

# 四个平台一个都不能少（AGENTS.md C7）。少一个就是发了个残缺版本，
# 而这种残缺在 Release 页面上一眼看不出来 —— 必须在上传前挡住。
for p in linux_amd64 linux_arm64 darwin_arm64 windows_amd64; do
    printf '%s\n' "$ASSETS" | grep -q "_$p\." \
        || die "$DIST/ 缺少 $p 的产物，四平台没齐，拒绝发布"
done

count=$(printf '%s\n' "$ASSETS" | grep -c . || true)
info "待上传附件 $count 个"
printf '%s\n' "$ASSETS" | sed 's/^/  /'

# ------------------------------------------------------------------ 发布说明

if [ -f "$NOTES_FILE" ]; then
    notes_src=$NOTES_FILE
else
    warn "$NOTES_FILE 不存在，改用占位正文"
    cat > "$TMP/notes.md" <<EOF
$NAME

发布说明待补（$NOTES_FILE 尚未生成），正式发布前会用 CHANGELOG.md 的内容替换本段。

附件为四平台静态二进制（CGO_ENABLED=0，无动态库依赖）：
linux/amd64、linux/arm64、darwin/arm64、windows/amd64。
校验和见 SHA256SUMS。
EOF
    notes_src=$TMP/notes.md
fi

# 服务端是按 target_commitish 建 tag 的，指向一个还没推上去的本地 commit 会直接失败。
# 所以默认取远端已有的默认分支头，而不是本地 HEAD。
if [ -z "$TARGET" ]; then
    _br=${CNB_DEFAULT_BRANCH:-main}
    TARGET=$(git rev-parse "origin/$_br" 2>/dev/null || git rev-parse HEAD)
    if [ "$TARGET" != "$(git rev-parse HEAD)" ]; then
        warn "本地 HEAD 与 origin/$_br 不一致，发布用的是 origin/$_br（$TARGET）"
    fi
fi

# ------------------------------------------------------------------ 创建 Release

# prerelease 的版本不该抢走 "latest" 标记，否则用户点仓库首页的最新版会拿到内测包。
if [ "$PRERELEASE" = "true" ]; then make_latest=false; else make_latest=true; fi

jq -n --arg tag "$TAG" --arg name "$NAME" --arg target "$TARGET" \
      --arg latest "$make_latest" --argjson pre "$PRERELEASE" \
      --rawfile body "$notes_src" \
      '{tag_name:$tag, name:$name, body:$body, target_commitish:$target,
        draft:false, prerelease:$pre, make_latest:$latest}' > "$TMP/create.json"

info "创建 Release $TAG（prerelease=$PRERELEASE, target=$TARGET）"

if [ "$DRY_RUN" = "1" ]; then
    info "[dry-run] POST $ENDPOINT/$SLUG/-/releases，请求体（正文已截断）："
    jq '.body |= (.[0:200] + "…")' "$TMP/create.json" | sed 's/^/  /'
    info "[dry-run] 随后会逐个上传上面列出的附件，然后确认上传。"
    exit 0
fi

existing=$(release_id_of "$TAG")
[ -z "$existing" ] || die "tag $TAG 上已经有 Release（id=$existing）。先 --delete 再重发，避免覆盖既有发布。"

code=$(api POST "/$SLUG/-/releases" "$TMP/create.json")
case "$code" in
    200|201) : ;;
    *) api_err "创建 Release" "$code" ;;
esac

rid=$(jq -r '.id // empty' "$RESP")
# 有些实现创建成功只回 201 不回 body，兜一手按 tag 反查。
[ -n "$rid" ] || rid=$(release_id_of "$TAG")
[ -n "$rid" ] || die "创建成功但拿不到 release id，无法继续上传附件"
pass "Release 已创建 id=$rid"

# ------------------------------------------------------------------ 上传附件

# 刻意用 for 而不是 `... | while read`：管道里的 while 是子 shell，
# 里面 die/exit 传不出来，失败会被"上传成功"的假象盖过去。产物文件名不含空格。
for f in $ASSETS; do
    base=$(basename "$f")
    size=$(wc -c < "$f" | tr -d ' ')

    jq -n --arg n "$base" --argjson s "$size" \
        '{asset_name:$n, size:$s, overwrite:true}' > "$TMP/asset.json"

    code=$(api POST "/$SLUG/-/releases/$rid/asset-upload-url" "$TMP/asset.json")
    case "$code" in
        200|201) : ;;
        *) api_err "获取 $base 的上传地址" "$code" ;;
    esac

    upload_url=$(jq -r '.upload_url // empty' "$RESP")
    verify_url=$(jq -r '.verify_url // empty' "$RESP")
    [ -n "$upload_url" ] || die "$base: 响应里没有 upload_url"
    [ -n "$verify_url" ] || die "$base: 响应里没有 verify_url"

    # 预签名地址自带鉴权，不要再带 Authorization 头（有的对象存储会因此拒签）。
    ucode=$(curl -sS -X PUT --upload-file "$f" -o "$TMP/put.out" -w '%{http_code}' "$upload_url")
    case "$ucode" in
        200|201|204) : ;;
        *) printf '\033[1;31m上传 %s 失败（HTTP %s）\033[0m\n' "$base" "$ucode" >&2
           head -c 1000 "$TMP/put.out" >&2; printf '\n' >&2; exit 1 ;;
    esac

    # 不确认的话附件不会出现在 Release 页面上 —— 这一步漏了会表现为"传了但看不见"。
    printf '{"ttl":0}' > "$TMP/confirm.json"
    ccode=$(api POST "$verify_url" "$TMP/confirm.json")
    case "$ccode" in
        200|201|204) : ;;
        *) api_err "确认上传 $base" "$ccode" ;;
    esac

    pass "$base（$size 字节）已上传"
done

# ------------------------------------------------------------------ 复核

code=$(api GET "/$SLUG/-/releases/tags/$TAG")
[ "$code" = "200" ] || api_err "复核 Release" "$code"

info "Release $TAG 复核结果"
jq -r '"  prerelease=\(.prerelease)  draft=\(.draft)  is_latest=\(.is_latest)
  附件 \(.assets | length) 个:", (.assets[]? | "    - \(.name)  \(.size) 字节  \(.browser_download_url // .brower_download_url // "")")' "$RESP"

got=$(jq -r '.assets | length' "$RESP")
if [ "$got" != "$count" ]; then
    die "附件数量对不上：期望 $count，实际 $got"
fi
if [ "$(jq -r '.prerelease' "$RESP")" != "$PRERELEASE" ]; then
    die "prerelease 标记没生效：期望 $PRERELEASE，实际 $(jq -r '.prerelease' "$RESP")"
fi

pass "发布完成：$TAG（prerelease=$PRERELEASE，附件 $got 个）"
