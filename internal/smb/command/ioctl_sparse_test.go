package command

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
	"github.com/idealisan/patrickSamba/internal/vfs"
)

// 稀疏文件三个 FSCTL 的 handler 测试。
//
// 这条路径是 Time Machine 能不能回收 .sparsebundle band 的关键（AGENTS.md §2
// 阶段二），且 mount.cifs 在本容器跑不通（§10.3），所以必须靠单元测试兜住。
//
// 这里用真实的 LocalFS 而不是桩：打洞是不是真的把盘上空间还回去了，
// 只有真文件系统能验证；用假 VFS 测出来的"通过"没有意义。

// TestIoctlSetSparseEmptyInputMeansTrue 验证 MS-FSCC §2.3.69 的规定：
// FSCTL_SET_SPARSE 的输入是**可选**的，缺省时视为 SetSparse=TRUE。
//
// 这是个容易写错的地方：把空输入当成 STATUS_INVALID_PARAMETER 会让 macOS
// 建 .sparsebundle 时第一步就失败。
func TestIoctlSetSparseEmptyInputMeansTrue(t *testing.T) {
	ctx, req := newSparseCtx(t, 0)

	req.CtlCode = wire.FSCTLSetSparse
	req.Input = nil
	if err := ioctlSetSparse(ctx, req); err != nil {
		t.Fatalf("空输入的 SET_SPARSE 应成功，实际 err=%v", err)
	}

	// 显式 TRUE 同样成功。
	ctx.Out = ctx.Out[:0]
	req.Input = []byte{1}
	if err := ioctlSetSparse(ctx, req); err != nil {
		t.Fatalf("SetSparse=TRUE 应成功，实际 err=%v", err)
	}

	// 显式 FALSE：后端**可以**做到，也**可以**做不到，但不许谎报。
	//
	//   POSIX 后端做不到「取消稀疏」，如实回 STATUS_NOT_SUPPORTED
	//   （理由见 ioctlSetSparse 的注释：SPARSE 位由 Alloc < Size 现算，
	//   谎称取消会让客户端回头查属性时照样看到 SPARSE，而真取消得把洞全填零，
	//   Time Machine band 会当场从几 KiB 涨到 8 MiB）；
	//   NTFS 上 FSCTL_SET_SPARSE 的清标志是真实生效的，可以回成功。
	//
	// 所以判据不是「必须失败」，而是二选一、且各自可证伪：
	// 要么诚实拒绝（NOT_SUPPORTED），要么报成功且属性位**确实没了**。
	// 写成「必须 NOT_SUPPORTED」在 NTFS 上会把正确实现判成缺陷。
	//
	// 与 Samba 的分歧（它存 user.DOSATTRIB 的 bit，所以 FALSE 总能成功）也记在
	// ioctlSetSparse 的注释里。没有已知客户端会发 FALSE。
	ctx.Out = ctx.Out[:0]
	req.Input = []byte{0}
	switch err := ioctlSetSparse(ctx, req); {
	case err == status.NotSupported:
		// 诚实拒绝，符合 POSIX 侧契约。
	case err != nil:
		t.Fatalf("SetSparse=FALSE err = %v，只允许 nil（真做到）或 %v（如实拒绝）",
			err, status.NotSupported)
	default:
		// 说成功就得真做到：SPARSE 属性位必须已经消失，否则就是谎报。
		a, statErr := ctx.Chain.LastOpen.Handle.Stat()
		if statErr != nil {
			t.Fatalf("SetSparse(FALSE) 后查属性失败: %v", statErr)
		}
		if a.FileAttributes&vfs.FileAttributeSparse != 0 {
			t.Errorf("SetSparse(FALSE) 报成功，但属性里仍是 SPARSE（位图 %#x）—— 谎报",
				a.FileAttributes)
		}
	}
}

// TestIoctlSetSparseRejectsDirectory：目录不可能是稀疏文件
// （MS-FSA §2.1.5.9.29 → STATUS_INVALID_PARAMETER）。
func TestIoctlSetSparseRejectsDirectory(t *testing.T) {
	ctx, req := newSparseCtx(t, 0)
	ctx.Chain.LastOpen.IsDir = true
	req.CtlCode = wire.FSCTLSetSparse

	if err := ioctlSetSparse(ctx, req); err != status.InvalidParameter {
		t.Errorf("目录上的 SET_SPARSE err = %v, 期望 %v", err, status.InvalidParameter)
	}
}

