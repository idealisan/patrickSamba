//go:build linux

package native

// sparse_linux_test.go —— 稀疏文件的真往返，外加本平台「接线完整性」的守卫。
//
// 稀疏这一项对本项目是刚需而不是锦上添花：Time Machine 的 .sparsebundle
// 由大量 band 文件组成，备份过期回收就是往 band 里打洞。打不了洞 ⇒
// 备份卷只增不减（AGENTS.md §2 阶段二）。

import (
	"bytes"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// blk 是打洞用的块大小。取 64 KiB 而不是 4 KiB：文件系统按**簇**回收，
// 洞比簇小时内核完全可以合法地什么都不释放，那样测出来的是运气不是行为。
const blk = 64 * 1024

// kernelSeesHoleAt 用 lseek 直接问内核「off 处是不是洞」。
//
// 这是 AllocatedRanges 的**独立对照源**：判定不能用被测代码自己的结论，
// 否则实现和判据一起错的时候测试照样绿（AGENTS.md「验收判据必须可证伪」）。
func kernelSeesHoleAt(t *testing.T, path string, off int64) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	hole, err := unix.Seek(int(f.Fd()), off, unix.SEEK_HOLE)
	if err != nil {
		return false // 本文件系统不报空洞
	}
	return hole == off
}

// TestSparsePunchHoleRoundTrip 钉住打洞的**可观测语义**：
// 读回来是零、逻辑长度不变、AllocatedRanges 不再把这段报成已分配。
func TestSparsePunchHoleRoundTrip(t *testing.T) {
	set, root := newTestSet(t, false)
	if set.Sparse == nil {
		t.Fatal("Linux 上 SparseFile 必须接线（linuxSparse）")
	}
	p := mkFile(t, root, "band", 3*blk)
	ref := oscap.Ref{Path: p}

	err := set.Sparse.PunchHole(ref, blk, blk)
	if !hostSupports(t, oscap.CapSparseFile, root) {
		if !errors.Is(err, oscap.ErrNotSupported) {
			t.Fatalf("探测说本机不支持稀疏，PunchHole 应当返回 ErrNotSupported，实际 %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("PunchHole: %v", err)
	}

	// 1) 逻辑长度不变 —— 打洞不是截断。
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() != 3*blk {
		t.Fatalf("打洞后文件长度变成 %d，期望 %d（PunchHole 必须带 KEEP_SIZE）", st.Size(), 3*blk)
	}

	// 2) 洞里读回来是零，洞外的数据一个字节都不许动。
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(data[blk:2*blk], make([]byte, blk)) {
		t.Fatal("打洞区间读回来不是全零")
	}
	if bytes.Equal(data[:blk], make([]byte, blk)) {
		t.Fatal("洞**外**的数据也被清零了 —— 打洞区间算错了")
	}

	// 3) AllocatedRanges 与内核的空洞视图一致。
	ranges, err := set.Sparse.AllocatedRanges(ref, 0, 3*blk)
	if err != nil {
		t.Fatalf("AllocatedRanges: %v", err)
	}
	assertRangesSane(t, ranges, 0, 3*blk)

	if kernelSeesHoleAt(t, p, blk) {
		// 内核确认这里是洞，那么 AllocatedRanges 就不许把它报成已分配。
		for _, r := range ranges {
			if r.Offset < 2*blk && r.Offset+r.Length > blk {
				t.Fatalf("区间 %+v 与已确认的空洞 [%d,%d) 重叠", r, blk, 2*blk)
			}
		}
		// 反向对照：有数据的那两段必须**报得出来**。少了这一条，
		// 一个永远返回空列表的实现也能通过上面的循环，
		// 而少报会让客户端以为数据丢了（ports.go）。
		if len(ranges) == 0 {
			t.Fatal("两端明明有数据，AllocatedRanges 却一段都没报 —— 少报比多报危险")
		}
	} else {
		// 本文件系统不报空洞。按 ports.go 的规定必须降级成「整段已分配」，
		// 而不是报错或返回空列表。
		if len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].Length != 3*blk {
			t.Fatalf("不支持空洞探测时应当整段报已分配，实际 %+v", ranges)
		}
	}
}

// TestSparseAllocatedRangesWindow 钉住窗口裁剪与 EOF 边界。
func TestSparseAllocatedRangesWindow(t *testing.T) {
	set, root := newTestSet(t, false)
	if !hostSupports(t, oscap.CapSparseFile, root) {
		return
	}
	p := mkFile(t, root, "win", 2*blk)
	ref := oscap.Ref{Path: p}

	// 完全落在 EOF 之外：空结果是合法答案，不是错误。
	if r, err := set.Sparse.AllocatedRanges(ref, 8*blk, blk); err != nil || r != nil {
		t.Fatalf("EOF 之外的窗口 = (%+v, %v)，期望 (nil, nil)", r, err)
	}
	// 零长度窗口同理。
	if r, err := set.Sparse.AllocatedRanges(ref, 0, 0); err != nil || r != nil {
		t.Fatalf("零长度窗口 = (%+v, %v)，期望 (nil, nil)", r, err)
	}
	// 跨过 EOF 的窗口必须裁到 EOF，不许报出文件之外的区间。
	ranges, err := set.Sparse.AllocatedRanges(ref, 0, 100*blk)
	if err != nil {
		t.Fatalf("AllocatedRanges: %v", err)
	}
	assertRangesSane(t, ranges, 0, 2*blk)

	// 目录上没有「已分配区间」这回事，如实拒绝而不是降级成整段已分配。
	if _, err := set.Sparse.AllocatedRanges(oscap.Ref{Path: root}, 0, blk); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("目录上的 AllocatedRanges 应当返回 ErrInvalidArg，实际 %v", err)
	}
}

