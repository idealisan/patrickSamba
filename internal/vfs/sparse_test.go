package vfs

// sparse_test.go —— 稀疏文件能力（AllocatedRanges / SetSparse）测试。
//
// **测试必须对「文件系统不支持空洞探测」保持宽容**：CI 容器的临时目录
// 常落在 tmpfs / overlayfs 上，那里 SEEK_HOLE 会把整个文件报成一整段数据。
// 契约规定这种情况降级为「整个窗口已分配」，是**合法结果**而不是失败。
// 因此断言分两类：
//
//	不变量（任何文件系统都必须满足）—— 有序、不重叠、不越界、覆盖真实数据；
//	空洞特征（只在探测可用时才检查）—— 用 isSparseAware 判定后再断。

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/oscap"
	"github.com/finalappstore/stupidsamba/internal/oscap/builtin"
	"github.com/finalappstore/stupidsamba/internal/oscap/native"
)

// sparseBlk 取 1 MiB：空洞探测的粒度是文件系统块（ext4 通常 4 KiB，
// 有些是 64 KiB），取大一点保证中间那段一定能落成真正的洞。
const sparseBlk = 1 << 20

// makeSparse 造一个 [数据][洞][数据] 三段、共 3*sparseBlk 字节的文件。
func makeSparse(t *testing.T, fs *LocalFS, rel string) {
	t.Helper()
	p := filepath.Join(fs.Root(), filepath.FromSlash(rel))
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	data := bytes.Repeat([]byte{'a'}, sparseBlk)
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	// 跳过 [sparseBlk, 2*sparseBlk) 不写 —— pwrite 到更远处会让中间自动成洞。
	if _, err := f.WriteAt(bytes.Repeat([]byte{'b'}, sparseBlk), 2*sparseBlk); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

// openSparse 打开测试文件并取出 SparseFile 能力。
func openSparse(t *testing.T, fs *LocalFS, rel string, write bool) (Handle, SparseFile) {
	t.Helper()
	flags := OpenRead
	if write {
		flags |= OpenWrite
	}
	h, _, err := fs.Open(&OpenRequest{Path: rel, Flags: flags, Disposition: OpenExisting})
	if err != nil {
		t.Fatalf("Open(%s): %v", rel, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	sf, ok := h.(SparseFile)
	if !ok {
		t.Fatalf("句柄未实现 SparseFile")
	}
	return h, sf
}

// checkInvariants 校验契约里承诺的不变量。
func checkInvariants(t *testing.T, rs []Range, off, end int64) {
	t.Helper()
	prevEnd := off
	for i, r := range rs {
		if r.Length <= 0 {
			t.Errorf("区间[%d] 长度非正: %+v", i, r)
		}
		if r.Offset < off || r.Offset+r.Length > end {
			t.Errorf("区间[%d]=%+v 越出查询窗口 [%d,%d)", i, r, off, end)
		}
		if r.Offset < prevEnd && i > 0 {
			t.Errorf("区间[%d]=%+v 与前一段重叠或未按偏移升序", i, r)
		}
		prevEnd = r.Offset + r.Length
	}
}

// covers 判断 rs 是否完整覆盖 [s, e)。
func covers(rs []Range, s, e int64) bool {
	cur := s
	for _, r := range rs {
		if r.Offset > cur {
			return false
		}
		if r.Offset+r.Length > cur {
			cur = r.Offset + r.Length
		}
		if cur >= e {
			return true
		}
	}
	return cur >= e
}

// isSparseAware 判断本次结果是否真的探测到了空洞。
// 只有一段且覆盖整个窗口 —— 那就是降级结果。
func isSparseAware(rs []Range, off, end int64) bool {
	return !(len(rs) == 1 && rs[0].Offset == off && rs[0].Length == end-off)
}

func TestAllocatedRanges(t *testing.T) {
	fs := newTestFS(t, false)
	makeSparse(t, fs, "band")
	_, sf := openSparse(t, fs, "band", false)

	total := int64(3 * sparseBlk)
	rs, err := sf.AllocatedRanges(0, total)
	if err != nil {
		t.Fatalf("AllocatedRanges: %v", err)
	}
	if len(rs) == 0 {
		t.Fatal("文件明明写过数据，却报告零个已分配区间")
	}
	checkInvariants(t, rs, 0, total)

	// 真实写过的两段无论如何都必须被覆盖，否则客户端会以为数据丢了。
	if !covers(rs, 0, sparseBlk) {
		t.Errorf("首段数据 [0,%d) 未被覆盖: %+v", sparseBlk, rs)
	}
	if !covers(rs, 2*sparseBlk, total) {
		t.Errorf("末段数据 [%d,%d) 未被覆盖: %+v", 2*sparseBlk, total, rs)
	}

	if !isSparseAware(rs, 0, total) {
		t.Log("本文件系统不支持空洞探测，已降级为「整段已分配」——这是契约允许的结果")
		return
	}
	// 探测可用时中间那段必须是洞。
	if covers(rs, sparseBlk, 2*sparseBlk) {
		t.Errorf("中间应为空洞却被报成已分配: %+v", rs)
	}
}

func TestAllocatedRangesWindow(t *testing.T) {
	fs := newTestFS(t, false)
	makeSparse(t, fs, "band")
	_, sf := openSparse(t, fs, "band", false)

	// 窗口跨在第一段数据与空洞之间，结果必须被裁剪到窗口内。
	off, length := int64(sparseBlk/2), int64(sparseBlk)
	rs, err := sf.AllocatedRanges(off, length)
	if err != nil {
		t.Fatalf("AllocatedRanges: %v", err)
	}
	checkInvariants(t, rs, off, off+length)
	if !covers(rs, off, sparseBlk) {
		t.Errorf("窗口内的真实数据 [%d,%d) 未被覆盖: %+v", off, sparseBlk, rs)
	}
}

func TestAllocatedRangesClampToEOF(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "small", "hello")
	_, sf := openSparse(t, fs, "small", false)

	// 客户端常发 length = 0x7FFFFFFFFFFFFFFF，结果不能越过 EOF。
	rs, err := sf.AllocatedRanges(0, maxInt64)
	if err != nil {
		t.Fatalf("AllocatedRanges: %v", err)
	}
	checkInvariants(t, rs, 0, 5)
	if !covers(rs, 0, 5) {
		t.Errorf("整个文件都是数据却未被覆盖: %+v", rs)
	}

	// 起点在 EOF 之外：空结果 + nil error，不是错误。
	rs, err = sf.AllocatedRanges(4096, 4096)
	if err != nil || len(rs) != 0 {
		t.Errorf("EOF 之外的窗口 = %+v, %v；期望 nil, nil", rs, err)
	}

	// length == 0 同理。
	if rs, err := sf.AllocatedRanges(0, 0); err != nil || len(rs) != 0 {
		t.Errorf("length=0 时 = %+v, %v；期望 nil, nil", rs, err)
	}
}

func TestAllocatedRangesErrors(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "x")

	_, sf := openSparse(t, fs, "f", false)
	for _, c := range []struct{ off, length int64 }{
		{-1, 16},
		{0, -1},
		{maxInt64 - 1, 16}, // off+length 溢出
	} {
		if _, err := sf.AllocatedRanges(c.off, c.length); !errors.Is(err, ErrInvalidArg) {
			t.Errorf("AllocatedRanges(%d,%d) err = %v；期望 ErrInvalidArg", c.off, c.length, err)
		}
	}

	// 目录句柄。
	dh, _, err := fs.Open(&OpenRequest{Flags: OpenRead | OpenDirectory, Disposition: OpenExisting})
	if err != nil {
		t.Fatalf("打开根目录: %v", err)
	}
	defer dh.Close()
	if dsf, ok := dh.(SparseFile); ok {
		if _, err := dsf.AllocatedRanges(0, 4096); !errors.Is(err, ErrIsDir) {
			t.Errorf("目录上的 AllocatedRanges err = %v；期望 ErrIsDir", err)
		}
	}

	// 已关闭的句柄。
	h2, _, err := fs.Open(&OpenRequest{Path: "f", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	_ = h2.Close()
	if _, err := h2.(SparseFile).AllocatedRanges(0, 4096); !errors.Is(err, ErrClosed) {
		t.Errorf("已关闭句柄的 AllocatedRanges err = %v；期望 ErrClosed", err)
	}

	// OpenAttrOnly 句柄没有真实 fd。
	ah, _, err := fs.Open(&OpenRequest{Path: "f", Flags: OpenRead | OpenAttrOnly, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	defer ah.Close()
	if _, err := ah.(SparseFile).AllocatedRanges(0, 4096); !errors.Is(err, ErrNotSupported) {
		t.Errorf("AttrOnly 句柄的 AllocatedRanges err = %v；期望 ErrNotSupported", err)
	}
}

// TestAllocatedRangesAfterPunchHole 验证打洞之后区间视图会变。
// 打洞本身不被支持时（macOS、部分文件系统）跳过。
func TestAllocatedRangesAfterPunchHole(t *testing.T) {
	fs := newTestFS(t, false)
	p := filepath.Join(fs.Root(), "solid")
	if err := os.WriteFile(p, bytes.Repeat([]byte{'z'}, 3*sparseBlk), 0o644); err != nil {
		t.Fatal(err)
	}
	_, sf := openSparse(t, fs, "solid", true)

	total := int64(3 * sparseBlk)
	if err := sf.PunchHole(sparseBlk, sparseBlk); err != nil {
		if errors.Is(err, ErrNotSupported) {
			t.Skip("本文件系统不支持打洞")
		}
		t.Fatalf("PunchHole: %v", err)
	}

	after, err := sf.AllocatedRanges(0, total)
	if err != nil {
		t.Fatalf("AllocatedRanges: %v", err)
	}
	checkInvariants(t, after, 0, total)
	// 打洞不改变文件逻辑长度，两端数据必须还在。
	if !covers(after, 0, sparseBlk) || !covers(after, 2*sparseBlk, total) {
		t.Errorf("打洞误伤了两端数据: %+v", after)
	}
	if !isSparseAware(after, 0, total) {
		// 打完洞仍报「整段已分配」：本文件系统没有空洞探测能力
		// （或 fallocate 只是写了零）。契约允许，不算失败。
		t.Log("本文件系统不支持空洞探测，已降级为「整段已分配」")
		return
	}
	if covers(after, sparseBlk, 2*sparseBlk) {
		t.Errorf("打洞后中间仍被报成已分配: %+v", after)
	}
}

func TestSetSparse(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "x")
	_, sf := openSparse(t, fs, "f", true)

	// SetSparse(true)：POSIX 上是无操作，Windows 上 NTFS 真的置位。
	// 两边都必须成功 —— 报错会让 macOS 放弃创建 .sparsebundle。
	if err := sf.SetSparse(true); err != nil {
		t.Errorf("SetSparse(true) = %v；必须成功", err)
	}

	// SetSparse(false)：POSIX 上做不到，必须**如实报不支持**而不是
	// 谎称成功 —— SPARSE 属性位是由 Alloc<Size 现算的，谎称取消会让
	// 客户端查属性时照样看到 SPARSE，视图自相矛盾。
	// Windows 上 NTFS 能真的清标志位，那里应当成功。
	err := sf.SetSparse(false)
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Errorf("windows 上 SetSparse(false) = %v；NTFS 应支持", err)
		}
	} else if !errors.Is(err, ErrNotSupported) {
		t.Errorf("POSIX 上 SetSparse(false) = %v；期望 ErrNotSupported", err)
	}

	// 只读共享必须拒绝。
	ro := newTestFS(t, true)
	writeFile(t, ro, "f", "x")
	_, rsf := openSparse(t, ro, "f", false)
	if err := rsf.SetSparse(true); !errors.Is(err, ErrReadOnly) {
		t.Errorf("只读共享上的 SetSparse = %v；期望 ErrReadOnly", err)
	}
}

func TestWholeRange(t *testing.T) {
	// 降级路径的构造函数：窗口为空时不能造出长度为 0 的区间。
	if r := wholeRange(10, 10); r != nil {
		t.Errorf("wholeRange(10,10) = %+v；期望 nil", r)
	}
	if r := wholeRange(20, 10); r != nil {
		t.Errorf("wholeRange(20,10) = %+v；期望 nil", r)
	}
	got := wholeRange(4, 10)
	if len(got) != 1 || got[0] != (Range{Offset: 4, Length: 6}) {
		t.Errorf("wholeRange(4,10) = %+v；期望 [{4 6}]", got)
	}
}

// --------------------------------------------------------------------- 接线证明

// countingSparseFile 是 oscap.SparseFile 的**计数包装**：它把每个方法调用
// 记进计数器后转发给真实实现。用途只有一个 —— 证伪「vfs 真的把稀疏能力
// 委托给了 caps.Sparse()」。没有这层包装，「测试通过」只证明代码路径没崩，
// 证明不了接线成立（AGENTS.md：验收判据必须可证伪）。
type countingSparseFile struct {
	oscap.SparseFile
	punch    *int64
	prealloc *int64
	alloc    *int64
	setSpar  *int64
}

func (c countingSparseFile) PunchHole(ref oscap.Ref, off, length int64) error {
	*c.punch++
	return c.SparseFile.PunchHole(ref, off, length)
}
func (c countingSparseFile) Preallocate(ref oscap.Ref, off, length int64) error {
	*c.prealloc++
	return c.SparseFile.Preallocate(ref, off, length)
}
func (c countingSparseFile) AllocatedRanges(ref oscap.Ref, off, length int64) ([]oscap.Range, error) {
	*c.alloc++
	return c.SparseFile.AllocatedRanges(ref, off, length)
}
func (c countingSparseFile) SetSparse(ref oscap.Ref, v bool) error {
	*c.setSpar++
	return c.SparseFile.SetSparse(ref, v)
}

// countingCaps 是 oscap.Provider 的**薄包装**：除 Sparse 换成计数版外，
// 其余五个访问器、Matrix、Close 全部转发给被包裹的真实 Provider。
// 这样 vfs 的其他能力（xattr / 命名流等）仍能正常工作。
type countingCaps struct {
	oscap.Provider
	sf countingSparseFile
}

func (p countingCaps) Sparse() oscap.SparseFile { return p.sf }

// newSparseTestFS 造一个注入了计数 Provider 的共享。
func newSparseTestFS(t *testing.T) (fs *LocalFS, punch, alloc, setSpar *int64) {
	t.Helper()
	root := t.TempDir()
	real, err := oscap.Open(oscap.ModeAuto, oscap.Options{Root: root}, native.New, builtin.New)
	if err != nil {
		t.Fatalf("oscap.Open: %v", err)
	}
	var cp, ca, cs int64
	cc := countingCaps{
		Provider: real,
		sf: countingSparseFile{
			SparseFile: real.Sparse(),
			punch:      &cp,
			alloc:      &ca,
			setSpar:    &cs,
		},
	}
	fs, err = NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, Caps: cc})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs, &cp, &ca, &cs
}

