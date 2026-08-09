#!/usr/bin/env python3
"""变异测试驱动器 —— 工作树零修改。

做法：把某个源文件的一小段字面量替换掉，把**变异体**写到仓库外的临时目录，
再用 `go test -overlay=<json>` 让编译器读变异体而不是原文件。
`git status` 全程干净，不需要「改一下再改回来」。

用法：
    python3 test/qa-vfs/mutate.py            # 跑全部变异
    python3 test/qa-vfs/mutate.py <名字>...  # 只跑指定变异

判定：
    KILLED   变异体让测试变红 —— 说明现有测试确实盯着这段逻辑。
    SURVIVED 变异体让测试仍然全绿 —— **测试盲区**，这是本脚本要找的东西。
    BROKEN   变异体编译不过（替换文本没匹配上，或语法坏了）—— 脚本自身的问题。
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
GO = "/usr/local/go/bin/go"
PKG = "./internal/vfs/"

# 每条变异：name, 文件, 被替换的原文, 替换成什么, 说明, 跑哪些测试(空=全包)
MUTANTS = [
    # ---------------- PR#19 ValidateComponent 接线与 ".." 绕过 ----------------
    dict(
        name="splitpath-dotdot-noop",
        file="internal/vfs/path.go",
        old="""		case "..":
			if len(out) == 0 {
				return nil, fmt.Errorf("%w: 路径 %q 试图越过共享根", ErrInvalidPath, p)
			}
			out = out[:len(out)-1]""",
        new="""		case "..":
			out = append(out, "..")""",
        why="SplitPath 不再消解 ..，直接把它当普通分量放行 —— 这是最直接的路径穿越",
    ),
    dict(
        name="splitpath-dotdot-underflow",
        file="internal/vfs/path.go",
        old="""			if len(out) == 0 {
				return nil, fmt.Errorf("%w: 路径 %q 试图越过共享根", ErrInvalidPath, p)
			}
			out = out[:len(out)-1]""",
        new="""			if len(out) > 0 {
				out = out[:len(out)-1]
			}""",
        why="越过共享根时不再报错，而是静默停在根 —— 语义变了但不越界，测试盯不盯得住？",
    ),
    dict(
        name="validatecomponent-dot-bypass-removed",
        file="internal/vfs/path.go",
        old="""	if name == "." || name == ".." {
		return nil
	}
	return validateWindowsName(name, hostTrimsTrailingDotSpace)""",
        new="""	return validateWindowsName(name, hostTrimsTrailingDotSpace)""",
        why="删掉 . / .. 的先行放行 —— 作者声称这不是死代码，验证之",
    ),
    dict(
        name="validatecomponent-unwired",
        file="internal/vfs/path.go",
        old="""	return validateWindowsName(name, hostTrimsTrailingDotSpace)
}""",
        new="""	_ = hostTrimsTrailingDotSpace
	return nil
}""",
        why="PR#19 的接线整个断开（回到接线前的状态）",
    ),
    dict(
        name="validatecomponent-always-nil",
        file="internal/vfs/path.go",
        old="""func validateComponent(name string, hostTrimsTrailingDotSpace bool) error {
	if name == "" {""",
        new="""func validateComponent(name string, hostTrimsTrailingDotSpace bool) error {
	if true {
		return nil
	}
	if name == "" {""",
        why="ValidateComponent 变成完全放行 —— 字符级校验整体被架空",
    ),
    dict(
        name="childname-dotdot-allowed",
        file="internal/vfs/optional.go",
        old="""	if name == "." || name == ".." {
		return ErrInvalidPath
	}
	return ValidateComponent(name)""",
        new="""	return ValidateComponent(name)""",
        why="validateChildName 不再拦 .. —— AppleInfoAt/Batch 变成目录穿越原语",
    ),
    dict(
        name="lookupexact-dotdot-allowed",
        file="internal/vfs/local_handle.go",
        old="""	if pattern == "." || pattern == ".." {
		return zero, false
	}""",
        new="",
        why="lookupExactLocked 不再拦 .. —— 精确查询 \"..\" 会 stat 到共享根之外的父目录",
    ),
    dict(
        name="snapshot-validate-removed",
        file="internal/vfs/local_handle.go",
        old="""		if ValidateComponent(n) != nil {
			continue
		}""",
        new="",
        why="枚举时不再过滤 SMB 表达不了的名字",
    ),
    dict(
        name="entryattr-dotdot-unclamped",
        file="internal/vfs/local_handle.go",
        old="""		if h.host == h.fs.res.Root() || !h.fs.res.contains(parent) {
			parent = h.fs.res.Root()
		}""",
        new="",
        why="共享根的 \"..\" 不再夹回根 —— 枚举结果会泄露共享外父目录的属性",
    ),

    # ---------------- PR#10 ResolveParent 精确匹配优先 ----------------
    dict(
        name="resolveparent-no-exact",
        file="internal/vfs/path.go",
        old="""		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil && os.IsNotExist(err) {
			if actual, ok := lookupCaseInsensitive(dir, name); ok {
				name = actual
			}
		}""",
        new="""		if actual, ok := lookupCaseInsensitive(dir, name); ok {
			name = actual
		}""",
        why="退回 PR#10 之前的「无条件全扫」—— 大小写冲突时会把明确指名的那个折叠掉",
    ),
    dict(
        name="resolveparent-no-fallback",
        file="internal/vfs/path.go",
        old="""			if actual, ok := lookupCaseInsensitive(dir, name); ok {
				name = actual
			}""",
        new="""			_ = dir""",
        why="精确匹配 miss 后不再回退 —— 大小写不敏感彻底失效",
    ),
    dict(
        name="resolve-no-casefallback",
        file="internal/vfs/path.go",
        old="""		fi, err := os.Lstat(next)
		if err != nil && os.IsNotExist(err) && r.caseFallback() {
			if actual, ok := lookupCaseInsensitive(cur, comp); ok {
				next = filepath.Join(cur, actual)
				fi, err = os.Lstat(next)
			}
		}""",
        new="""		fi, err := os.Lstat(next)""",
        why="中间分量不再做大小写回退",
    ),

    # ---------------- PR#14/#15 配额 ----------------
    dict(
        name="quota-free-underflow",
        file="internal/vfs/local.go",
        old="""	var quotaFree uint64
	if quotaBlocks > usedBlocks {
		quotaFree = quotaBlocks - usedBlocks
	}""",
        new="""	quotaFree := quotaBlocks - usedBlocks""",
        why="去掉下溢保护 —— 用量超配额时 uint64 回绕成天文数字",
    ),
    dict(
        name="quota-no-total-clamp",
        file="internal/vfs/local.go",
        old="""	if quotaBlocks < info.TotalBlocks {
		info.TotalBlocks = quotaBlocks
	}""",
        new="",
        why="配额不再压 Total",
    ),
    dict(
        name="quota-no-free-clamp",
        file="internal/vfs/local.go",
        old="""	if quotaFree < info.FreeBlocks {
		info.FreeBlocks = quotaFree
	}""",
        new="""	_ = quotaFree""",
        why="配额不再压 Free",
    ),
    dict(
        name="quota-no-inversion-guard",
        file="internal/vfs/local.go",
        old="""	if info.FreeBlocks > info.TotalBlocks {
		info.FreeBlocks = info.TotalBlocks
	}
	if info.AvailBlocks > info.FreeBlocks {
		info.AvailBlocks = info.FreeBlocks
	}""",
        new="",
        why="去掉 Avail<=Free<=Total 的收尾夹紧",
    ),
    dict(
        name="quota-used-rounds-down",
        file="internal/vfs/local.go",
        old="""	usedBlocks := (usedBytes + bs - 1) / bs""",
        new="""	usedBlocks := usedBytes / bs""",
        why="已用量改成向下取整 —— 剩余空间会偏大（方向不安全）",
    ),
    dict(
        name="quota-uses-host-used",
        file="internal/vfs/local.go",
        old="""	usedBytes, _ := l.usage.used()""",
        new="""	usedBytes := (info.TotalBlocks - info.FreeBlocks) * bs""",
        why="退回 PR#14 之前的「拿宿主卷已用量」—— 空共享在忙碌宿主上报 0 可用",
    ),

    # ---------------- PR#13 Rename 别名 / mode ----------------
    dict(
        name="rename-no-case-restore",
        file="internal/vfs/local.go",
        old="""	sameObject := false
	if l.cfg.CaseInsensitive && oldDir == newDir && strings.EqualFold(oldName, newName) {
		if want := lastComponent(newPath); want != "" && want != newName {
			dst = filepath.Join(newDir, want)
			sameObject = true
		}
	}""",
        new="""	sameObject := false
	_ = strings.EqualFold""",
        why="纯改大小写的 rename 退化成 no-op（PR#13 修的就是这个）",
    ),
    dict(
        name="rename-samedirentry-off",
        file="internal/vfs/local.go",
        old="""		if !sameObject && sameDirEntry(oldDir, newDir, src, dstFI) {
			sameObject = true
		}""",
        new="""		_ = dstFI""",
        why="宿主折叠出的别名不再识别 —— replace=true 会先删掉源文件（数据丢失）",
    ),
]


def run(cmd, **kw):
    return subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, **kw)


def mutate(m, workdir):
    src = os.path.join(REPO, m["file"])
    with open(src, encoding="utf-8") as f:
        text = f.read()
    if text.count(m["old"]) != 1:
        return None, "原文出现 %d 次（期望 1 次）" % text.count(m["old"])
    dst = os.path.join(workdir, os.path.basename(m["file"]))
    with open(dst, "w", encoding="utf-8") as f:
        f.write(text.replace(m["old"], m["new"]))
    ov = os.path.join(workdir, "overlay.json")
    with open(ov, "w", encoding="utf-8") as f:
        json.dump({"Replace": {src: dst}}, f)
    return ov, None


def main():
    want = set(sys.argv[1:])
    env = dict(os.environ, CGO_ENABLED="0", PATH=os.environ["PATH"] + ":/usr/local/go/bin")
    results = []
    for m in MUTANTS:
        if want and m["name"] not in want:
            continue
        wd = tempfile.mkdtemp(prefix="mut-")
        ov, err = mutate(m, wd)
        if err:
            results.append((m, "BROKEN", err))
            print("BROKEN   %-34s %s" % (m["name"], err))
            continue
        cmd = [GO, "test", PKG, "-count=1", "-overlay=" + ov]
        if m.get("run"):
            cmd += ["-run", m["run"]]
        p = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, env=env)
        out = p.stdout + p.stderr
        if "build failed" in out or "cannot use" in out or "undefined:" in out or "syntax error" in out:
            verdict = "BROKEN"
            detail = out.strip().splitlines()[:4]
        elif p.returncode == 0:
            verdict = "SURVIVED"
            detail = []
        else:
            verdict = "KILLED"
            detail = [l for l in out.splitlines() if l.startswith("--- FAIL")][:6]
        results.append((m, verdict, detail))
        print("%-8s %-34s %s" % (verdict, m["name"], m["why"]))
        for d in detail:
            print("             %s" % (d if isinstance(d, str) else d))
        shutil.rmtree(wd, ignore_errors=True)

    print("\n==== 汇总 ====")
    for m, v, _ in results:
        print("%-8s %s" % (v, m["name"]))
    surv = [m["name"] for m, v, _ in results if v == "SURVIVED"]
    if surv:
        print("\n存活（= 测试盲区）：%s" % ", ".join(surv))
    return 0


if __name__ == "__main__":
    sys.exit(main())