// TestSparsePreallocateKeepsSize 钉住 Preallocate 不改变逻辑长度。
//
// 它对应 SMB 的 AllocationSize / AlSi create context：预留空间但 EOF 不动。
// 改了 EOF 的话客户端会看到一个凭空变大的文件。
func TestSparsePreallocateKeepsSize(t *testing.T) {
	set, root := newTestSet(t, false)
	p := mkFile(t, root, "prealloc", blk)
	ref := oscap.Ref{Path: p}

	err := set.Sparse.Preallocate(ref, 0, 8*blk)
	if err != nil && !errors.Is(err, oscap.ErrNotSupported) {
		t.Fatalf("Preallocate: %v", err)
	}
	st, serr := os.Stat(p)
	if serr != nil {
		t.Fatalf("Stat: %v", serr)
	}
	if st.Size() != blk {
		t.Fatalf("Preallocate 之后长度变成 %d，期望 %d（必须带 KEEP_SIZE）", st.Size(), blk)
	}
}

// TestSparseSetSparseAsymmetry 钉住 POSIX 上**刻意的不对称**（ports.go）。
//
//	true  → nil            文件天然可稀疏
//	false → ErrNotSupported 做不到，且不能假装做到
//
// 为什么 false 不能谎称成功：SPARSE 属性位是由 Alloc < Size 现算的，
// 假装取消之后客户端回头查属性照样看到 SPARSE 位，得到自相矛盾的视图。
func TestSparseSetSparseAsymmetry(t *testing.T) {
	set, root := newTestSet(t, false)
	ref := oscap.Ref{Path: mkFile(t, root, "flag", 16)}

	if err := set.Sparse.SetSparse(ref, true); err != nil {
		t.Fatalf("SetSparse(true) 应当返回 nil，实际 %v", err)
	}
	if err := set.Sparse.SetSparse(ref, false); !errors.Is(err, oscap.ErrNotSupported) {
		t.Fatalf("SetSparse(false) 应当返回 ErrNotSupported（不许假装成功），实际 %v", err)
	}
}

// TestSparseRejectsBadWindow 确认溢出校验挂在真实调用路径上。
//
// checkWindow 自身的单测在 native_test.go；这里验的是「三个方法都真的调了它」——
// 漏接一处就是一次越界写（AGENTS.md §8）。
func TestSparseRejectsBadWindow(t *testing.T) {
	set, root := newTestSet(t, false)
	ref := oscap.Ref{Path: mkFile(t, root, "bad", 16)}

	for _, c := range []struct {
		name        string
		off, length int64
	}{
		{"负偏移", -1, blk},
		{"负长度", 0, -1},
		{"相加溢出", math.MaxInt64 - 10, 11},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := set.Sparse.PunchHole(ref, c.off, c.length); !errors.Is(err, oscap.ErrInvalidArg) {
				t.Errorf("PunchHole 应当返回 ErrInvalidArg，实际 %v", err)
			}
			if err := set.Sparse.Preallocate(ref, c.off, c.length); !errors.Is(err, oscap.ErrInvalidArg) {
				t.Errorf("Preallocate 应当返回 ErrInvalidArg，实际 %v", err)
			}
			if _, err := set.Sparse.AllocatedRanges(ref, c.off, c.length); !errors.Is(err, oscap.ErrInvalidArg) {
				t.Errorf("AllocatedRanges 应当返回 ErrInvalidArg，实际 %v", err)
			}
		})
	}
}

// TestSparseReadOnlyShareRejectsWrites 钉住只读共享上的写类拒绝。
func TestSparseReadOnlyShareRejectsWrites(t *testing.T) {
	_, root := newTestSet(t, false)
	ro, err := New(oscap.Options{Root: root, ReadOnly: true})
	if err != nil {
		t.Fatalf("New(readOnly): %v", err)
	}
	ref := oscap.Ref{Path: mkFile(t, root, "ro", blk)}

	if err := ro.Sparse.PunchHole(ref, 0, blk); !errors.Is(err, oscap.ErrReadOnly) {
		t.Fatalf("只读共享上 PunchHole 应当返回 ErrReadOnly，实际 %v", err)
	}
	if err := ro.Sparse.Preallocate(ref, 0, blk); !errors.Is(err, oscap.ErrReadOnly) {
		t.Fatalf("只读共享上 Preallocate 应当返回 ErrReadOnly，实际 %v", err)
	}
	// 读类操作在只读共享上必须照常可用 —— 这也是反向对照：
	// 一个把整套实现都返回 ErrReadOnly 的写法会在这里露馅。
	if _, err := ro.Sparse.AllocatedRanges(ref, 0, blk); err != nil {
		t.Fatalf("只读共享上 AllocatedRanges 应当照常可用，实际 %v", err)
	}
}