// TestIoctlSetZeroDataPunchesHole 验证 FSCTL_SET_ZERO_DATA 真的打了洞：
// 区间读回来是零，且 AllocationSize 小于文件逻辑长度。
//
// 这正是 Time Machine 回收 band 的实质动作 —— 只把内容改成零而不释放空间
// 的实现能通过"读回来是零"的检查，但备份卷永远不会缩小，所以这里必须
// 同时断言分配长度。
func TestIoctlSetZeroDataPunchesHole(t *testing.T) {
	const size = 1 << 20 // 1 MiB，远大于任何文件系统的块大小
	ctx, req := newSparseCtx(t, size)
	open := ctx.Chain.LastOpen

	before, err := open.Handle.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if before.Alloc == 0 {
		t.Skip("宿主文件系统不上报分配长度，无法验证空间是否释放")
	}

	req.CtlCode = wire.FSCTLSetZeroData
	// FILE_ZERO_DATA_INFORMATION{ FileOffset, BeyondFinalZero }，小端。
	req.Input = encodeZeroData(0, size)
	if err := ioctlSetZeroData(ctx, req); err != nil {
		t.Fatalf("SET_ZERO_DATA err=%v", err)
	}

	buf := make([]byte, 4096)
	if _, err := open.Handle.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("打洞后 offset %d 读到 %#x，期望 0", i, b)
		}
	}

	after, err := open.Handle.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if after.Size != size {
		t.Errorf("打洞后逻辑长度 = %d, 期望不变（%d）", after.Size, size)
	}
	if after.Alloc >= before.Alloc {
		t.Errorf("打洞后分配长度 = %d, 期望小于打洞前的 %d（空间没有真的释放）",
			after.Alloc, before.Alloc)
	}
}

// TestIoctlQueryAllocatedRanges 验证打洞后的区间查询：
// 文件前半被打洞、后半有数据时，只应报告后半。
func TestIoctlQueryAllocatedRanges(t *testing.T) {
	const size = 1 << 20
	ctx, req := newSparseCtx(t, size)
	open := ctx.Chain.LastOpen

	// 前半打洞，后半保持有数据。
	sp := open.Handle.(vfs.SparseFile)
	if err := sp.PunchHole(0, size/2); err != nil {
		t.Fatalf("PunchHole: %v", err)
	}

	req.CtlCode = wire.FSCTLQueryAllocatedRanges
	req.MaxOutputResponse = 4096
	req.Input = encodeAllocatedRangeInput(0, size)
	if err := ioctlQueryAllocatedRanges(ctx, req); err != nil {
		t.Fatalf("QUERY_ALLOCATED_RANGES err=%v", err)
	}
	if ctx.Status != 0 {
		t.Errorf("ctx.Status = %v, 期望 STATUS_SUCCESS", ctx.Status)
	}

	ranges := parseRangesFromIoctlOut(t, ctx.Out)
	if len(ranges) == 0 {
		t.Fatal("没有报告任何已分配区间，但文件后半明明有数据")
	}
	// 不假定块大小，只断言：报告的区间都落在查询窗口内，且覆盖到了后半段。
	var covered bool
	for _, r := range ranges {
		if r.FileOffset < 0 || r.Length <= 0 || r.FileOffset+r.Length > size {
			t.Errorf("区间 [%d,+%d) 越出查询窗口 [0,%d)", r.FileOffset, r.Length, size)
		}
		if r.FileOffset+r.Length > size/2 {
			covered = true
		}
	}
	if !covered {
		t.Errorf("已分配区间 %v 未覆盖文件后半段", ranges)
	}
}

// TestIoctlQueryAllocatedRangesRejectsBadInput：负偏移/负长度/相加溢出
// 必须挡在门外（AGENTS.md §8）。
func TestIoctlQueryAllocatedRangesRejectsBadInput(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)
	cases := []struct {
		name        string
		off, length int64
	}{
		{"负偏移", -1, 16},
		{"负长度", 0, -1},
		{"相加溢出", maxInt64, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, req := newSparseCtx(t, 4096)
			req.CtlCode = wire.FSCTLQueryAllocatedRanges
			req.MaxOutputResponse = 4096
			req.Input = encodeAllocatedRangeInput(tc.off, tc.length)
			if err := ioctlQueryAllocatedRanges(ctx, req); err != status.InvalidParameter {
				t.Errorf("err = %v, 期望 %v", err, status.InvalidParameter)
			}
		})
	}

	// 输入短于 16 字节同样非法。
	ctx, req := newSparseCtx(t, 4096)
	req.CtlCode = wire.FSCTLQueryAllocatedRanges
	req.MaxOutputResponse = 4096
	req.Input = make([]byte, 15)
	if err := ioctlQueryAllocatedRanges(ctx, req); err != status.InvalidParameter {
		t.Errorf("短输入 err = %v, 期望 %v", err, status.InvalidParameter)
	}
}

