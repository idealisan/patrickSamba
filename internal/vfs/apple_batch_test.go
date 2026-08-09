package vfs

// apple_batch_test.go —— DirAppleMetadata.AppleInfoAtBatch。
//
// 批量版本唯一的实现差异是**整批复用同一个 402 字节读缓冲**，
// 所以这里的重点不是「能不能读出来」，而是「上一条的元数据会不会
// 渗到下一条」—— 缓冲复用最典型的 bug 就是短读之后残留旧字节，
// 让一个本来没有 FinderInfo 的 band 文件继承前一个文件的图标。

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestAppleInfoAtBatchMatchesSingle 钉住「批量与逐条完全一致」这条契约。
func TestAppleInfoAtBatchMatchesSingle(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)

	// 交错排列：有元数据的 rich 夹在两个干净文件中间，
	// 一旦缓冲被复用出 bug，plain2 会读到 rich 的 FinderInfo。
	writeFile(t, fs, "plain1", "a")
	writeFile(t, fs, "rich", "b")
	writeFile(t, fs, "plain2", "c")

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	hi, _ := openStreamH(t, fs, "rich", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	if _, err := hi.WriteAt(ai.Marshal(), 0); err != nil {
		t.Fatal(err)
	}
	if err := hi.Close(); err != nil {
		t.Fatal(err)
	}
	hr, _ := openStreamH(t, fs, "rich", StreamAFPResource, OpenRead|OpenWrite, OpenAlways)
	if _, err := hr.WriteAt(make([]byte, 512), 0); err != nil {
		t.Fatal(err)
	}
	if err := hr.Close(); err != nil {
		t.Fatal(err)
	}

	names := []string{"plain1", "rich", "plain2", "nonexistent"}

	// 未拍快照 / 已拍快照两种状态都要一致：后者走的是 dotUnder 命中路径。
	for _, snapshot := range []bool{false, true} {
		h, dm := dirHandle(t, fs, "")
		if snapshot {
			drainDir(t, h)
		}

		got, err := dm.AppleInfoAtBatch(names)
		if err != nil {
			t.Fatalf("snapshot=%v AppleInfoAtBatch: %v", snapshot, err)
		}
		if len(got) != len(names) {
			t.Fatalf("结果长度 = %d；期望 %d（必须与 names 一一对应）", len(got), len(names))
		}
		for i, name := range names {
			wantFI, wantRsrc, wantErr := dm.AppleInfoAt(name)
			if !errors.Is(got[i].Err, wantErr) {
				t.Errorf("snapshot=%v %s: batch err = %v；逐条 err = %v",
					snapshot, name, got[i].Err, wantErr)
			}
			if !bytes.Equal(got[i].FinderInfo[:], wantFI[:]) {
				t.Errorf("snapshot=%v %s: batch FinderInfo = % x；逐条 = % x",
					snapshot, name, got[i].FinderInfo, wantFI)
			}
			if got[i].RsrcSize != wantRsrc {
				t.Errorf("snapshot=%v %s: batch rsrc = %d；逐条 = %d",
					snapshot, name, got[i].RsrcSize, wantRsrc)
			}
		}

		// 绝对值也要对，别让「两边一起错成全零」蒙混过关。
		if !bytes.Equal(got[1].FinderInfo[:], finderInfoPattern()) {
			t.Errorf("snapshot=%v rich 的 FinderInfo = % x", snapshot, got[1].FinderInfo)
		}
		if got[1].RsrcSize != 512 {
			t.Errorf("snapshot=%v rich 的资源派生 = %d；期望 512", snapshot, got[1].RsrcSize)
		}
		// 复用缓冲的核心断言：rich 之后的干净文件必须是全零。
		var zero [FinderInfoSize]byte
		for _, i := range []int{2, 3} {
			if !bytes.Equal(got[i].FinderInfo[:], zero[:]) {
				t.Errorf("snapshot=%v %s 继承了上一条的 FinderInfo: % x",
					snapshot, names[i], got[i].FinderInfo)
			}
		}
	}
}

// TestAppleInfoAtBatchBadNamesArePerEntry：一条非法名字不能毁掉整页。
func TestAppleInfoAtBatchBadNamesArePerEntry(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "sub/ok", "x")
	_, dm := dirHandle(t, fs, "sub")

	names := []string{"ok", "..", "a/b", "", ".", `a\b`, "ok"}
	got, err := dm.AppleInfoAtBatch(names)
	if err != nil {
		t.Fatalf("整批不应失败: %v", err)
	}
	if len(got) != len(names) {
		t.Fatalf("结果长度 = %d；期望 %d", len(got), len(names))
	}
	for i, name := range names {
		if name == "ok" {
			if got[i].Err != nil {
				t.Errorf("合法条目 %d 报错 %v", i, got[i].Err)
			}
			continue
		}
		if got[i].Err == nil {
			t.Errorf("非法名字 %q 竟然被接受", name)
		}
	}
}

func TestAppleInfoAtBatchWrongHandleKind(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "x")

	fh, _, err := fs.Open(&OpenRequest{Path: "f", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.(DirAppleMetadata).AppleInfoAtBatch([]string{"x"}); !errors.Is(err, ErrNotDir) {
		t.Errorf("文件句柄上的 AppleInfoAtBatch = %v；期望 ErrNotDir", err)
	}

	dh, _, err := fs.Open(&OpenRequest{Flags: OpenRead | OpenDirectory, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	_ = dh.Close()
	if _, err := dh.(DirAppleMetadata).AppleInfoAtBatch([]string{"f"}); !errors.Is(err, ErrClosed) {
		t.Errorf("已关闭目录句柄上的 AppleInfoAtBatch = %v；期望 ErrClosed", err)
	}
}

// TestAppleInfoAtBatchEmpty：空输入返回空结果而不是 nil 崩溃。
func TestAppleInfoAtBatchEmpty(t *testing.T) {
	fs := newTestFS(t, false)
	_, dm := dirHandle(t, fs, "")
	got, err := dm.AppleInfoAtBatch(nil)
	if err != nil || len(got) != 0 {
		t.Errorf("空批 = (%v, %v)；期望 (空, nil)", got, err)
	}
}

// BenchmarkAppleInfoBatch 量一下批量接口相对逐条调用到底省了多少。
//
// 两个子基准在**同一次运行**里对比，因为本容器是 8 个 agent 共用的，
// 跨进程比较的绝对 ns 完全不可信，只有同一进程内的相对值和
// allocs/op 有意义。
//
// 跑法：
//
//	go test -run XXX -bench BenchmarkAppleInfoBatch -benchtime 20x \
//	    -benchmem ./internal/vfs/
func BenchmarkAppleInfoBatch(b *testing.B) {
	const n = 2000
	root := buildBandDir(b, n)
	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = fs.Close() }()

	h, _, err := fs.Open(&OpenRequest{
		Path: "bands", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	dm := h.(DirAppleMetadata)

	// 枚举一遍拿到名字，顺带让句柄进入已拍快照状态（真实 readdir_attr
	// 就是这个状态）。
	var names []string
	for {
		batch, err := h.ReadDir("*", false, 100)
		for _, e := range batch {
			if e.Name != "." && e.Name != ".." {
				names = append(names, e.Name)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			b.Fatal(err)
		}
	}

	b.Run("Single", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, name := range names {
				if _, _, err := dm.AppleInfoAt(name); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("Batch", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := dm.AppleInfoAtBatch(names); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// drainDir 把目录枚举到底，让句柄进入「已拍快照」状态。
func drainDir(t *testing.T, h Handle) {
	t.Helper()
	for {
		_, err := h.ReadDir("*", false, 100)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
