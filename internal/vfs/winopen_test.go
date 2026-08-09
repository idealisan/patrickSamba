package vfs

// winopen_test.go —— CreateFileW 入参翻译的表驱动测试。
//
// 全部在 Linux 上跑。我们没有 Windows 机器，所以凡是能从系统调用里剥出来的
// 判断都剥进了 winopen.go，这里就是在验那一半；open_windows.go 剩下的是
// "把这些参数原样递给 CreateFileW"，薄到肉眼可查。
//
// ⚠️ 这些用例证明的是**翻译正确**，不是"在 Windows 上跑通了"。
// 别在 PR 或报告里把它说成后者。

import (
	"errors"
	"os"
	"testing"
)

// 真实的重解析标记，用于 isNameSurrogateTag 的正反两侧对照。
// 值出自 winnt.h / MS-FSCC §2.1.2.1；SYMLINK 与 MOUNT_POINT 已与
// golang.org/x/sys@v0.47.0/windows/types_windows.go 核对一致。
const (
	tagSymlink     uint32 = 0xA000000C // IO_REPARSE_TAG_SYMLINK
	tagMountPoint  uint32 = 0xA0000003 // IO_REPARSE_TAG_MOUNT_POINT（junction）
	tagLxSymlink   uint32 = 0xA000001D // IO_REPARSE_TAG_LX_SYMLINK（WSL）
	tagGlobalRepar uint32 = 0xA0000019 // IO_REPARSE_TAG_GLOBAL_REPARSE
	tagDedup       uint32 = 0x80000013 // IO_REPARSE_TAG_DEDUP
	tagCloud       uint32 = 0x9000001A // IO_REPARSE_TAG_CLOUD（OneDrive 占位）
	tagCloud1      uint32 = 0x9000101A // IO_REPARSE_TAG_CLOUD_1
	tagWci         uint32 = 0x80000018 // IO_REPARSE_TAG_WCI（容器分层）
	tagAppExecLink uint32 = 0x8000001B // IO_REPARSE_TAG_APPEXECLINK
	tagAfUnix      uint32 = 0x80000023 // IO_REPARSE_TAG_AF_UNIX
)

func TestIsNameSurrogateTag(t *testing.T) {
	cases := []struct {
		tag  uint32
		want bool
		why  string
	}{
		// —— 会把名字重定向到别处：必须当成链接 ——
		{tagSymlink, true, "NTFS 符号链接"},
		{tagMountPoint, true, "junction —— Go 的 os 包不认它是符号链接，是主要的逃逸口"},
		{tagLxSymlink, true, "WSL 建的符号链接，Windows 侧同样会重定向"},
		{tagGlobalRepar, true, "全局重解析，同样是名字代理"},

		// —— 只是同一对象的另一种存储形态：不能误伤 ——
		{tagDedup, false, "重复数据删除。文件服务器上极常见，拒掉等于整卷不可共享"},
		{tagCloud, false, "OneDrive 占位文件"},
		{tagCloud1, false, "OneDrive 占位文件的变体"},
		{tagWci, false, "Windows 容器镜像的分层文件"},
		{tagAppExecLink, false, "应用执行别名，不是路径重定向"},
		{tagAfUnix, false, "AF_UNIX 套接字文件"},

		{0, false, "非重解析点"},
	}
	for _, c := range cases {
		if got := isNameSurrogateTag(c.tag); got != c.want {
			t.Errorf("isNameSurrogateTag(%#08x) = %v，应为 %v；%s", c.tag, got, c.want, c.why)
		}
	}
}

// allFlagCombos 枚举本项目实际会用到的 flag 组合。
// O_APPEND 不在内 —— 它被 winOpenParamsFor 显式拒绝，单独测。
func allFlagCombos() []int {
	base := []int{os.O_RDONLY, os.O_WRONLY, os.O_RDWR}
	extra := []int{0, os.O_CREATE, os.O_CREATE | os.O_EXCL, os.O_TRUNC,
		os.O_CREATE | os.O_TRUNC, os.O_CREATE | os.O_EXCL | os.O_TRUNC}
	out := make([]int, 0, len(base)*len(extra))
	for _, b := range base {
		for _, e := range extra {
			out = append(out, b|e)
		}
	}
	return out
}