// TestTruncateAllocatedRanges 验证输出缓冲不足时的截断规则：
// 只能按整条 FILE_ALLOCATED_RANGE_BUFFER 切，且要报 overflow。
//
// 切一半会让客户端解析出一个内容错误的区间 —— 对稀疏文件来说这意味着
// 它会以为某段数据存在（或不存在），后果比报错严重得多。
func TestTruncateAllocatedRanges(t *testing.T) {
	ranges := []wire.FileAllocatedRangeBuffer{
		{FileOffset: 0, Length: 100},
		{FileOffset: 200, Length: 100},
		{FileOffset: 400, Length: 100},
	}

	// 装得下全部。
	out, overflow := truncateAllocatedRanges(ranges, 48)
	if overflow {
		t.Error("48 字节刚好装下 3 条，不应 overflow")
	}
	if len(out) != 48 {
		t.Errorf("输出 = %d 字节, 期望 48", len(out))
	}

	// 只装得下 2 条：必须截断且报 overflow。
	out, overflow = truncateAllocatedRanges(ranges, 47)
	if !overflow {
		t.Error("47 字节装不下 3 条，应报 overflow")
	}
	if len(out) != 32 {
		t.Errorf("输出 = %d 字节, 期望 32（2 条整）", len(out))
	}
	got := parseRanges(t, out)
	if len(got) != 2 || got[1].FileOffset != 200 || got[1].Length != 100 {
		t.Errorf("截断后的区间 = %+v, 期望前两条原样", got)
	}

	// 一条都装不下：空输出 + overflow，而不是半条。
	out, overflow = truncateAllocatedRanges(ranges, 15)
	if !overflow || len(out) != 0 {
		t.Errorf("out = %d 字节 overflow = %v, 期望 0 字节且 overflow", len(out), overflow)
	}

	// 本来就没有区间（全是空洞）：空输出且不是 overflow。
	out, overflow = truncateAllocatedRanges(nil, 4096)
	if overflow || len(out) != 0 {
		t.Errorf("空区间列表 out = %d 字节 overflow = %v, 期望 0 字节且非 overflow",
			len(out), overflow)
	}
}

// TestIoctlQueryAllocatedRangesBufferTooSmall 验证「连一条区间都装不下」时
// 回 STATUS_BUFFER_TOO_SMALL，而不是空输出 + BUFFER_OVERFLOW。
//
// 依据 Samba source3/smbd/smb2_ioctl_filesys.c:fsctl_qar：
//
//	/* must have enough space for at least one range */
//	if (in_max_output < sizeof(struct file_alloced_range_buf)) {
//		return NT_STATUS_BUFFER_TOO_SMALL;
//	}
//
// 两者的区别对客户端不是无所谓的：BUFFER_OVERFLOW 是「还有更多，加大缓冲再来」，
// 而空输出 + OVERFLOW 会被解释成「这段没有已分配区间」，客户端可能据此认为
// 整个 band 都是洞。
func TestIoctlQueryAllocatedRangesBufferTooSmall(t *testing.T) {
	const size = 1 << 20
	ctx, req := newSparseCtx(t, size)

	req.CtlCode = wire.FSCTLQueryAllocatedRanges
	req.Input = encodeAllocatedRangeInput(0, size)
	// 15 字节：比一条 FILE_ALLOCATED_RANGE_BUFFER(16) 少一个字节。
	req.MaxOutputResponse = allocatedRangeSize - 1

	if err := ioctlQueryAllocatedRanges(ctx, req); err != status.BufferTooSmall {
		t.Errorf("err = %v, 期望 %v", err, status.BufferTooSmall)
	}
	if ctx.Status == status.BufferOverflow {
		t.Error("不应把「一条都装不下」报成 BUFFER_OVERFLOW")
	}
}

