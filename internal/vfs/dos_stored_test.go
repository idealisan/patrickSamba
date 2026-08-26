package vfs

// dos_stored_test.go —— DOS 属性存储语义（bh3 F5 / F8）。
//
// F8：SET_INFO 带来的 FileAttributes 落库前必须滤掉客观事实位
// （DIRECTORY / SPARSE / REPARSE）。Samba 对照：
//   - smb_set_file_dosmode（source3/smbd/smb2_trans2.c:3906–3914）
//     目录强制加 DIRECTORY、非目录剥掉；
//   - file_set_dosmode（dosmode.c:953）先 & SAMBA_ATTRIBUTES_MASK；
//   - parse 侧对 SPARSE/REPARSE 单独处理「valid on get but not on set」
//     （dosmode.c:377–380）。
//
// F5：合成方向必须是「存储值优先」（Samba 默认 store dos attributes=yes
// 时 fdos_mode 的行为，dosmode.c:710–748），而不是把存储位 OR 到
// 权限位推导结果上 —— OR 语义让「清除只读」被宿主权限位的重新推导
// 静默冲掉。

import (
	"os"
	"testing"
)

// TestSetAttrFiltersObjectiveDOSBits（F8）：客户端经 SET_INFO 设置的
// DIRECTORY/SPARSE/REPARSE 是文件系统客观事实，入库前必须剥掉，
// 否则一旦读路径改成存储值优先（F5），记录里的假客观位会直接冒出来。
func TestSetAttrFiltersObjectiveDOSBits(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "data")

	h, _, err := fs.Open(&OpenRequest{
		Path:        "f.txt",
		Flags:       OpenWrite | OpenRead,
		Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := FileAttributeReadonly | FileAttributeDirectory |
		FileAttributeSparse | FileAttributeReparse
	err = h.SetAttr(&Attr{FileAttributes: raw}, AttrFileAttributes)
	cerr := h.Close()
	if err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	if cerr != nil {
		t.Fatal(cerr)
	}

	bits, err := storedDOS(t, fs, "f.txt")
	if err != nil {
		t.Fatalf("属性记录应已落库: %v", err)
	}
	for _, bit := range []struct {
		mask uint32
		name string
	}{
		{FileAttributeDirectory, "DIRECTORY"},
		{FileAttributeSparse, "SPARSE"},
		{FileAttributeReparse, "REPARSE"},
	} {
		if bits&bit.mask != 0 {
			t.Errorf("存储值 %#x 含客观位 %s；Samba 的 SAMBA_ATTRIBUTES_MASK 会滤掉它", bits, bit.name)
		}
	}
	if bits&FileAttributeReadonly == 0 {
		t.Errorf("存储值 %#x 丢了客户端要求的 READONLY 位", bits)
	}
}

// TestStoredWinsClearsDerivedReadonly（F5 核心场景）：宿主权限位推导出的
// READONLY 必须能被客户端的显式设置**清除**。OR 语义下 SET_INFO 清了只读，
// 下一次读又被 chmod 的重新推导合回来 —— 「清除只读」静默失效。
//
// Samba 对照：store dos attributes=yes（loadparm.c:3218 默认）时 fdos_mode
// （dosmode.c:710–748）只采信 xattr 记录，map_readonly 推导根本不参与读路径。
func TestStoredWinsClearsDerivedReadonly(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "data")
	// 宿主文件保持只读权限位（builtin 档不回写 chmod，这正是缺陷现场）：
	if err := os.Chmod(fs.Root()+"/f.txt", 0o444); err != nil {
		t.Fatal(err)
	}

	h, _, err := fs.Open(&OpenRequest{
		Path: "f.txt", Flags: OpenWrite | OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 客户端「清除只读」：显式设 ARCHIVE（不含 READONLY）。
	err = h.SetAttr(&Attr{FileAttributes: FileAttributeArchive}, AttrFileAttributes)
	cerr := h.Close()
	if err != nil || cerr != nil {
		t.Fatalf("SetAttr/Close: %v / %v", err, cerr)
	}

	a, err := fs.Stat("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a.FileAttributes&FileAttributeReadonly != 0 {
		t.Errorf("客户端已清除只读，QUERY 仍报 READONLY（位图 %#x）—— OR 合成把推导位加了回来", a.FileAttributes)
	}
	if a.FileAttributes&FileAttributeArchive == 0 {
		t.Errorf("位图 %#x 应含存储的 ARCHIVE", a.FileAttributes)
	}
}

// TestStoredReadonlyWinsOnWritableHost：反向同样成立 —— 可写宿主文件被
// 显式设 READONLY 后必须报只读，不能因为没有权限位依据就丢掉记录值。
func TestStoredReadonlyWinsOnWritableHost(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "data")

	h, _, err := fs.Open(&OpenRequest{
		Path: "f.txt", Flags: OpenWrite | OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	// SetAttr 是整条记录替换，可设置位一次带上：READONLY + HIDDEN。
	err = h.SetAttr(&Attr{
		FileAttributes: FileAttributeReadonly | FileAttributeHidden,
	}, AttrFileAttributes)
	cerr := h.Close()
	if err != nil || cerr != nil {
		t.Fatalf("SetAttr/Close: %v / %v", err, cerr)
	}

	a, err := fs.Stat("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a.FileAttributes&FileAttributeReadonly == 0 {
		t.Errorf("存储 READONLY 未生效，位图 %#x", a.FileAttributes)
	}
	// HIDDEN 来自同一条记录，不是点开头名字 —— 同样必须保留。
	if a.FileAttributes&FileAttributeHidden == 0 {
		t.Errorf("存储 HIDDEN 被合成逻辑挤掉，位图 %#x", a.FileAttributes)
	}
}

// TestStoredKeepsObjectiveFacts（对齐 hide_dot_files 与目录强制）：
// 存储值优先时，客观事实位仍由宿主实况决定 —— 目录恒带 DIRECTORY、
// 点开头的名字恒追加 HIDDEN（Samba dos_mode_post，dosmode.c:594–608/662–666）。
func TestStoredKeepsObjectiveFacts(t *testing.T) {
	fs := metaFS(t)
	if err := fs.Mkdir("d", 0); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, ".dotfile", "x")

	for _, tc := range []struct {
		rel      string
		wantMask uint32
	}{
		{"d", FileAttributeDirectory},
		{".dotfile", FileAttributeHidden},
	} {
		// 给两个对象都种一份不含任何客观位的记录。
		h, _, err := fs.Open(&OpenRequest{
			Path: tc.rel, Flags: OpenWrite | OpenRead, Disposition: OpenExisting,
		})
		if err != nil {
			t.Fatalf("open %s: %v", tc.rel, err)
		}
		err = h.SetAttr(&Attr{FileAttributes: FileAttributeArchive}, AttrFileAttributes)
		cerr := h.Close()
		if err != nil || cerr != nil {
			t.Fatalf("%s: SetAttr/Close: %v / %v", tc.rel, err, cerr)
		}
		a, err := fs.Stat(tc.rel)
		if err != nil {
			t.Fatal(err)
		}
		if a.FileAttributes&tc.wantMask == 0 {
			t.Errorf("%s: 位图 %#x 缺少客观/名字派生位 %#x", tc.rel, a.FileAttributes, tc.wantMask)
		}
	}
}

// TestStoredIgnoresJunkObjectiveBits（旧记录兼容）：F8 之前的 bbolt 库里
// 可能存有 DIRECTORY/SPARSE/REPARSE 垃圾位。读路径不得把它们当成真话
// 报给客户端 —— 客观位永远以宿主实况为准。
func TestStoredIgnoresJunkObjectiveBits(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "data") // 普通非稀疏文件

	junk := FileAttributeDirectory | FileAttributeSparse | FileAttributeReparse |
		FileAttributeHidden
	ref := hostRef(fs, "f.txt")
	if err := fs.caps.DOS().SetDOSAttributes(ref, junk); err != nil {
		t.Fatalf("种旧格式记录: %v", err)
	}

	a, err := fs.Stat("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, bit := range []struct {
		mask uint32
		name string
	}{
		{FileAttributeDirectory, "DIRECTORY"},
		{FileAttributeSparse, "SPARSE"},
		{FileAttributeReparse, "REPARSE"},
	} {
		if a.FileAttributes&bit.mask != 0 {
			t.Errorf("普通文件报出客观位 %s（来自旧记录垃圾 %#x），读路径未过滤", bit.name, a.FileAttributes)
		}
	}
	if a.FileAttributes&FileAttributeHidden == 0 {
		t.Errorf("记录里的可设置位 HIDDEN 应保留，位图 %#x", a.FileAttributes)
	}
}