// TestWinOpenParamsAlwaysSharesDelete —— 差异 1：永远带 FILE_SHARE_DELETE。
//
// Go 的 syscall.Open 只给 READ|WRITE，于是我们自己打开着的文件就改不了名、
// 删不掉。SMB 的共享冲突由 server 层按 ShareAccess 判，不能借宿主句柄语义。
// 这条对**每一种** flag 组合都必须成立，所以做成全组合遍历而不是抽样。
func TestWinOpenParamsAlwaysSharesDelete(t *testing.T) {
	for _, flag := range allFlagCombos() {
		p, err := winOpenParamsFor(flag, 0o644)
		if err != nil {
			t.Fatalf("flag=%#o: %v", flag, err)
		}
		want := winFileShareRead | winFileShareWrite | winFileShareDelete
		if p.ShareMode != want {
			t.Errorf("flag=%#o: ShareMode = %#x，应为 %#x（缺 DELETE 会让打开着的文件删不掉、改不了名）",
				flag, p.ShareMode, want)
		}
	}
}

// TestWinOpenParamsAlwaysNoFollow —— 差异 2：永远带 FILE_FLAG_OPEN_REPARSE_POINT。
//
// Go 只在 CREATE_NEW 时加它，其余情况一律跟随重解析点 —— 等于 Linux 那边
// O_NOFOLLOW 的防护在 Windows 上整个不存在（junction 尤其危险，Go 的 os 包
// 连 ModeSymlink 都不给它）。同样做全组合遍历：只要有一种组合漏了，那就是
// 一条可用的逃逸路径。
func TestWinOpenParamsAlwaysNoFollow(t *testing.T) {
	for _, flag := range allFlagCombos() {
		p, err := winOpenParamsFor(flag, 0o644)
		if err != nil {
			t.Fatalf("flag=%#o: %v", flag, err)
		}
		if p.FlagsAndAttributes&winFileFlagOpenReparsePoint == 0 {
			t.Errorf("flag=%#o: 缺 FILE_FLAG_OPEN_REPARSE_POINT，这条路径会跟随 junction 逃出共享根", flag)
		}
		if p.FlagsAndAttributes&winFileFlagBackupSemantics == 0 {
			t.Errorf("flag=%#o: 缺 FILE_FLAG_BACKUP_SEMANTICS，打不开目录句柄", flag)
		}
	}
}

func TestWinOpenParamsAccess(t *testing.T) {
	cases := []struct {
		name string
		flag int
		want uint32
	}{
		{"只读", os.O_RDONLY, winGenericRead},
		{"只写", os.O_WRONLY, winGenericWrite},
		{"读写", os.O_RDWR, winGenericRead | winGenericWrite},
		{"只读+创建", os.O_RDONLY | os.O_CREATE, winGenericRead | winGenericWrite},
		{"只写+创建", os.O_WRONLY | os.O_CREATE, winGenericWrite},
		{"读写+创建+独占", os.O_RDWR | os.O_CREATE | os.O_EXCL, winGenericRead | winGenericWrite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := winOpenParamsFor(c.flag, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if p.Access != c.want {
				t.Fatalf("Access = %#x，应为 %#x", p.Access, c.want)
			}
		})
	}
}

