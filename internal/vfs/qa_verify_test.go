package vfs

// qa_verify_test.go —— 独立验证员（qa-vfs）补的**盲区**用例。
//
// 这些用例不是「再测一遍已经绿的东西」，每一条都对应 test/qa-vfs/mutate.py
// 里一个**存活的变异体**：把产品代码里那段判定删掉，改动前整个 internal/vfs
// 包一条测试都不会变红。也就是说那几段代码当时处于「写了但没人盯」的状态，
// 谁顺手删掉都能一路绿灯合进 main。
//
// 每个 Test 的注释里写明它钉的是哪个变异体，复现方式：
//
//	python3 test/qa-vfs/mutate.py <变异体名>
//
// 加上本文件之后应当从 SURVIVED 变成 KILLED。

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------- ".." 不得越界

// TestQAExactLookupDotDotStaysInShare 钉死变异体 `lookupexact-dotdot-allowed`。
//
// 背景：QUERY_DIRECTORY 的「模式串里没有通配符」快速路径
// （localHandle.lookupExactLocked）会拿客户端给的模式串直接
// filepath.Join(h.host, pattern) 去 stat。它靠一条
//
//	if pattern == "." || pattern == ".." { return zero, false }
//
// 把 ".." 挡回完整快照路径，由 entryAttr 去做「共享根的 .. 夹回根自己」。
// 快速路径本身**没有**那个夹紧动作。
//
// 已有的 TestReadDirExactNameSameAsWildcardScan 也查了 ".."，但它只比对
// **名字**——而两条路径返回的名字都是 ".."，属性来自谁看不出来。于是把上面
// 那条 return 删掉，全包测试依然全绿，只是共享根的 ".." 悄悄变成了宿主机上
// 共享根的父目录：客户端能拿到共享外目录的 FileID / 时间戳 / 大小。
//
// 本用例改为比对 **FileID（inode）**，直接区分「根自己」和「根的父目录」。
func TestQAExactLookupDotDotStaysInShare(t *testing.T) {
	fs := newTestFS(t, false)
	root := fs.Root()

	// 让父目录和共享根一定是两个不同的 inode（t.TempDir 已保证），
	// 并在父目录里放个东西，确保它不是恰好同 inode 的怪情况。
	parent := filepath.Dir(root)
	if err := os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("在共享外建文件: %v", err)
	}
	rootFI, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	parentFI, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(rootFI, parentFI) {
		t.Skip("共享根与其父目录是同一个对象，本用例无法区分")
	}

	// 共享根自己的 FileID，作为期望值。
	rootAttr, err := fs.statHost(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	parentAttr, err := fs.statHost(parent, "..")
	if err != nil {
		t.Fatal(err)
	}
	if rootAttr.FileID == parentAttr.FileID {
		t.Skip("根与父目录 FileID 相同，本用例无法区分")
	}

	// 走真实入口：打开共享根这个目录句柄，精确查 ".."。
	h, _ := dirHandle(t, fs, "")
	got := readDirOnce(t, h, "..", true)
	if len(got) != 1 || got[0].Name != ".." {
		t.Fatalf("精确查 \"..\" 得到 %v，期望恰好一条 \"..\"", names(got))
	}
	if got[0].Attr.FileID == parentAttr.FileID {
		t.Fatalf("共享根的 \"..\" 报的是**共享外父目录**的属性 (FileID=%d)，"+
			"这是把宿主机 %q 的元数据泄露给客户端；期望夹回共享根自己 (FileID=%d)",
			got[0].Attr.FileID, parent, rootAttr.FileID)
	}
	if got[0].Attr.FileID != rootAttr.FileID {
		t.Fatalf("共享根的 \"..\" FileID=%d，既不是父目录也不是根 (%d)，语义不明",
			got[0].Attr.FileID, rootAttr.FileID)
	}
}

// TestQAExactLookupDotDotMatchesScanPath 是上一条的**一致性对照**：
// 快速路径与完整快照路径对 "." / ".." 必须给出**同一份属性**，
// 不只是同一个名字。共享根与子目录两种位置都要对得上。
func TestQAExactLookupDotDotMatchesScanPath(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "sub/f", "x")

	for _, dir := range []string{"", "sub"} {
		for _, pat := range []string{".", ".."} {
			// 完整快照路径：用通配符枚举，挑出同名条目。
			hAll, _ := dirHandle(t, fs, dir)
			var want *DirEntry
			for _, e := range readDirOnce(t, hAll, "*", true) {
				if e.Name == pat {
					c := e
					want = &c
				}
			}
			if want == nil {
				t.Fatalf("目录 %q 的通配枚举里没有 %q，测试前提不成立", dir, pat)
			}

			// 精确路径。
			h, _ := dirHandle(t, fs, dir)
			got := readDirOnce(t, h, pat, true)
			if len(got) != 1 {
				t.Fatalf("目录 %q 精确查 %q 得到 %v，期望一条", dir, pat, names(got))
			}
			if got[0].Attr.FileID != want.FileID() {
				t.Errorf("目录 %q 的 %q：精确路径 FileID=%d，快照路径 FileID=%d —— 两条路径不一致",
					dir, pat, got[0].Attr.FileID, want.FileID())
			}
		}
	}
}

