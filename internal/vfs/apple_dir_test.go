package vfs

// apple_dir_test.go —— DirAppleMetadata（AAPL readdir_attr 的逐条目入口）。
//
// 重点是**它必须与 FileSystem.AppleInfo 给出完全相同的答案** ——
// 它只是同一件事的一条快路径，语义分叉会让同一个文件在 Finder 里
// 时而有资源派生、时而没有。

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// dirHandle 打开一个目录句柄并取出 DirAppleMetadata 能力。
func dirHandle(t *testing.T, fs *LocalFS, dir string) (Handle, DirAppleMetadata) {
	t.Helper()
	h, _, err := fs.Open(&OpenRequest{
		Path: dir, Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("打开目录 %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	dm, ok := h.(DirAppleMetadata)
	if !ok {
		t.Fatal("目录句柄应实现 DirAppleMetadata")
	}
	return h, dm
}

func TestAppleInfoAtMatchesAppleInfo(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)

	// plain 没有任何 Apple 元数据；rich 两样都有。
	writeFile(t, fs, "plain.txt", "hello")
	writeFile(t, fs, "rich.bin", "data")

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	hi, _ := openStreamH(t, fs, "rich.bin", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	if _, err := hi.WriteAt(ai.Marshal(), 0); err != nil {
		t.Fatal(err)
	}
	if err := hi.Close(); err != nil {
		t.Fatal(err)
	}
	hr, _ := openStreamH(t, fs, "rich.bin", StreamAFPResource, OpenRead|OpenWrite, OpenAlways)
	if _, err := hr.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := hr.Close(); err != nil {
		t.Fatal(err)
	}

	// 两条路径分别在「未拍快照」与「已拍快照」两种状态下都要一致 ——
	// 已拍快照时 AppleInfoAt 会用 ._ 集合跳过探测，这正是最容易出错的分支。
	for _, snapshot := range []bool{false, true} {
		h, dm := dirHandle(t, fs, "")
		if snapshot {
			for {
				if _, err := h.ReadDir("*", false, 100); err != nil {
					if err == io.EOF {
						break
					}
					t.Fatal(err)
				}
			}
		}
		for _, name := range []string{"plain.txt", "rich.bin"} {
			wantFI, wantRsrc, err := fs.AppleInfo(name)
			if err != nil {
				t.Fatalf("AppleInfo(%s): %v", name, err)
			}
			gotFI, gotRsrc, err := dm.AppleInfoAt(name)
			if err != nil {
				t.Fatalf("AppleInfoAt(%s) snapshot=%v: %v", name, snapshot, err)
			}
			if !bytes.Equal(gotFI[:], wantFI[:]) || gotRsrc != wantRsrc {
				t.Errorf("snapshot=%v %s: AppleInfoAt = (% x, %d)；AppleInfo = (% x, %d)",
					snapshot, name, gotFI, gotRsrc, wantFI, wantRsrc)
			}
		}
		// rich.bin 的两样元数据都必须真的读出来，别让「两边都错成全零」
		// 蒙混过关。
		fi, rsrc, err := dm.AppleInfoAt("rich.bin")
		if err != nil {
			t.Fatal(err)
		}
		if rsrc != 4096 {
			t.Errorf("snapshot=%v 资源派生大小 = %d；期望 4096", snapshot, rsrc)
		}
		if !bytes.Equal(fi[:], finderInfoPattern()) {
			t.Errorf("snapshot=%v FinderInfo 不对: % x", snapshot, fi)
		}
	}
}

func TestAppleInfoAtRejectsBadNames(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "sub/f", "x")
	_, dm := dirHandle(t, fs, "sub")

	// 只接受单个分量。任何能拼出目录之外路径的写法都必须被拒
	// —— 这是安全边界，不能因为调用方是自己人就放行。
	for _, bad := range []string{"..", ".", "../f", "a/b", "", "/f", `a\b`} {
		if _, _, err := dm.AppleInfoAt(bad); err == nil {
			t.Errorf("AppleInfoAt(%q) 竟然被接受", bad)
		}
	}
}

func TestAppleInfoAtWrongHandleKind(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "x")

	// 文件句柄不是目录。
	fh, _, err := fs.Open(&OpenRequest{Path: "f", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, _, err := fh.(DirAppleMetadata).AppleInfoAt("x"); !errors.Is(err, ErrNotDir) {
		t.Errorf("文件句柄上的 AppleInfoAt = %v；期望 ErrNotDir", err)
	}

	// 已关闭的目录句柄。
	dh, _, err := fs.Open(&OpenRequest{Flags: OpenRead | OpenDirectory, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	_ = dh.Close()
	if _, _, err := dh.(DirAppleMetadata).AppleInfoAt("f"); !errors.Is(err, ErrClosed) {
		t.Errorf("已关闭目录句柄上的 AppleInfoAt = %v；期望 ErrClosed", err)
	}
}

// TestDotUnderscoreSnapshotHint 直接盯住那个优化：
// 快照里没有 ._<name> 时不再去探测资源派生。
func TestDotUnderscoreSnapshotHint(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "a", "x")
	writeFile(t, fs, "b", "y")
	// 给 b 造一个真的资源派生（走正常写入路径，保证 ._b 的头部合法）。
	hr, _ := openStreamH(t, fs, "b", StreamAFPResource, OpenRead|OpenWrite, OpenAlways)
	if _, err := hr.WriteAt(make([]byte, 128), 0); err != nil {
		t.Fatal(err)
	}
	if err := hr.Close(); err != nil {
		t.Fatal(err)
	}

	h, dm := dirHandle(t, fs, "")
	names := map[string]bool{}
	for {
		batch, err := h.ReadDir("*", false, 100)
		for _, e := range batch {
			names[e.Name] = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	// ._b 本身不能作为条目出现。
	if names["._b"] {
		t.Error("._b 不应出现在枚举结果里")
	}
	if !names["a"] || !names["b"] {
		t.Fatalf("枚举结果缺条目: %v", names)
	}

	if _, rsrc, err := dm.AppleInfoAt("a"); err != nil || rsrc != 0 {
		t.Errorf("a 没有资源派生，得到 %d, %v", rsrc, err)
	}
	if _, rsrc, err := dm.AppleInfoAt("b"); err != nil || rsrc != 128 {
		t.Errorf("b 的资源派生大小 = %d, %v；期望 128", rsrc, err)
	}
}