func TestWinOpenParamsDispositionAndTruncate(t *testing.T) {
	cases := []struct {
		name         string
		flag         int
		wantDisp     uint32
		wantTruncate bool
	}{
		{"打开已存在", os.O_RDONLY, winOpenExisting, false},
		{"截断已存在", os.O_RDWR | os.O_TRUNC, winOpenExisting, true},
		{"没有就建", os.O_RDWR | os.O_CREATE, winOpenAlways, false},
		{"没有就建且截断", os.O_RDWR | os.O_CREATE | os.O_TRUNC, winOpenAlways, true},
		{"必须新建", os.O_RDWR | os.O_CREATE | os.O_EXCL, winCreateNew, false},
		// CREATE_NEW 拿到的一定是空文件，没什么可截断的。
		{"必须新建+多余的截断", os.O_RDWR | os.O_CREATE | os.O_EXCL | os.O_TRUNC, winCreateNew, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := winOpenParamsFor(c.flag, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if p.CreateDisposition != c.wantDisp {
				t.Errorf("CreateDisposition = %d，应为 %d", p.CreateDisposition, c.wantDisp)
			}
			if p.TruncateAfterOpen != c.wantTruncate {
				t.Errorf("TruncateAfterOpen = %v，应为 %v", p.TruncateAfterOpen, c.wantTruncate)
			}
		})
	}
}

// TestWinOpenParamsNeverUsesCreateAlways —— 钉死 go.dev/issue/38225 的规避。
//
// CREATE_ALWAYS 配上 FILE_ATTRIBUTE_READONLY 会把已存在的文件**替换**成一个
// 新的只读文件，而不是截断它 —— 那是静默的数据丢失。后来者看到
// TruncateAfterOpen 这个别扭的字段很可能想"直接用 CREATE_ALWAYS 不就好了"，
// 这条用例就是拦这个念头的。
func TestWinOpenParamsNeverUsesCreateAlways(t *testing.T) {
	const createAlways, truncateExisting uint32 = 2, 5
	for _, flag := range allFlagCombos() {
		p, err := winOpenParamsFor(flag, 0o444) // 只读 perm，正是踩坑的前提
		if err != nil {
			t.Fatalf("flag=%#o: %v", flag, err)
		}
		if p.CreateDisposition == createAlways || p.CreateDisposition == truncateExisting {
			t.Errorf("flag=%#o: 用了 CREATE_ALWAYS/TRUNCATE_EXISTING（%d）；"+
				"配 FILE_ATTRIBUTE_READONLY 会替换而不是截断已有文件，见 go.dev/issue/38225",
				flag, p.CreateDisposition)
		}
	}
}

func TestWinOpenParamsReadonlyAttribute(t *testing.T) {
	p, err := winOpenParamsFor(os.O_RDWR|os.O_CREATE, 0o444)
	if err != nil {
		t.Fatal(err)
	}
	if p.FlagsAndAttributes&winFileAttributeReadonly == 0 {
		t.Error("perm 无属主写位时应当带 FILE_ATTRIBUTE_READONLY")
	}
	// 反向对照：有写位时不能带 READONLY，否则新建的文件立刻变只读。
	p, err = winOpenParamsFor(os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if p.FlagsAndAttributes&winFileAttributeReadonly != 0 {
		t.Error("perm 有属主写位时不应当带 FILE_ATTRIBUTE_READONLY")
	}
	if p.FlagsAndAttributes&winFileAttributeNormal == 0 {
		t.Error("perm 有属主写位时应当带 FILE_ATTRIBUTE_NORMAL")
	}
}

// TestWinOpenParamsRejectsAppend —— O_APPEND 是禁止项，不是未实现项。
//
// SMB2 WRITE 永远带显式 offset，内核替我们改写 offset 会让内容错乱。
// 默默忽略它比报错危险得多，所以这里必须是硬失败。
func TestWinOpenParamsRejectsAppend(t *testing.T) {
	_, err := winOpenParamsFor(os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		t.Fatal("O_APPEND 应当被拒绝：SMB2 WRITE 是 pwrite 语义，内核追加会写错位置")
	}
	if !errors.Is(err, ErrInvalidArg) {
		t.Fatalf("错误应当可 errors.Is 到 ErrInvalidArg，实得 %v", err)
	}
}