// FileID 只是给上面那条用例读起来顺一点的小包装。
func (e DirEntry) FileID() uint64 { return e.Attr.FileID }

// TestQADotDotNeverEscapesAnyEntryPoint 是 ".." 的**总入口清单**。
//
// PR#19 之后 ValidateComponent **故意放行** "." 与 ".."（为了让同一个名字
// 在三平台上得到同一个结论）。这条放行本身不是漏洞，前提是所有调用方都
// 在调用之前自己处理掉这两个分量。本用例把当前全部四个调用方逐一钉住，
// 将来任何一个忘了处理，这里就会红。
func TestQADotDotNeverEscapesAnyEntryPoint(t *testing.T) {
	// 1) ValidateComponent 本身：按契约放行（这是被验证的前提，不是漏洞）。
	if err := ValidateComponent(".."); err != nil {
		t.Fatalf("前提变了：ValidateComponent(\"..\") = %v，"+
			"本用例假设它放行。若现在改成拒绝，请同步更新本文件的说明", err)
	}

	// 2) SplitPath：词法消解，越根必须报错。
	for _, p := range []string{
		"..", "../x", "a/../..", `..\x`, `a\..\..`,
		"./..", "a/./../..", "a/b/../../..",
	} {
		if comps, err := SplitPath(p); err == nil {
			t.Errorf("SplitPath(%q) = %v, nil —— 越过共享根却没报错", p, comps)
		}
	}
	// 反向对照：不越根的 ".." 必须正常消解，否则「一律拒绝」也能让上面全绿。
	for _, c := range []struct{ in, want string }{
		{"a/../b", "b"},
		{"a/b/../c", "a/c"},
		{"a/./b", "a/b"},
		{`a\b\..\c`, "a/c"},
	} {
		got, err := CleanPath(c.in)
		if err != nil || got != c.want {
			t.Errorf("CleanPath(%q) = (%q, %v)，期望 (%q, nil)", c.in, got, err, c.want)
		}
	}

	// 3) validateChildName：句柄内按名字寻址，必须自己拦。
	for _, n := range []string{".", ".."} {
		if err := validateChildName(n); err == nil {
			t.Errorf("validateChildName(%q) = nil —— filepath.Join(host, %q) 就是父目录", n, n)
		}
	}
	// 反向对照。
	if err := validateChildName("normal.txt"); err != nil {
		t.Errorf("validateChildName(\"normal.txt\") = %v，应当放行", err)
	}

	// 4) DirAppleMetadata 的两个入口（validateChildName 的真实消费方）。
	fs := newTestFS(t, false)
	writeFile(t, fs, "sub/f", "x")
	_, dm := dirHandle(t, fs, "sub")
	for _, n := range []string{".", ".."} {
		if _, _, err := dm.AppleInfoAt(n); err == nil {
			t.Errorf("AppleInfoAt(%q) = nil err —— 目录穿越原语", n)
		}
	}
	res, err := dm.AppleInfoAtBatch([]string{"f", ".", ".."})
	if err != nil {
		t.Fatalf("AppleInfoAtBatch: %v", err)
	}
	if res[0].Err != nil {
		t.Errorf("批量查 \"f\" 出错 %v，反向对照不成立", res[0].Err)
	}
	for i, n := range []string{".", ".."} {
		if res[i+1].Err == nil {
			t.Errorf("AppleInfoAtBatch 放行了 %q", n)
		}
	}

	// 5) lookupExactLocked：见上面两条专门的用例。
}

// ---------------------------------------------------------------- 枚举过滤

