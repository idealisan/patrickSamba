package builtin

// store_migration_test.go —— 旁路存储按路径迁移/清理（B5）。
//
// 缺陷背景（bh3 F3）：bbolt 各 bucket 按 pathKey 记账却没有迁移/清理 API，
// rename 之后旧路径的记录残留，之后任何新建在旧路径上的无关文件都会
// 通过 objKey 命中前任的 btime/DOS 记录 —— 跨对象元数据泄漏。
//
// 这组用例钉的是 store 层的**前缀语义**：
//   - 精确键：objKey(p)；
//   - 本对象的具名子项：pathKey+NUL+name（xattr / stream）；
//   - 子树：pathKey+"/" 开头的一切（目录改名要带走全部后代）。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// seedAllBuckets 在 path（宿主文件）上给六个 bucket 都种一条记录，
// 返回种下的 btime 值供断言比对。
func seedAllBuckets(t *testing.T, a *adapter, path string) time.Time {
	t.Helper()
	// holes 桶的记账要求宿主文件真实存在（打洞语义要写零）。
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("seed mkdir: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, 8192), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	bt := seedMetaOnly(t, a, path, true)
	return bt
}

// seedDirBuckets 给目录路径种记录（除 holes 外的五个 bucket：
// 打洞只对文件有意义）。宿主目录一并建出，贴近真实使用。
func seedDirBuckets(t *testing.T, a *adapter, path string) time.Time {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	return seedMetaOnly(t, a, path, false)
}

func seedMetaOnly(t *testing.T, a *adapter, path string, withHoles bool) time.Time {
	t.Helper()
	ref := oscap.Ref{Path: path}
	bt := time.Unix(1700000000, 123456789).UTC()
	if err := a.SetCreationTime(ref, bt); err != nil {
		t.Fatalf("seed btime: %v", err)
	}
	if err := a.SetDOSAttributes(ref, 0x2|0x20); err != nil { // HIDDEN|ARCHIVE
		t.Fatalf("seed dosattr: %v", err)
	}
	if err := a.SetXattr(ref, "user.tag", []byte("v1")); err != nil {
		t.Fatalf("seed xattr: %v", err)
	}
	if withHoles {
		if err := a.PunchHole(ref, 0, 4096); err != nil {
			t.Fatalf("seed holes: %v", err)
		}
	}
	h, err := a.OpenStream(ref, "AFP_Resource", oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("seed stream: %v", err)
	}
	if _, err := h.WriteAt([]byte("rsrc"), 0); err != nil {
		t.Fatalf("seed stream write: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("seed stream close: %v", err)
	}
	return bt
}

func wantTimes(t *testing.T, a *adapter, path string, want time.Time) {
	t.Helper()
	got, err := a.CreationTime(oscap.Ref{Path: path})
	if err != nil {
		t.Fatalf("%s: CreationTime: %v", path, err)
	}
	if !got.Equal(want) {
		t.Fatalf("%s: CreationTime = %v, want %v", path, got, want)
	}
}

func wantMissingEverywhere(t *testing.T, a *adapter, path string) {
	t.Helper()
	if _, err := a.CreationTime(oscap.Ref{Path: path}); err == nil {
		t.Errorf("%s: btime 记录仍在，期望已清理", path)
	}
	if _, err := a.DOSAttributes(oscap.Ref{Path: path}); err == nil {
		t.Errorf("%s: dosattr 记录仍在，期望已清理", path)
	}
	names, err := a.ListXattr(oscap.Ref{Path: path})
	if err != nil || len(names) != 0 {
		t.Errorf("%s: xattr = %v, %v；期望空", path, names, err)
	}
	streams, err := a.ListStreams(oscap.Ref{Path: path})
	if err != nil || len(streams) != 0 {
		t.Errorf("%s: streams = %v, %v；期望空", path, streams, err)
	}
}

// TestRenameMovesAllBuckets 六个 bucket 的记录都必须跟着路径走。
func TestRenameMovesAllBuckets(t *testing.T) {
	e := newEnv(t)
	a := e.set.Xattr.(*adapter)

	old := e.root + "/a/f.txt"
	new := e.root + "/b/g.txt"
	bt := seedAllBuckets(t, a, old)

	if err := a.RenameMetadata(old, new); err != nil {
		t.Fatalf("RenameMetadata: %v", err)
	}
	// 与真实调用方（vfs.Rename）的顺序一致：宿主文件先/后搬都行，
	// 但查询类断言（holes 需要 stat 到文件）要求宿主对象在新路径上存在。
	if err := os.MkdirAll(filepath.Dir(new), 0o755); err != nil {
		t.Fatalf("mkdir dst dir: %v", err)
	}
	if err := os.Rename(old, new); err != nil {
		t.Fatalf("host rename: %v", err)
	}

	wantTimes(t, a, new, bt)
	bits, err := a.DOSAttributes(oscap.Ref{Path: new})
	if err != nil || bits != 0x22 {
		t.Fatalf("新路径 DOS = %#x, %v; want 0x22", bits, err)
	}
	names, err := a.ListXattr(oscap.Ref{Path: new})
	if err != nil || len(names) != 1 || names[0] != "user.tag" {
		t.Fatalf("新路径 xattr = %v, %v", names, err)
	}
	ss, err := a.ListStreams(oscap.Ref{Path: new})
	if err != nil || len(ss) != 1 || ss[0].Name != "AFP_Resource" {
		t.Fatalf("新路径 streams = %v, %v", ss, err)
	}
	rs, err := a.AllocatedRanges(oscap.Ref{Path: new}, 0, 8192)
	if err != nil || len(rs) != 1 {
		t.Fatalf("新路径 holes = %v, %v", rs, err)
	}

	wantMissingEverywhere(t, a, old)
}

// TestRenameMovesSubtree 目录改名必须带走全部后代的记录。
func TestRenameMovesSubtree(t *testing.T) {
	e := newEnv(t)
	a := e.set.Xattr.(*adapter)

	dirOld := e.root + "/dir"
	dirNew := e.root + "/nest/dir"
	kidOld := dirOld + "/kid.txt"
	kidNew := dirNew + "/kid.txt"
	bt := seedAllBuckets(t, a, kidOld)
	seedDirBuckets(t, a, dirOld)

	if err := a.RenameMetadata(dirOld, dirNew); err != nil {
		t.Fatalf("RenameMetadata: %v", err)
	}

	wantTimes(t, a, kidNew, bt)
	wantTimes(t, a, dirNew, bt)
	wantMissingEverywhere(t, a, kidOld)
	wantMissingEverywhere(t, a, dirOld)
}

// TestPrefixMustNotBleedAcrossSiblingNames 前缀匹配必须尊重边界：
// "a/f.txt" 的迁移不得碰 "a/f.txt.bak"（字符串前缀相同但路径不同）。
func TestPrefixMustNotBleedAcrossSiblingNames(t *testing.T) {
	e := newEnv(t)
	a := e.set.Xattr.(*adapter)

	moved := e.root + "/a/f.txt"
	sibling := e.root + "/a/f.txt.bak"
	btMoved := seedAllBuckets(t, a, moved)
	btSib := seedAllBuckets(t, a, sibling)

	if err := a.RenameMetadata(moved, e.root+"/b/g.txt"); err != nil {
		t.Fatalf("RenameMetadata: %v", err)
	}

	wantTimes(t, a, sibling, btSib)
	_ = btMoved
}

// TestRenameReplacesStaleTargetRecords 目标路径上若有陈旧记录
// （replace 改名覆盖了别的文件），必须清掉而不是与迁入记录合并。
func TestRenameReplacesStaleTargetRecords(t *testing.T) {
	e := newEnv(t)
	a := e.set.Xattr.(*adapter)

	src := e.root + "/src"
	dst := e.root + "/dst"
	btSrc := seedAllBuckets(t, a, src)
	stale := time.Unix(1000000000, 0).UTC()
	if err := a.SetCreationTime(oscap.Ref{Path: dst}, stale); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if err := a.SetXattr(oscap.Ref{Path: dst}, "stale.name", []byte("x")); err != nil {
		t.Fatalf("seed stale xattr: %v", err)
	}

	if err := a.RenameMetadata(src, dst); err != nil {
		t.Fatalf("RenameMetadata: %v", err)
	}

	wantTimes(t, a, dst, btSrc) // 不是 stale
	names, _ := a.ListXattr(oscap.Ref{Path: dst})
	for _, n := range names {
		if n == "stale.name" {
			t.Fatalf("目标路径的陈旧 xattr 未被清理: %v", names)
		}
	}
}

// TestDeleteRemovesObjectAndSubtree 删除要同时清掉本对象与整棵子树。
func TestDeleteRemovesObjectAndSubtree(t *testing.T) {
	e := newEnv(t)
	a := e.set.Xattr.(*adapter)

	p := e.root + "/doomed"
	seedDirBuckets(t, a, p)
	seedAllBuckets(t, a, p+"/kid")

	if err := a.DeleteMetadata(p); err != nil {
		t.Fatalf("DeleteMetadata: %v", err)
	}
	wantMissingEverywhere(t, a, p)
	wantMissingEverywhere(t, a, p+"/kid")

	// 兄弟路径不能被误伤。
	ok := e.root + "/doomed-not"
	bt := seedAllBuckets(t, a, ok)
	wantTimes(t, a, ok, bt)

	// 删除不存在的路径是成功而非错误（幂等）——调用方刚 os.Remove 完，
	// 记录可能本来就没有。
	if err := a.DeleteMetadata(e.root + "/never-existed"); err != nil {
		t.Fatalf("删除无记录路径应幂等成功: %v", err)
	}
}

// TestMigrationReadOnlyRejected 只读视图必须拒绝迁移/清理。
func TestMigrationReadOnlyRejected(t *testing.T) {
	e := newEnv(t)
	a := e.set.Xattr.(*adapter)
	p := e.root + "/f"
	bt := seedAllBuckets(t, a, p)

	// 重开成只读视图（同一个库文件，里面已有账）。
	e.reopen(true)
	a = e.set.Xattr.(*adapter)

	if err := a.RenameMetadata(p, e.root+"/g"); err == nil {
		t.Fatal("只读视图 RenameMetadata 应被拒绝")
	}
	if err := a.DeleteMetadata(p); err == nil {
		t.Fatal("只读视图 DeleteMetadata 应被拒绝")
	}
	_ = bt
}
