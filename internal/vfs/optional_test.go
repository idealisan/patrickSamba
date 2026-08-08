package vfs

// optional_test.go —— 可选能力（稀疏文件、扩展属性）的行为测试。
//
// 这些能力依赖宿主文件系统，能力缺失时实现会返回 ErrNotSupported，
// 用例统一 t.Skip 而不是 Fail —— 跨平台跑同一份测试的前提。

import (
	"bytes"
	"errors"
	"testing"
)

func openRW(t *testing.T, fs *LocalFS, rel string) Handle {
	t.Helper()
	h, _, err := fs.Open(&OpenRequest{
		Path: rel, Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	})
	if err != nil {
		t.Fatalf("打开 %q: %v", rel, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestPunchHole 覆盖 Time Machine .sparsebundle 回收 band 的核心动作：
// 把中间一段打成空洞后，逻辑长度不变、读回来全零。
func TestPunchHole(t *testing.T) {
	fs := newTestFS(t, false)
	h := openRW(t, fs, "band")

	const size = 128 * 1024
	data := bytes.Repeat([]byte{0xAB}, size)
	if _, err := h.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}

	sp, ok := h.(SparseFile)
	if !ok {
		t.Fatal("localHandle 应实现 SparseFile")
	}

	const holeOff, holeLen = 32 * 1024, 32 * 1024
	if err := sp.PunchHole(holeOff, holeLen); err != nil {
		if errors.Is(err, ErrNotSupported) {
			t.Skipf("宿主文件系统不支持打洞: %v", err)
		}
		t.Fatalf("PunchHole: %v", err)
	}

	a, err := h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if a.Size != size {
		t.Errorf("打洞不应改变逻辑长度，Size = %d, want %d", a.Size, size)
	}

	got := make([]byte, size)
	if _, err := h.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[holeOff:holeOff+holeLen], make([]byte, holeLen)) {
		t.Error("洞里应当读回全零")
	}
	// 洞两侧的数据必须完好
	if !bytes.Equal(got[:holeOff], data[:holeOff]) {
		t.Error("洞之前的数据被破坏")
	}
	if !bytes.Equal(got[holeOff+holeLen:], data[holeOff+holeLen:]) {
		t.Error("洞之后的数据被破坏")
	}
}

func TestPunchHoleBounds(t *testing.T) {
	fs := newTestFS(t, false)
	h := openRW(t, fs, "b")
	sp := h.(SparseFile)

	// 负数与溢出必须在碰到 syscall 之前就被拦下
	for _, c := range []struct{ off, length int64 }{
		{-1, 16},
		{0, -1},
		{maxInt64, 1},
		{maxInt64 - 1, 8},
	} {
		if err := sp.PunchHole(c.off, c.length); !errors.Is(err, ErrInvalidArg) {
			t.Errorf("PunchHole(%d,%d) = %v, want ErrInvalidArg", c.off, c.length, err)
		}
		if err := sp.Preallocate(c.off, c.length); !errors.Is(err, ErrInvalidArg) {
			t.Errorf("Preallocate(%d,%d) = %v, want ErrInvalidArg", c.off, c.length, err)
		}
	}
	// 零长度是 no-op，不该报错
	if err := sp.PunchHole(0, 0); err != nil {
		t.Errorf("零长度打洞应是 no-op: %v", err)
	}
	if err := sp.Preallocate(0, 0); err != nil {
		t.Errorf("零长度预分配应是 no-op: %v", err)
	}
}

// TestPreallocate 覆盖 AllocationSize / AlSi create context 的落地：
// 预留空间但**不改变**文件逻辑长度。
func TestPreallocate(t *testing.T) {
	fs := newTestFS(t, false)
	h := openRW(t, fs, "prealloc")

	if _, err := h.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	sp := h.(SparseFile)
	if err := sp.Preallocate(0, 1<<20); err != nil {
		if errors.Is(err, ErrNotSupported) {
			t.Skipf("宿主文件系统不支持预分配: %v", err)
		}
		t.Fatalf("Preallocate: %v", err)
	}
	a, err := h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if a.Size != 1 {
		t.Errorf("预分配不应改变逻辑长度，Size = %d, want 1", a.Size)
	}
}

// TestSparseReadOnly：只读共享上不能打洞/预分配。
func TestSparseReadOnly(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "hello")

	roFS, err := NewLocalFS(LocalConfig{Root: fs.Root(), ReadOnly: true, CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = roFS.Close() }()

	h, _, err := roFS.Open(&OpenRequest{Path: "f", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	sp := h.(SparseFile)
	if err := sp.PunchHole(0, 1); err == nil {
		t.Error("只读共享上打洞应被拒绝")
	}
	if err := sp.Preallocate(0, 1); err == nil {
		t.Error("只读共享上预分配应被拒绝")
	}
	if d, ok := h.(DeleteOnCloser); ok {
		if err := d.SetDeleteOnClose(true); !errors.Is(err, ErrReadOnly) {
			t.Errorf("只读共享上标记删除应返回 ErrReadOnly，得到 %v", err)
		}
	}
}

// TestSetDeleteOnCloseAfterOpen 覆盖客户端删文件的标准流程：
// CREATE → SET_INFO(FileDispositionInformation) → CLOSE。
func TestSetDeleteOnCloseAfterOpen(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "doomed.txt", "bye")

	h, _, err := fs.Open(&OpenRequest{
		Path: "doomed.txt", Flags: OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := h.(DeleteOnCloser)
	if !ok {
		t.Fatal("localHandle 应实现 DeleteOnCloser")
	}
	if err := d.SetDeleteOnClose(true); err != nil {
		t.Fatalf("SetDeleteOnClose: %v", err)
	}
	// 撤销再重新标记，模拟客户端反悔
	if err := d.SetDeleteOnClose(false); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDeleteOnClose(true); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := fs.Stat("doomed.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("关闭后文件应已删除，Stat = %v", err)
	}
	// 关闭后再操作必须是 ErrClosed，不能 panic
	if err := d.SetDeleteOnClose(true); !errors.Is(err, ErrClosed) {
		t.Errorf("已关闭句柄应返回 ErrClosed，得到 %v", err)
	}
}

// TestXattrRoundTrip 为阶段二的 AFP_AfpInfo / AFP_Resource 铺路：
// 先确认扩展属性通道本身是通的。
func TestXattrRoundTrip(t *testing.T) {
	fs := newTestFS(t, false)
	h := openRW(t, fs, "x.txt")

	x, err := h.Xattr()
	if err != nil {
		if errors.Is(err, ErrNotSupported) {
			t.Skipf("本平台不支持扩展属性: %v", err)
		}
		t.Fatal(err)
	}

	const name = "com.apple.FinderInfo"
	want := bytes.Repeat([]byte{0x5A}, 32)
	if err := x.Set(name, want); err != nil {
		if errors.Is(err, ErrNotSupported) || errors.Is(err, ErrPermission) {
			t.Skipf("宿主文件系统不支持扩展属性: %v", err)
		}
		t.Fatalf("Set: %v", err)
	}

	got, err := x.Get(name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get = % x, want % x", got, want)
	}

	names, err := x.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !contains(names, name) {
		t.Errorf("List = %v, 应含 %q", names, name)
	}

	// 覆盖写：Apple 会反复改写 FinderInfo
	want2 := bytes.Repeat([]byte{0x11}, 32)
	if err := x.Set(name, want2); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.Get(name); !bytes.Equal(got, want2) {
		t.Errorf("覆盖写后 Get = % x, want % x", got, want2)
	}

	if err := x.Remove(name); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := x.Get(name); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后 Get 应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := x.Get("com.apple.nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的属性应返回 ErrNotFound，得到 %v", err)
	}
}