// TestQASnapshotFiltersUnrepresentableNames 钉死变异体 `snapshot-validate-removed`。
//
// snapshotLocked 会把「SMB 表达不了的名字」从枚举结果里滤掉（宿主机上
// 真实存在但客户端打不开的东西，列出来就是「看得见摸不着」）。
// 把那三行删掉，改动前全包测试没有一条变红。
//
// Linux 宿主上能造出来、而 SMB 侧一定打不开的名字：Windows 保留设备名。
// （含非法字符的名字造不出来 —— '/' 是分隔符、其余字符 Linux 都允许，
// 但 ValidateComponent 里那批 `"*:<>?\|` 在 Linux 上恰好都是合法文件名，
// 所以下面也一并造出来做对拍。）
func TestQASnapshotFiltersUnrepresentableNames(t *testing.T) {
	fs := newTestFS(t, false)
	root := fs.Root()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "d")

	hidden := []string{
		"CON",       // 设备名（旧表就有）
		"COM0",      // 设备名（PR#19 新表才有）
		"CONIN$",    // 设备名（PR#19 新表才有）
		"lpt\u00b9", // 上标数字变体（PR#19 新表才有）
		"a:b",       // ADS 分隔符
		"q?mark",    // Windows 非法字符
		"pipe|x",
		"star*x",
		"lt<gt>",
		"quote\"x",
		"back\\slash", // '\' 在 Linux 上是普通字符，在 SMB 侧是分隔符
	}
	visible := []string{"ok.txt", "CONSOLE", "COMx", "COM10"} // 反向对照：这些**必须**列出来
	for _, n := range append(append([]string{}, hidden...), visible...) {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Skipf("宿主建不出文件 %q: %v（本用例依赖 POSIX 文件名的宽松度）", n, err)
		}
	}

	h, _ := dirHandle(t, fs, "d")
	got := map[string]bool{}
	for _, e := range readDirOnce(t, h, "*", true) {
		got[e.Name] = true
	}
	for _, n := range hidden {
		if got[n] {
			t.Errorf("枚举列出了 %q —— 客户端按这个名字来打开必被 ValidateComponent 拒，"+
				"是「看得见摸不着」的幽灵条目", n)
		}
	}
	for _, n := range visible {
		if !got[n] {
			t.Errorf("枚举漏掉了合法名字 %q —— 过滤过头了", n)
		}
	}
}

// ---------------------------------------------------------------- Rename 别名

// TestQARenameSameDirEntryIsWiredIn 钉死变异体 `rename-samedirentry-off`。
//
// sameDirEntry 这个**判定函数**本身在 rename_alias_test.go 里测得很细，
// 但它在 LocalFS.Rename 里的**调用点**没有任何端到端用例：把
//
//	if !sameObject && sameDirEntry(oldDir, newDir, src, dstFI) { sameObject = true }
//
// 整段删掉，改动前全包测试全绿。这正是 AGENTS.md 说的「写好没接线」的
// 变种——写好了、接上了，但没人证明接上了，下一个人顺手删掉不会有人知道。
//
// 触发条件必须是「Lstat(dst) 成功且 dst 就是 src 自己」。ext4 上唯一能造出
// 这个局面的是**同目录硬链接**（大小写折叠那条要 NTFS/APFS，本容器造不出，
// 见 rename_alias_test.go 文件头的诚实说明）。
//
// 断言的是 local.go:598 明文写下的取舍：同目录互为硬链接的两个名字，
// rename 退化为 no-op，两个名字都还在、内容都还在。变异体下则会先
// os.Remove(dst) 再 rename —— 名字 b 的目录项被删掉过，行为可区分。
func TestQARenameSameDirEntryIsWiredIn(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "a.txt", "DATA")
	root := fs.Root()
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")); err != nil {
		t.Skipf("宿主不支持硬链接: %v", err)
	}

	if err := fs.Rename("a.txt", "b.txt", true); err != nil {
		t.Fatalf("Rename(a.txt -> b.txt, replace=true) = %v，"+
			"同一对象的改名不该报错", err)
	}

	// 接线在：判成同一对象 → 跳过 Remove → POSIX rename 同 inode 是 no-op。
	if _, err := os.Lstat(filepath.Join(root, "a.txt")); err != nil {
		t.Errorf("a.txt 不见了：%v —— sameDirEntry 的调用点被架空了，"+
			"走的是「先 os.Remove(dst) 再 rename」那条路（在会折叠名字的宿主上"+
			"这条路会直接删掉源文件、丢数据）", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "b.txt"))
	if err != nil || string(got) != "DATA" {
		t.Errorf("b.txt = (%q, %v)，期望 (\"DATA\", nil)", got, err)
	}
}

// ---------------------------------------------------------------- 配额

