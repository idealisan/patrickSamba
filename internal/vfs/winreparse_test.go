package vfs

// winreparse_test.go —— Windows 重解析判定与共享根包含性的表驱动测试。
//
// 全部在 Linux 上跑（本文件与 winreparse.go 都不带 build tag）。
//
// ⚠️ 这些用例证明的是**判定规则正确**，不是「在 Windows 上跑通了」。
// 真机上「建一个 junction、经 SMB 访问、确认被拒」这条端到端验证，
// 本项目没有 Windows 机器，**至今未做**。别在 PR 或报告里把两者混为一谈。

import "testing"

// winFileAttributeDirectory 仅测试用，凑一个真实的属性组合。
const winFileAttributeDirectory uint32 = 0x00000010

func TestWinIsRedirect(t *testing.T) {
	const rp = winFileAttributeReparsePoint

	cases := []struct {
		name  string
		attrs uint32
		tag   uint32
		want  bool
		why   string
	}{
		// —— 会把名字重定向出去：必须拦 ——
		{"符号链接", rp, tagSymlink, true, "NTFS 符号链接"},
		{"junction", rp | winFileAttributeDirectory, tagMountPoint, true,
			"junction —— Go 不认它是 symlink，是主要逃逸口"},
		{"WSL 符号链接", rp, tagLxSymlink, true, "WSL 建的链接，Windows 侧同样重定向"},
		{"全局重解析", rp, tagGlobalRepar, true, "同样是 name surrogate"},

		// —— 只是另一种存储形态：拦了就是误伤整卷 ——
		{"重删", rp, tagDedup, false, "文件服务器上极常见，拒掉等于整卷不可共享"},
		{"OneDrive 占位", rp, tagCloud, false, "云同步占位文件"},
		{"OneDrive 占位变体", rp, tagCloud1, false, "同上"},
		{"容器分层", rp, tagWci, false, "Windows 容器镜像分层文件"},
		{"应用执行别名", rp, tagAppExecLink, false, "不是路径重定向"},
		{"AF_UNIX", rp, tagAfUnix, false, "套接字文件"},

		// —— 没有 REPARSE_POINT 属性时 tag 字段是垃圾，绝不能参与判断 ——
		{"普通文件但 tag 位置有脏值", 0, tagMountPoint, false,
			"不带 FILE_ATTRIBUTE_REPARSE_POINT 时 dwReserved0 存的是别的东西，" +
				"读它会把普通文件误判成 junction"},
		{"普通目录", winFileAttributeDirectory, tagSymlink, false, "同上"},
		{"全零", 0, 0, false, "普通文件"},
	}

	for _, c := range cases {
		if got := winIsRedirect(c.attrs, c.tag); got != c.want {
			t.Errorf("%s: winIsRedirect(attrs=%#08x, tag=%#08x) = %v，应为 %v；%s",
				c.name, c.attrs, c.tag, got, c.want, c.why)
		}
	}
}

// TestWinRedirectClosesLegacyGap 是这次修复的**反向对照**。
//
// 它不验证「新代码能跑」，而是验证「旧判据确实放行了 junction」——
// 也就是把被修复的那个缺口本身钉成一条会失败的断言。
//
// 没有这一条，「Windows 符号链接逃逸已修复」就只是一句自述：
// 谁也说不清修的到底是不是一个真实存在过的洞。
func TestWinRedirectClosesLegacyGap(t *testing.T) {
	const rp = winFileAttributeReparsePoint

	// 旧判据（等价于 path.go 原先的 fi.Mode()&os.ModeSymlink != 0）
	// 对这些标记是**放行**的 —— 每一条都是一个可用的逃逸口。
	escapes := []struct {
		tag uint32
		why string
	}{
		{tagMountPoint, "junction：mklink /J，普通用户即可创建，无需管理员"},
		{tagLxSymlink, "WSL 符号链接：装了 WSL 的机器上普通用户即可创建"},
		{tagGlobalRepar, "全局重解析"},
	}

	for _, e := range escapes {
		if winLegacyIsRedirect(rp, e.tag) {
			t.Fatalf("tag %#08x 旧判据居然拦住了 —— 说明本用例的前提（缺口存在）"+
				"已经不成立，请重新确认 winLegacyIsRedirect 是否还忠实复刻旧行为", e.tag)
		}
		if !winIsRedirect(rp, e.tag) {
			t.Errorf("tag %#08x 仍未被拦截，逃逸口没堵上；%s", e.tag, e.why)
		}
	}

	// 反过来：符号链接是旧判据**唯一**认得的一种，新判据不能把它弄丢。
	if !winLegacyIsRedirect(rp, tagSymlink) || !winIsRedirect(rp, tagSymlink) {
		t.Error("IO_REPARSE_TAG_SYMLINK 新旧判据都应拦截")
	}

	// 而且新判据不能顺手把非重定向的重解析点也一起拦了（那是另一种事故）。
	for _, tag := range []uint32{tagDedup, tagCloud, tagWci, tagAppExecLink} {
		if winIsRedirect(rp, tag) {
			t.Errorf("tag %#08x 被误拦，会导致整卷/整目录不可共享", tag)
		}
	}
}