// TestSparseDelegatesToCaps 证明 vfs 的 SparseFile 四个方法确实委托到了
// caps.Sparse()，而不是自己直接调平台系统调用 —— 这是 v0.3.0 把 CapSparse
// 接进 oscap 抽象层的验收核心。
func TestSparseDelegatesToCaps(t *testing.T) {
	fs, punch, alloc, setSpar := newSparseTestFS(t)
	writeFile(t, fs, "f", "hello world")
	_, sf := openSparse(t, fs, "f", true)

	// PunchHole：无论底层文件系统是否支持打洞，计数都已 +1 证明被调用。
	if err := sf.PunchHole(0, 4096); err != nil && !errors.Is(err, ErrNotSupported) {
		t.Fatalf("PunchHole: %v", err)
	}
	if *punch == 0 {
		t.Error("caps.Sparse().PunchHole 未被调用")
	}

	// AllocatedRanges：只读能力即可触发，无需 writable。
	if _, err := sf.AllocatedRanges(0, 4096); err != nil {
		t.Fatalf("AllocatedRanges: %v", err)
	}
	if *alloc == 0 {
		t.Error("caps.Sparse().AllocatedRanges 未被调用")
	}

	// SetSparse(true)：POSIX 上是无操作，必须成功且被调用。
	if err := sf.SetSparse(true); err != nil {
		t.Fatalf("SetSparse(true): %v", err)
	}
	if *setSpar == 0 {
		t.Error("caps.Sparse().SetSparse 未被调用")
	}
}