// TestQAQuotaOverfilledShareReportsZeroNotWrapAround 是配额的**溢出/下溢**核对。
//
// applyQuota 里 quotaBlocks - usedBlocks 是 uint64 减法。用量超过配额时若不
// 先比大小，结果会回绕成 ~1.8e19 块。这条已被现有 TestApplyQuotaNoUnderflow
// 覆盖（变异体 quota-free-underflow 是 KILLED 的），本用例补的是**上层观感**：
// 超配额时上报给客户端的可用空间必须恰好是 0，且 Avail<=Free<=Total 不倒挂。
func TestQAQuotaOverfilledShareReportsZeroNotWrapAround(t *testing.T) {
	const bs = 4096
	info := &FSInfo{
		BlockSize:   bs,
		TotalBlocks: 1 << 40,
		FreeBlocks:  1 << 40,
		AvailBlocks: 1 << 40,
	}
	l := &LocalFS{
		cfg:   LocalConfig{QuotaBytes: 10 * bs},
		usage: staticUsage(50 * bs), // 已用 50 块 > 配额 10 块
	}
	l.applyQuota(info)

	if info.FreeBlocks != 0 || info.AvailBlocks != 0 {
		t.Errorf("超配额时 Free=%d Avail=%d，期望都是 0（回绕的话会是天文数字）",
			info.FreeBlocks, info.AvailBlocks)
	}
	if info.TotalBlocks != 10 {
		t.Errorf("Total=%d，期望被压到配额 10 块", info.TotalBlocks)
	}
	if info.AvailBlocks > info.FreeBlocks || info.FreeBlocks > info.TotalBlocks {
		t.Errorf("倒挂：Avail=%d Free=%d Total=%d",
			info.AvailBlocks, info.FreeBlocks, info.TotalBlocks)
	}
}

// TestQAQuotaBlockCountNoOverflow：配额是 uint64 字节，块数换算不能溢出。
// quota_bytes 取 uint64 最大值（配置层允许）时，quotaBytes/bs 仍在范围内，
// 但 (usedBytes + bs - 1) 这个向上取整的加法在 usedBytes 接近 2^64 时会回绕。
// 实际用量不可能到那个量级，这里只是把边界钉住，防止将来把 used 改成
// 来自客户端可控的输入。
func TestQAQuotaBlockCountNoOverflow(t *testing.T) {
	const bs = 4096
	info := &FSInfo{BlockSize: bs, TotalBlocks: 1 << 30, FreeBlocks: 1 << 30, AvailBlocks: 1 << 30}
	l := &LocalFS{
		cfg:   LocalConfig{QuotaBytes: ^uint64(0)},
		usage: staticUsage(^uint64(0) - 1),
	}
	l.applyQuota(info)
	if info.FreeBlocks > info.TotalBlocks {
		t.Errorf("Free=%d > Total=%d，块数换算溢出了", info.FreeBlocks, info.TotalBlocks)
	}
	if info.AvailBlocks > info.FreeBlocks {
		t.Errorf("Avail=%d > Free=%d", info.AvailBlocks, info.FreeBlocks)
	}
}

// ---------------------------------------------------------------- 大小写冲突

// TestQAResolveParentCaseCollisionRoundTrip 是 PR#10（精确匹配优先）的
// **端到端**语义核对：目录里同时存在 "a.txt" 与 "A.TXT" 时，
// Remove / Rename / Open 打到的必须是客户端指名的那一个。
//
// path_case_test.go 已经从 Resolver 层证明了这一点；这里从 FileSystem 层
// 再走一遍，因为性能优化改的是 Resolver，而受影响的是这几个真实操作。
func TestQAResolveParentCaseCollisionRoundTrip(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "a.txt", "lower")
	writeFile(t, fs, "A.TXT", "UPPER")
	root := fs.Root()

	// Remove 指名 "A.TXT"：小写的那个必须还在。
	if err := fs.Remove("A.TXT"); err != nil {
		t.Fatalf("Remove(A.TXT): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "A.TXT")); !os.IsNotExist(err) {
		t.Error("Remove(A.TXT) 之后 A.TXT 还在 —— 删到了别的文件上")
	}
	got, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(got) != "lower" {
		t.Fatalf("a.txt = (%q, %v)，期望原封不动 —— Remove 折叠到了错误的目标", got, err)
	}

	// 剩下只有 "a.txt" 时，指名 "A.TXT" 必须回退折叠到它（大小写不敏感语义）。
	h, _, err := fs.Open(&OpenRequest{Path: "A.TXT", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatalf("精确 miss 后没有回退折叠：Open(A.TXT) = %v", err)
	}
	buf := make([]byte, 5)
	if _, err := h.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	_ = h.Close()
	if string(buf) != "lower" {
		t.Errorf("折叠后读到 %q，期望 \"lower\"", buf)
	}
}
