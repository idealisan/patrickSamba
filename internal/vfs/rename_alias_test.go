package vfs

// rename_alias_test.go —— Rename 的「目标其实就是源」判定。
//
// 背景（win-vfs 从 Windows 视角报的）：宿主文件系统对名字做等价折叠时
// （NTFS/APFS 大小写不敏感，APFS 还认 Unicode 规范化等价），
// os.Lstat(dst) 会命中 src 自己。此时 replace=true 会先 os.Remove(dst) ——
// 删掉的正是源文件，数据当场丢失。
//
// ⚠️ 诚实说明：**端到端的那条路径在本容器里验证不了。** 它需要一个
// 大小写不敏感的宿主文件系统，而 ext4 的 casefold 要 mkfs 时开特性再挂载，
// 本容器在非初始 user namespace 里根本挂不了任何文件系统。
// 所以这里只对判定函数 sameDirEntry 做单元测试，端到端那一侧
// 要靠 win-vfs 在真 Windows 上验。别把这里的绿灯当成「Windows 上没问题」。

import (
	"os"
	"path/filepath"
	"testing"
)

// mustTouchFile 建一个空文件。
//
// 刻意不叫 touch：另一个分支（tm-vfs/path-exact-first）的 path_case_test.go
// 里已经有一个同名 helper，两边合入同一个包会撞名。
func mustTouchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("建 %s: %v", path, err)
	}
}

// TestSameDirEntryDetectsAlias 复现折叠命中的效果：
// 在大小写不敏感的宿主上，Lstat("A.TXT") 返回的就是 "a.txt" 的 FileInfo。
// 这里直接把 src 自己的 FileInfo 喂进去，等价于那个场景。
func TestSameDirEntryDetectsAlias(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	mustTouchFile(t, src)

	fi, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if !sameDirEntry(dir, dir, src, fi) {
		t.Error("没认出 dst 就是 src 自己 —— replace=true 会把源文件删掉")
	}
}

// TestSameDirEntryDistinctObjects 是**反向对照**：真正不同的两个对象
// 必须判 false，否则 replace=true 就不会覆盖目标了（把功能改坏）。
func TestSameDirEntryDistinctObjects(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	mustTouchFile(t, src)
	mustTouchFile(t, dst)

	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if sameDirEntry(dir, dir, src, fi) {
		t.Error("两个不同的文件被判成了同一个对象")
	}
}

// TestSameDirEntryCrossDirKeepsOldBehavior：跨目录的硬链接必须判 false。
//
// os.SameFile 对硬链接也返回 true，但不同目录下的两个名字**不可能**是
// 同一个目录项，删掉一个不会丢数据 —— 旧的「先删后改名」对它是正确的，
// 不能因为这次修复而改掉。
func TestSameDirEntryCrossDirKeepsOldBehavior(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(dirA, "f")
	dst := filepath.Join(dirB, "f")
	mustTouchFile(t, src)
	if err := os.Link(src, dst); err != nil {
		t.Skipf("宿主不支持硬链接: %v", err)
	}

	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if sameDirEntry(dirA, dirB, src, fi) {
		t.Error("跨目录硬链接被判成同一目录项，会改掉既有的覆盖语义")
	}
}

// TestRenameReplaceStillOverwrites 是端到端的**反向对照**：
// 普通的「目标已存在 + replace=true」必须照旧覆盖。
func TestRenameReplaceStillOverwrites(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "src.txt", "NEW")
	writeFile(t, fs, "dst.txt", "OLD")

	if err := fs.Rename("src.txt", "dst.txt", true); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(fs.Root(), "dst.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW" {
		t.Errorf("dst 内容 = %q, want %q", got, "NEW")
	}
	if _, err := os.Lstat(filepath.Join(fs.Root(), "src.txt")); !os.IsNotExist(err) {
		t.Error("src 还在，rename 没真正搬动")
	}
}

// TestRenameNoReplaceStillFails 是另一条反向对照：replace=false 仍要报 ErrExist。
func TestRenameNoReplaceStillFails(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "src.txt", "NEW")
	writeFile(t, fs, "dst.txt", "OLD")

	if err := fs.Rename("src.txt", "dst.txt", false); err != ErrExist {
		t.Errorf("err = %v, want ErrExist", err)
	}
}
