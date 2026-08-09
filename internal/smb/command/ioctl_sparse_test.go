package command

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
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

	// 显式 FALSE：POSIX 后端把它实现成无操作（回 ErrNotSupported 会让 macOS
	// 直接放弃稀疏卷，见 vfs.SparseFile.SetSparse 的注释）。
	ctx.Out = ctx.Out[:0]
	req.Input = []byte{0}
	if err := ioctlSetSparse(ctx, req); err != nil {
		t.Fatalf("SetSparse=FALSE 应成功（无操作），实际 err=%v", err)
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
	ctx.Chain.LastOpen = open

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
	return parseRanges(t, body[ioctlRespFixed:])
}