// assertRangesSane 校验 ports.go 对 AllocatedRanges 的返回值保证：
// 升序、互不重叠、裁到窗口内、长度为正。
//
// 这些不变量与文件系统无关，**在任何宿主上都必须成立**，
// 所以它们是本文件里唯一无条件执行的断言。
func assertRangesSane(t *testing.T, ranges []oscap.Range, off, end int64) {
	t.Helper()
	var prevEnd int64 = -1
	for i, r := range ranges {
		if r.Length <= 0 {
			t.Errorf("区间 #%d %+v 长度非正", i, r)
		}
		if r.Offset < off || r.Offset+r.Length > end {
			t.Errorf("区间 #%d %+v 越出查询窗口 [%d,%d)", i, r, off, end)
		}
		if r.Offset < prevEnd {
			t.Errorf("区间 #%d %+v 与前一段重叠或乱序（前一段结束于 %d）", i, r, prevEnd)
		}
		prevEnd = r.Offset + r.Length
	}
}

// TestNativeWiringIsNotVacuous 是本包的**反空转守卫**。
//
// 上面所有用例在「本机不支持这一项」时都会走反向断言分支。那是对的，
// 但也意味着：万一哪天 newSet 被改坏成一个空 Set，那些用例仍旧全绿 ——
// 因为它们验的是「做不到时如实说」，而一个什么都不做的实现恰好满足。
//
// 所以这里单独钉死 **Linux 上的接线事实**（与文件系统无关，只与代码有关）：
// 五项该有的必须有，DOS 属性那一项必须没有。
// 它同时也是 native_linux.go 那张对照表的可执行版本 —— 表和代码分家时，
// 分家的那一刻这个用例就红。
func TestNativeWiringIsNotVacuous(t *testing.T) {
	set, err := New(oscap.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, c := range []struct {
		cap  oscap.Capability
		have bool
		why  string
	}{
		{oscap.CapXattr, set.Xattr != nil, "posixXattr"},
		{oscap.CapSparseFile, set.Sparse != nil, "linuxSparse（fallocate PUNCH_HOLE）"},
		{oscap.CapNamedStream, set.Streams != nil, "posixStreams（承载在 xattr 上）"},
		{oscap.CapStableFileID, set.IDs != nil, "posixIDs（st_ino）"},
		{oscap.CapCreationTime, set.Times != nil, "linuxTimes（statx BTIME）"},
	} {
		if !c.have {
			t.Errorf("Linux 上 %s 应当由 native 提供（%s），实际是 nil", c.cap, c.why)
		}
	}
	if set.DOS != nil {
		t.Error("Linux 上 DOSAttributes 应当留 nil 交给 builtin：" +
			"内核对 DOS 属性位一无所知，用 user.DOSATTRIB 存一份那是旁路存储、属于 builtin")
	}
}

// TestSetCreationTimeIsHonestlyUnsupportedOnLinux 钉住 Linux 上
// 「读得到、写不进」这个半边能力的**诚实性**。
//
// 内核根本没有设置 btime 的接口。假装成功会让客户端写完立刻读回一个对不上
// 的值，且全程无错 —— 正是本项目反复栽过的「成功回显 ≠ 事情真的发生了」。
func TestSetCreationTimeIsHonestlyUnsupportedOnLinux(t *testing.T) {
	set, root := newTestSet(t, false)
	p := mkFile(t, root, "settime", 4)
	ref := oscap.Ref{Path: p}

	// 一个不可能与真实创建时间重合的值，用来判断「有没有被写进去」。
	marker := time.Date(1994, 3, 22, 1, 2, 3, 0, time.UTC)

	if err := set.Times.SetCreationTime(ref, marker); !errors.Is(err, oscap.ErrNotSupported) {
		t.Fatalf("Linux 上 SetCreationTime 应当返回 ErrNotSupported，实际 %v", err)
	}
	// 反向对照：确认它是**真的没写**，而不是写了之后再报错。
	if hostSupports(t, oscap.CapCreationTime, root) {
		got, err := set.Times.CreationTime(ref)
		if err != nil {
			t.Fatalf("CreationTime: %v", err)
		}
		if got.Equal(marker) {
			t.Fatal("SetCreationTime 报了 ErrNotSupported，创建时间却真的变成了那个值")
		}
	}
}
