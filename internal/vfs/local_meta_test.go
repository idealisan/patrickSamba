package vfs

// local_meta_test.go —— rename/remove 与旁路元数据账本的一致性（B5）。
//
// 缺陷背景（bh3 F3）：builtin 的 btime/dosattr 等 bucket 按 pathKey 记账，
// 但 Rename/Remove/DELETE_ON_CLOSE 从不迁移/清理，导致：
//   1. 改名后客户端设置过的创建时间与 DOS 位「丢」；
//   2. 旧路径残留记录会安到之后新建在该路径上的**无关文件**头上
//      （跨对象元数据泄漏）；
//   3. replace 改名覆盖目标时，目标的前任记录与迁入记录混在一起。
//
// 全部用例跑在 portable 档（六项能力全走 builtin 旁路库），
// 这样「记录在不在」可以直接问 caps 拿到确定性答案。

import (
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

var (
	metaT0 = time.Unix(1700000000, 0).UTC() // 种进旁路库的创建时间
	metaT1 = time.Unix(1000000000, 0).UTC() // 「前任」的陈旧创建时间
)

// metaFS 建一个 portable 档共享。
func metaFS(t *testing.T) *LocalFS {
	t.Helper()
	return newModeFS(t, oscap.ModePortable)
}

func hostRef(fs *LocalFS, rel string) oscap.Ref {
	return oscap.Ref{Path: fs.Root() + "/" + rel}
}

// plantMeta 走产品路径给 rel 种上创建时间与 HIDDEN 位
// （SET_INFO 的 FileBasicInformation 就是这么落到旁路库的）。
func plantMeta(t *testing.T, fs *LocalFS, rel string, bt time.Time) {
	t.Helper()
	h, _, err := fs.Open(&OpenRequest{
		Path:        rel,
		Flags:       OpenWrite | OpenRead,
		Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	err = h.SetAttr(&Attr{
		CreateTime:     bt,
		FileAttributes: FileAttributeHidden,
	}, AttrCreateTime|AttrFileAttributes)
	cerr := h.Close()
	if err != nil {
		t.Fatalf("plant %s: %v", rel, err)
	}
	if cerr != nil {
		t.Fatalf("close %s: %v", rel, cerr)
	}
}

func wantRecordAt(t *testing.T, fs *LocalFS, rel string, bt time.Time) {
	t.Helper()
	got, err := fs.caps.Times().CreationTime(hostRef(fs, rel))
	if err != nil {
		t.Fatalf("%s: 旁路 btime 应存在: %v", rel, err)
	}
	if !got.Equal(bt) {
		t.Fatalf("%s: btime = %v, want %v", rel, got, bt)
	}
	bits, err := fs.caps.DOS().DOSAttributes(hostRef(fs, rel))
	if err != nil || bits&FileAttributeHidden == 0 {
		t.Fatalf("%s: dosattr = %#x, %v；期望带 HIDDEN", rel, bits, err)
	}
}

func wantNoRecordAt(t *testing.T, fs *LocalFS, rel string) {
	t.Helper()
	if _, err := fs.caps.Times().CreationTime(hostRef(fs, rel)); err == nil {
		t.Errorf("%s: 旁路 btime 记录仍在，期望已清理", rel)
	}
	if _, err := fs.caps.DOS().DOSAttributes(hostRef(fs, rel)); err == nil {
		t.Errorf("%s: 旁路 dosattr 记录仍在，期望已清理", rel)
	}
}

// TestRenameMigratesBypassRecords 改名后创建时间与 DOS 位必须跟到新路径，
// 旧路径不得残留。
func TestRenameMigratesBypassRecords(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "a.txt", "hello")
	plantMeta(t, fs, "a.txt", metaT0)

	// 目标目录先建出来：vfs.Rename 不做隐式 mkdir（与 POSIX 一致）。
	h, _, err := fs.Open(&OpenRequest{
		Path:        "b",
		Flags:       OpenDirectory | OpenWrite,
		Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename("a.txt", "b/c.txt", false); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	wantRecordAt(t, fs, "b/c.txt", metaT0)
	wantNoRecordAt(t, fs, "a.txt")

	a, err := fs.Stat("b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !a.CreateTime.Equal(metaT0) {
		t.Fatalf("客户端视角 CreateTime = %v, want %v", a.CreateTime, metaT0)
	}
	if a.FileAttributes&FileAttributeHidden == 0 {
		t.Fatalf("客户端视角属性 = %#x；期望仍带 HIDDEN", a.FileAttributes)
	}
}

// TestRemoveClearsBypassRecords 删除文件必须清掉旁路记录。
func TestRemoveClearsBypassRecords(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "a.txt", "hello")
	plantMeta(t, fs, "a.txt", metaT0)

	if err := fs.Remove("a.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	wantNoRecordAt(t, fs, "a.txt")
}

// TestDeleteOnCloseClearsBypassRecords DELETE_ON_CLOSE 关闭即删，
// 旁路记录同样必须清掉。
func TestDeleteOnCloseClearsBypassRecords(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "a.txt", "hello")
	plantMeta(t, fs, "a.txt", metaT0)

	h, _, err := fs.Open(&OpenRequest{
		Path:        "a.txt",
		Flags:       OpenRead | OpenDeleteOnClose,
		Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	wantNoRecordAt(t, fs, "a.txt")
}

// TestRecycledPathGetsNoStaleMeta 路径复用不得继承前任的元数据：
// 这是 B5 缺陷最危险的形态 —— 新建的无关文件顶着别人的创建时间与 HIDDEN 位。
func TestRecycledPathGetsNoStaleMeta(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "a.txt", "old data")
	plantMeta(t, fs, "a.txt", metaT1)

	if err := fs.Remove("a.txt"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "a.txt", "brand new")

	wantNoRecordAt(t, fs, "a.txt")
	a, err := fs.Stat("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a.CreateTime.Equal(metaT1) {
		t.Fatalf("新文件继承了前任的创建时间 %v", metaT1)
	}
	if a.FileAttributes&FileAttributeHidden != 0 {
		t.Fatalf("新文件继承了前任的 HIDDEN 位: %#x", a.FileAttributes)
	}
}

// TestReplaceRenameDropsTargetRecords replace 改名覆盖目标时，
// 目标路径上的前任记录必须被清掉，不能与迁入记录合并。
func TestReplaceRenameDropsTargetRecords(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "src.txt", "src")
	writeFile(t, fs, "dst.txt", "dst")
	plantMeta(t, fs, "src.txt", metaT0)
	plantMeta(t, fs, "dst.txt", metaT1) // 目标的前任

	if err := fs.Rename("src.txt", "dst.txt", true); err != nil {
		t.Fatalf("Rename(replace): %v", err)
	}

	wantRecordAt(t, fs, "dst.txt", metaT0)
	wantNoRecordAt(t, fs, "src.txt")
}

// TestDirRenameMigratesChildren 目录改名要带走全部后代的旁路记录。
func TestDirRenameMigratesChildren(t *testing.T) {
	fs := metaFS(t)
	h, _, err := fs.Open(&OpenRequest{
		Path:        "d",
		Flags:       OpenDirectory | OpenWrite,
		Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fs, "d/kid.txt", "kid")
	plantMeta(t, fs, "d/kid.txt", metaT0)

	if err := fs.Rename("d", "e", false); err != nil {
		t.Fatalf("Rename(dir): %v", err)
	}

	wantRecordAt(t, fs, "e/kid.txt", metaT0)
	wantNoRecordAt(t, fs, "d/kid.txt")
}