func TestWinPathContains(t *testing.T) {
	const root = `\\?\C:\srv\share`

	cases := []struct {
		p    string
		want bool
		why  string
	}{
		{root, true, "根自身在根内"},
		{root + `\a.txt`, true, "直接子项"},
		{root + `\d\e\f.txt`, true, "深层子项"},

		// —— 边界：前缀必须卡在分隔符上 ——
		{`\\?\C:\srv\shareEvil`, false, "同前缀的兄弟目录，最经典的前缀比较漏判"},
		{`\\?\C:\srv\shareEvil\x.txt`, false, "同上，带子路径"},
		{`\\?\C:\srv\share_backup`, false, "下划线同样不是分隔符"},

		// —— 真正的越界 ——
		{`\\?\C:\Windows\System32\config\SAM`, false, "junction 逃逸的典型目标"},
		{`\\?\C:\srv`, false, "父目录不在根内"},
		{`\\?\D:\srv\share\a.txt`, false, "换了个卷"},

		// —— 大小写：刻意精确比较，宁可误拒不可误放 ——
		{`\\?\C:\SRV\SHARE\a.txt`, false,
			"Win10 1803+ 的按目录大小写敏感下这可能是另一个目录；" +
				"折叠比较会造成假接受"},

		// —— UNC 形式（GetFinalPathNameByHandleW 对网络路径的输出）——
		{`\\?\UNC\srv\share\a.txt`, false, "UNC 与本地盘符是不同的根"},

		{"", false, "空路径"},
	}

	for _, c := range cases {
		if got := winPathContains(root, c.p); got != c.want {
			t.Errorf("winPathContains(%q, %q) = %v，应为 %v；%s",
				root, c.p, got, c.want, c.why)
		}
	}
}

// TestWinPathContainsDriveRoot 单拎出来：共享根是盘符根时末尾自带分隔符，
// 是最容易拼出 `\\?\C:\\` 的地方。
func TestWinPathContainsDriveRoot(t *testing.T) {
	for _, root := range []string{`\\?\C:\`, `\\?\C:`} {
		if !winPathContains(root, `\\?\C:\a.txt`) {
			t.Errorf("root=%q：盘符根下的文件应判为在根内", root)
		}
		if !winPathContains(root, `\\?\C:`) {
			t.Errorf("root=%q：根自身应判为在根内", root)
		}
		if winPathContains(root, `\\?\D:\a.txt`) {
			t.Errorf("root=%q：别的卷不应判为在根内", root)
		}
	}
}

func TestWinStripLongPathPrefix(t *testing.T) {
	cases := []struct {
		in, want, why string
	}{
		{`\\?\C:\srv\share`, `C:\srv\share`, "最常见的盘符形式"},
		{`\\?\C:\srv\share\a.txt`, `C:\srv\share\a.txt`, "带子路径"},
		{`\\?\c:\srv`, `c:\srv`, "小写盘符同样还原，大小写留给上层判定"},
		{`\\?\UNC\host\share\a.txt`, `\\host\share\a.txt`, "UNC 形式还原成双反斜杠"},
		{`C:\srv\share`, `C:\srv\share`, "已经是普通形式，原样返回"},
		{`\\host\share`, `\\host\share`, "普通 UNC，原样返回"},
		{"", "", "空串"},

		// 认不出的形式必须原样返回 —— 后续前缀比较会判它在共享外（拒绝）。
		// 猜一个等价形式反而可能猜出一个**能匹配上**的串，那是假接受。
		{`\\?\Volume{12345678-1234-1234-1234-123456789abc}\a.txt`,
			`\\?\Volume{12345678-1234-1234-1234-123456789abc}\a.txt`,
			"卷 GUID 形式不还原，让它匹配不上从而被拒"},
		{`\\?\1:\x`, `\\?\1:\x`, "盘符不是字母，不认"},
	}
	for _, c := range cases {
		if got := winStripLongPathPrefix(c.in); got != c.want {
			t.Errorf("winStripLongPathPrefix(%q) = %q，应为 %q；%s",
				c.in, got, c.want, c.why)
		}
	}
}

// TestWinStripThenContains 把两个函数串起来验一遍，这才是真实调用形态：
// final path 先还原形式，再与共享根做包含性判定。
func TestWinStripThenContains(t *testing.T) {
	const root = `C:\srv\share` // Resolver.root 的形式（EvalSymlinks 产出）

	cases := []struct {
		final string
		want  bool
		why   string
	}{
		{`\\?\C:\srv\share\a.txt`, true, "junction 指回共享内，应放行"},
		{`\\?\C:\srv\share`, true, "指向共享根自身"},
		{`\\?\C:\Windows\System32\config\SAM`, false, "junction 逃逸到系统目录"},
		{`\\?\C:\srv\shareEvil\x`, false, "同前缀兄弟目录"},
		{`\\?\D:\srv\share\a.txt`, false, "换卷"},
		{`\\?\UNC\evil\share\a.txt`, false, "指向网络路径"},
		{`\\?\Volume{12345678-1234-1234-1234-123456789abc}\srv\share\a.txt`, false,
			"卷 GUID 形式还原不了，按拒绝处理"},
	}
	for _, c := range cases {
		got := winPathContains(root, winStripLongPathPrefix(c.final))
		if got != c.want {
			t.Errorf("final=%q → %v，应为 %v；%s", c.final, got, c.want, c.why)
		}
	}
}

// TestWinPathContainsEmptyRootDenies 钉死失败方向：
// 根为空（配置异常 / 取 final path 失败）时必须**拒绝**，不能退化成放行一切。
func TestWinPathContainsEmptyRootDenies(t *testing.T) {
	for _, root := range []string{"", `\`, `\\`} {
		if winPathContains(root, `\\?\C:\Windows\System32\config\SAM`) {
			t.Errorf("root=%q 时不应放行任何路径：空根必须 fail closed", root)
		}
	}
}