// TestIoctlQueryAllocatedRangesEmptyBeatsBufferCheck 验证空结果的判定
// **排在缓冲区大小检查之前**。
//
// Samba 的 fsctl_qar 里，`len == 0 / 文件为空 / file_off >= EOF` 这三种情况
// 直接 `return NT_STATUS_OK`，根本走不到那句 BUFFER_TOO_SMALL。顺序反了的话，
// 客户端拿 16 字节缓冲去问一段位于 EOF 之外的区间会收到错误而不是「没有区间」。
func TestIoctlQueryAllocatedRangesEmptyBeatsBufferCheck(t *testing.T) {
	const size = 4096
	ctx, req := newSparseCtx(t, size)

	req.CtlCode = wire.FSCTLQueryAllocatedRanges
	// 查询窗口整个落在 EOF 之外：vfs.AllocatedRanges 契约保证回 nil, nil。
	req.Input = encodeAllocatedRangeInput(size*2, size)
	req.MaxOutputResponse = 8 // 故意小于一条区间

	if err := ioctlQueryAllocatedRanges(ctx, req); err != nil {
		t.Fatalf("EOF 之外的查询应成功，实际 err=%v", err)
	}
	if ctx.Status != 0 {
		t.Errorf("ctx.Status = %v, 期望 STATUS_SUCCESS", ctx.Status)
	}
	if got := parseRangesFromIoctlOut(t, ctx.Out); len(got) != 0 {
		t.Errorf("EOF 之外应无已分配区间，实际 %+v", got)
	}
}

// TestIoctlQueryAllocatedRangesOverflowTruncates 验证 handler 层的截断语义：
// 装得下 1 条但装不下全部时，回**整条**区间 + STATUS_BUFFER_OVERFLOW。
//
// TestTruncateAllocatedRanges 测的是纯函数，这里测的是 handler 有没有把
// overflow 真的写进 ctx.Status —— 漏写的话客户端会以为自己拿到了完整列表。
func TestIoctlQueryAllocatedRangesOverflowTruncates(t *testing.T) {
	const size = 1 << 20
	ctx, req := newSparseCtx(t, size)
	open := ctx.Chain.LastOpen

	// 中间打洞，制造「前段有数据 + 洞 + 后段有数据」两条区间。
	sp := open.Handle.(vfs.SparseFile)
	if err := sp.PunchHole(size/4, size/2); err != nil {
		t.Fatalf("PunchHole: %v", err)
	}

	req.CtlCode = wire.FSCTLQueryAllocatedRanges
	req.Input = encodeAllocatedRangeInput(0, size)
	req.MaxOutputResponse = 4096
	if err := ioctlQueryAllocatedRanges(ctx, req); err != nil {
		t.Fatalf("QUERY_ALLOCATED_RANGES err=%v", err)
	}
	full := parseRangesFromIoctlOut(t, ctx.Out)
	if len(full) < 2 {
		// tmpfs / overlayfs 探测不到空洞时会降级成「整个窗口已分配」，
		// 这是 vfs 层承诺的合法降级（不是 bug），此时无从构造截断场景。
		t.Skipf("后端未报告多段区间（得到 %d 段），空洞探测可能已降级，跳过截断用例", len(full))
	}

	// 只给一条的空间：应回第一条原样 + BUFFER_OVERFLOW。
	ctx.Out = ctx.Out[:0]
	ctx.Status = 0
	req.MaxOutputResponse = allocatedRangeSize
	if err := ioctlQueryAllocatedRanges(ctx, req); err != nil {
		t.Fatalf("截断场景不应返回错误，实际 err=%v", err)
	}
	if ctx.Status != status.BufferOverflow {
		t.Errorf("ctx.Status = %v, 期望 %v", ctx.Status, status.BufferOverflow)
	}
	got := parseRangesFromIoctlOut(t, ctx.Out)
	if len(got) != 1 {
		t.Fatalf("截断后 = %d 条, 期望 1 条整", len(got))
	}
	if got[0] != full[0] {
		t.Errorf("截断后的第一条 = %+v, 期望与完整结果的第一条一致 %+v", got[0], full[0])
	}
}

// TestIoctlSparseRejectsReadOnlyShare：只读共享上的写类 FSCTL 必须被拒。
func TestIoctlSparseRejectsReadOnlyShare(t *testing.T) {
	ctx, req := newSparseCtx(t, 4096)
	ctx.Tree.Share.ReadOnly = true

	req.CtlCode = wire.FSCTLSetSparse
	if err := ioctlSetSparse(ctx, req); err != status.MediaWriteProtected {
		t.Errorf("只读共享 SET_SPARSE err = %v, 期望 %v", err, status.MediaWriteProtected)
	}
	req.CtlCode = wire.FSCTLSetZeroData
	req.Input = encodeZeroData(0, 4096)
	if err := ioctlSetZeroData(ctx, req); err != status.MediaWriteProtected {
		t.Errorf("只读共享 SET_ZERO_DATA err = %v, 期望 %v", err, status.MediaWriteProtected)
	}
}

