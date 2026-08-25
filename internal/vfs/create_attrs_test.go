package vfs

// create_attrs_test.go —— CREATE 请求携带的 FileAttributes 必须按 Samba
// 语义落地（B3，bh3 F1）。
//
// 缺陷背景：create.go 把 req.FileAttributes 填进 vfs.OpenRequest 之后，
// 整个处理链没有任何一处消费它 —— 客户端「带属性创建」退化成两步，
// QUERY_INFO 立刻回不到刚设的 HIDDEN。
//
// 对照的 Samba 行为（source3/smbd/open.c）：
//   - open_file_ntcreate: 静默剥掉 FILE_ATTRIBUTE_DIRECTORY（Windows 同款），
//     并叠加 FILE_ATTRIBUTE_ARCHIVE（"this mode is only used if the file is
//     created new"）；
//   - possibly_set_archive: FILE_WAS_OVERWRITTEN / FILE_WAS_SUPERSEDED 也补
//     ARCHIVE；
//   - FILE_WAS_OPENED 一律不动属性。

import (
	"testing"
)

// storedDOS 白盒直读旁路库里存的 DOS 位。
func storedDOS(t *testing.T, fs *LocalFS, rel string) (uint32, error) {
	t.Helper()
	return fs.caps.DOS().DOSAttributes(hostRef(fs, rel))
}

// TestCreatePersistsRequestedAttrs 带属性创建文件：
// DIRECTORY 位剥掉、叠 ARCHIVE、其余位存档，查询立刻可见。
func TestCreatePersistsRequestedAttrs(t *testing.T) {
	fs := metaFS(t)

	h, action, err := fs.Open(&OpenRequest{
		Path:           "doc.txt",
		Flags:          OpenWrite,
		Disposition:    CreateNew,
		FileAttributes: FileAttributeHidden | FileAttributeDirectory, // 客户端乱填 DIR 位
	})
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionCreated {
		t.Fatalf("action = %v", action)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	bits, err := storedDOS(t, fs, "doc.txt")
	if err != nil {
		t.Fatalf("旁路 dosattr 记录不存在（CREATE 属性被丢弃）: %v", err)
	}
	if bits&FileAttributeDirectory != 0 {
		t.Errorf("存储值 %#x 含 DIRECTORY 位；Samba 会静默剥掉", bits)
	}
	if want := FileAttributeHidden | FileAttributeArchive; bits != want {
		t.Errorf("存储值 = %#x, want %#x (HIDDEN|ARCHIVE)", bits, want)
	}

	a, err := fs.Stat("doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a.FileAttributes&FileAttributeHidden == 0 {
		t.Errorf("QUERY 视角属性 %#x 不含 HIDDEN —— 客户端刚设的位丢了", a.FileAttributes)
	}
}

// TestCreateWithoutAttrsLeavesNoRecord 未携带属性位的常规新建**不得**留旁路
// 记录：Time Machine 的 bands 目录动辄十万级条目，每次创建都写库是不可接受的；
// 可观测行为不受影响（合成逻辑对普通文件本来就报 ARCHIVE）。
func TestCreateWithoutAttrsLeavesNoRecord(t *testing.T) {
	fs := metaFS(t)

	if _, _, err := fs.Open(&OpenRequest{
		Path:        "plain.txt",
		Flags:       OpenWrite,
		Disposition: CreateNew,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := storedDOS(t, fs, "plain.txt"); err == nil {
		t.Fatal("无属性新建不应产生旁路 dosattr 记录")
	}
	a, err := fs.Stat("plain.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a.FileAttributes&FileAttributeArchive == 0 {
		t.Errorf("普通新建应报 ARCHIVE（合成），实际 %#x", a.FileAttributes)
	}
}

// TestOpenExistingNeverStoresAttrs 打开已存在文件时请求里的属性位一律忽略
// （Samba 只在 created/overwritten/superseded 时动属性）。
func TestOpenExistingNeverStoresAttrs(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "data")

	h, action, err := fs.Open(&OpenRequest{
		Path:           "f.txt",
		Flags:          OpenRead,
		Disposition:    OpenExisting,
		FileAttributes: FileAttributeHidden,
	})
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionOpened {
		t.Fatalf("action = %v", action)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storedDOS(t, fs, "f.txt"); err == nil {
		t.Fatal("plain open 不得写入属性记录")
	}
}

// TestOverwriteAddsArchive 覆盖截断时对已有存储记录补 ARCHIVE
// （possibly_set_archive 对 FILE_WAS_OVERWRITTEN 的行为）。
func TestOverwriteAddsArchive(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "old")
	plantMeta(t, fs, "f.txt", metaT0) // 存储值 = HIDDEN

	h, _, err := fs.Open(&OpenRequest{
		Path:        "f.txt",
		Flags:       OpenWrite,
		Disposition: TruncateExisting, // 客户端没带属性位
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	bits, err := storedDOS(t, fs, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := FileAttributeHidden | FileAttributeArchive; bits != want {
		t.Errorf("覆盖后存储值 = %#x, want %#x", bits, want)
	}
}

// TestSupersedeDropsStaleBypassMeta SUPERSEDE 是「删除重建」：
// 前任的旁路记录必须整体作废，新对象不得继承旧创建时间。
func TestSupersedeDropsStaleBypassMeta(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "old")
	plantMeta(t, fs, "f.txt", metaT1)

	h, action, err := fs.Open(&OpenRequest{
		Path:        "f.txt",
		Flags:       OpenWrite,
		Disposition: Supersede,
	})
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionSuperseded {
		t.Fatalf("action = %v", action)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := fs.caps.Times().CreationTime(hostRef(fs, "f.txt"))
	if err == nil && got.Equal(metaT1) {
		t.Fatalf("SUPERSEDE 后仍读到前任的创建时间 %v", got)
	}
	if bits, err := storedDOS(t, fs, "f.txt"); err == nil && bits&FileAttributeHidden != 0 {
		t.Fatalf("SUPERSEDE 后前任的 HIDDEN 位仍在: %#x", bits)
	}
}

// TestCreateDirStoresNonDirectoryBits 目录创建同理：
// DIRECTORY 不落地、不叠 ARCHIVE、其余位存档。
func TestCreateDirStoresNonDirectoryBits(t *testing.T) {
	fs := metaFS(t)

	h, _, err := fs.Open(&OpenRequest{
		Path:           "dir",
		Flags:          OpenDirectory | OpenWrite,
		Disposition:    CreateNew,
		FileAttributes: FileAttributeHidden | FileAttributeSystem | FileAttributeDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	bits, err := storedDOS(t, fs, "dir")
	if err != nil {
		t.Fatalf("目录创建属性未落地: %v", err)
	}
	if bits != FileAttributeHidden|FileAttributeSystem {
		t.Errorf("存储值 = %#x, want HIDDEN|SYSTEM（无 ARCHIVE、无 DIRECTORY）", bits)
	}
}

// TestMkdirStoresNonDirectoryBits 路径式 Mkdir 与 CREATE 建目录同语义。
func TestMkdirStoresNonDirectoryBits(t *testing.T) {
	fs := metaFS(t)
	if err := fs.Mkdir("d2", FileAttributeHidden); err != nil {
		t.Fatal(err)
	}
	bits, err := storedDOS(t, fs, "d2")
	if err != nil {
		t.Fatalf("Mkdir 属性未落地: %v", err)
	}
	if bits != FileAttributeHidden {
		t.Errorf("存储值 = %#x, want HIDDEN", bits)
	}
}
