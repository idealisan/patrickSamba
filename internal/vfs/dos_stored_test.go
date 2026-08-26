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