// --- 测试脚手架 -------------------------------------------------------------

// newSparseCtx 造一个「已连上可写共享、已打开一个 size 字节文件」的 Context。
//
// 用 Chain.LastOpen + 复合句柄（FileID 全 FF）绕开 Session 的句柄表：
// 这几个 handler 只经过 resolveOpen，用复合路径能少造一半状态。
func newSparseCtx(t *testing.T, size int64) (*Context, *wire.IoctlRequest) {
	t.Helper()

	root := t.TempDir()
	path := filepath.Join(root, "band")
	if size > 0 {
		data := make([]byte, size)
		for i := range data {
			// 非零内容，否则文件系统可能自己就把它存成空洞，
			// 打洞前后的分配长度就没有区别了。
			data[i] = byte(i%251 + 1)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("准备测试文件: %v", err)
		}
	} else if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("准备测试文件: %v", err)
	}

	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	h, _, err := fs.Open(&vfs.OpenRequest{
		Path:        "band",
		Flags:       vfs.OpenRead | vfs.OpenWrite,
		Disposition: vfs.OpenExisting,
	})
	if err != nil {
		t.Fatalf("vfs Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	share := &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}
	tree := &Tree{ID: 1, Share: share}
	open := &Open{
		Path:          "band",
		Handle:        h,
		Tree:          tree,
		GrantedAccess: wire.FileReadData | wire.FileWriteData | wire.FileWriteAttributes,
	}

	ctx := &Context{
		Tree: tree,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx.Chain = &Chain{LastOpen: open}

	req := &wire.IoctlRequest{
		Flags:             wire.IoctlIsFSCTL,
		FileID:            wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)},
		MaxOutputResponse: 4096,
	}
	return ctx, req
}

func encodeZeroData(off, beyond int64) []byte {
	b := make([]byte, 16)
	putLE64(b[0:], off)
	putLE64(b[8:], beyond)
	return b
}

func encodeAllocatedRangeInput(off, length int64) []byte {
	b := make([]byte, 16)
	putLE64(b[0:], off)
	putLE64(b[8:], length)
	return b
}

func putLE64(b []byte, v int64) {
	u := uint64(v)
	for i := 0; i < 8; i++ {
		b[i] = byte(u >> (8 * i))
	}
}

func le64(b []byte) int64 {
	var u uint64
	for i := 0; i < 8; i++ {
		u |= uint64(b[i]) << (8 * i)
	}
	return int64(u)
}

func parseRanges(t *testing.T, b []byte) []wire.FileAllocatedRangeBuffer {
	t.Helper()
	if len(b)%allocatedRangeSize != 0 {
		t.Fatalf("输出 %d 字节不是 %d 的整数倍", len(b), allocatedRangeSize)
	}
	out := make([]wire.FileAllocatedRangeBuffer, 0, len(b)/allocatedRangeSize)
	for i := 0; i+allocatedRangeSize <= len(b); i += allocatedRangeSize {
		out = append(out, wire.FileAllocatedRangeBuffer{
			FileOffset: le64(b[i:]),
			Length:     le64(b[i+8:]),
		})
	}
	return out
}

// parseRangesFromIoctlOut 从 IOCTL Response 报文体里取出 Output。
//
// Output 在没有 Input 的响应里位于报文末尾，且长度是 16 的整数倍，
// 直接从尾部按整块回切即可（wire 层的编码细节在 wire 包自己的测试里覆盖）。
func parseRangesFromIoctlOut(t *testing.T, body []byte) []wire.FileAllocatedRangeBuffer {
	t.Helper()
	// IoctlResponse 固定部分是 48 字节（MS-SMB2 §2.2.32）。
	const ioctlRespFixed = 48
	if len(body) < ioctlRespFixed {
		t.Fatalf("IOCTL 响应体 %d 字节，短于固定部分 %d", len(body), ioctlRespFixed)
	}
	out := body[ioctlRespFixed:]
	// 输出为空时报文里仍有 1 个字节的占位符：SMB2 的变长字段即使长度为 0
	// 也要留一个字节，否则 Buffer 偏移会落在报文之外。这不是一条区间。
	if len(out) <= 1 {
		return nil
	}
	return parseRanges(t, out)
}
