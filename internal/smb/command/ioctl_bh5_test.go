package command

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// bh5 轮 FSCTL 缺陷的回归测试（docs/bughunt-20260825/findings-bh5.md）。
//
// F4：SET_ZERO_DATA 此前不做字节范围锁检查 —— 锁表只在 LOCK 命令与
// CLOSE 里被使用。Samba 在 smb2_ioctl_filesys.c:459-468 对 zero_data
// 先做 SMB_VFS_STRICT_LOCK_CHECK（WRITE_LOCK 语义），冲突回
// NT_STATUS_FILE_LOCK_CONFLICT。
// F7/F8：QAR 与 SET_SPARSE 的访问掩码口径。

// runZeroData 构造一条 SET_ZERO_DATA 请求并直接喂给 handler，
// open 走复合 FileId 复用通道注入。
func runZeroData(t *testing.T, tree *Tree, open *Open, off, beyond int64) error {
	t.Helper()
	req := &wire.IoctlRequest{
		CtlCode:           wire.FSCTLSetZeroData,
		Flags:             wire.IoctlIsFSCTL,
		FileID:            compoundFID,
		Input:             encodeZeroData(off, beyond),
		MaxOutputResponse: 4096,
	}
	ctx := &Context{
		Tree:  tree,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Chain: &Chain{},
	}
	ctx.Chain.LastOpen = open
	return ioctlSetZeroData(ctx, req)
}

// TestSetZeroDataConflictsWithForeignByteRangeLock：A 对 [0,100) 持独占锁后，
// B 打洞区间内必须 FILE_LOCK_CONFLICT、区间外必须成功；
// A 自己打自己锁定的区间不受影响（豁免单位是句柄，与 LOCK/READ/WRITE 一致）。
func TestSetZeroDataConflictsWithForeignByteRangeLock(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("0123456789abcdef"), 32))

	if !lockOne(p.table, "f", p.a, 0, 100, true) {
		t.Fatal("A 加独占锁应成功")
	}

	wantStatus(t, "B 打洞 [50,60) 撞 A 的独占锁",
		runZeroData(t, p.tree, p.b, 50, 60), status.LockConflict)

	if err := runZeroData(t, p.tree, p.b, 200, 208); err != nil {
		t.Fatalf("B 打洞 [200,208) 不应受 [0,100) 锁影响: %v", err)
	}

	if err := runZeroData(t, p.tree, p.a, 10, 20); err != nil {
		t.Fatalf("A 打洞自己持有的锁区间不应冲突: %v", err)
	}
}

// TestSetZeroDataConflictsWithSharedLock：共享锁不挡读但挡写；
// 打洞是写操作，必须被共享锁挡住（Samba 的检查用 WRITE_LOCK 语义）。
func TestSetZeroDataConflictsWithSharedLock(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("0123456789abcdef"), 32))

	if !lockOne(p.table, "f", p.a, 0, 100, false) {
		t.Fatal("A 加共享锁应成功")
	}
	wantStatus(t, "B 打洞撞 A 的共享锁",
		runZeroData(t, p.tree, p.b, 50, 60), status.LockConflict)
}

// TestQueryAllocatedRangesRequiresReadAccess（bh5-F7）：
// QUERY_ALLOCATED_RANGES 只认 FILE_READ_DATA，只写句柄必须被拒。
// Samba 对照：smb2_ioctl_filesys.c:632 check_any_access_fsp(fsp, FILE_READ_DATA)。
func TestQueryAllocatedRangesRequiresReadAccess(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("0123456789abcdef"), 32))
	p.b.GrantedAccess = wire.FileWriteData // 只写句柄

	req := &wire.IoctlRequest{
		CtlCode:           wire.FSCTLQueryAllocatedRanges,
		Flags:             wire.IoctlIsFSCTL,
		FileID:            compoundFID,
		Input:             encodeAllocatedRangeInput(0, 512),
		MaxOutputResponse: 4096,
	}
	ctx := &Context{Tree: p.tree, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Chain: &Chain{}}
	ctx.Chain.LastOpen = p.b
	wantStatus(t, "只写句柄查询已分配区间", ioctlQueryAllocatedRanges(ctx, req), status.AccessDenied)
}

// TestSetSparseAcceptsAppendAccess（bh5-F8）：以 FILE_APPEND_DATA 打开的句柄
// 也允许设稀疏位。Windows Server 2008/2012 允许 WRITE_DATA|WRITE_ATTRIBUTES|
// APPEND_DATA 任一（Samba dosmode.c:1118-1128 引的正是这三家任一）。
func TestSetSparseAcceptsAppendAccess(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "band")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("准备测试文件: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	h, _, err := fs.Open(&vfs.OpenRequest{Path: "band", Flags: vfs.OpenRead | vfs.OpenWrite, Disposition: vfs.OpenExisting})
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
		GrantedAccess: wire.FileAppendData, // 只有 append 位
	}
	req := &wire.IoctlRequest{
		CtlCode:           wire.FSCTLSetSparse,
		Flags:             wire.IoctlIsFSCTL,
		FileID:            compoundFID,
		MaxOutputResponse: 4096,
	}
	ctx := &Context{Tree: tree, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Chain: &Chain{}}
	ctx.Chain.LastOpen = open
	if err := ioctlSetSparse(ctx, req); err != nil {
		t.Fatalf("仅 APPEND_DATA 的 SET_SPARSE 应成功，实际 err=%v", err)
	}
}

// newStreamIoctlCtx（bh5-F9）：基础文件 + 其上的一个已打开流句柄。
func newStreamIoctlCtx(t *testing.T) (*Context, *wire.IoctlRequest) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "band"), []byte("base"), 0o644); err != nil {
		t.Fatalf("准备基础文件: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	h, _, err := fs.Open(&vfs.OpenRequest{
		Path:        "band",
		Stream:      "meta",
		Flags:       vfs.OpenRead | vfs.OpenWrite,
		Disposition: vfs.OpenAlways,
	})
	if err != nil {
		t.Fatalf("vfs 打开流: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	share := &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}
	tree := &Tree{ID: 1, Share: share}
	open := &Open{
		Path:          "band",
		Stream:        "meta",
		Handle:        h,
		Tree:          tree,
		GrantedAccess: wire.FileReadData | wire.FileWriteData | wire.FileWriteAttributes,
	}
	ctx := &Context{Tree: tree, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Chain: &Chain{}}
	ctx.Chain.LastOpen = open
	req := &wire.IoctlRequest{
		Flags:             wire.IoctlIsFSCTL,
		FileID:            compoundFID,
		MaxOutputResponse: 4096,
	}
	return ctx, req
}

// TestSetSparseOnStreamIsNoOpSuccess（bh5-F9）：流句柄上的 SET_SPARSE 必须
// 无操作成功。Samba vfswrap_fsctl 开头 metadata_fsp(fsp)，对流上的稀疏位设置
// 直接假装成功（dosmode.c:1147-1155，MS-FSA §2.1.1.5：流永远不是稀疏文件）；
// 回 NOT_SUPPORTED 会让客户端把整个共享当成「不支持稀疏」而放弃打洞。
func TestSetSparseOnStreamIsNoOpSuccess(t *testing.T) {
	ctx, req := newStreamIoctlCtx(t)

	req.CtlCode = wire.FSCTLSetSparse
	req.Input = []byte{1}
	if err := ioctlSetSparse(ctx, req); err != nil {
		t.Fatalf("流句柄上的 SET_SPARSE(TRUE) 应无操作成功，实际 err=%v", err)
	}
	// FALSE 同样假装成功（参照实现对流不做任何形态改变）。
	ctx.Out = ctx.Out[:0]
	req.Input = []byte{0}
	if err := ioctlSetSparse(ctx, req); err != nil {
		t.Fatalf("流句柄上的 SET_SPARSE(FALSE) 应无操作成功，实际 err=%v", err)
	}
}

// TestZeroDataAndQarOnStreamNotSupported（bh5-F9 记录在案）：流句柄上的
// SET_ZERO_DATA / QUERY_ALLOCATED_RANGES 回 STATUS_NOT_SUPPORTED。
//
// Samba 经 metadata_fsp 把它们映射到基础文件；我们不重开基础文件（要绕共享
// 模式检查、凭空多 fd），findings-bh5 F9 原文允许维持 NOT_SUPPORTED 并记录。
// 这条用例把该分歧钉住：将来若真做了映射，改这里时必须连带补映射语义测试。
func TestZeroDataAndQarOnStreamNotSupported(t *testing.T) {
	ctx, req := newStreamIoctlCtx(t)

	req.CtlCode = wire.FSCTLSetZeroData
	req.Input = encodeZeroData(0, 4)
	if err := ioctlSetZeroData(ctx, req); err != status.NotSupported {
		t.Errorf("流句柄上的 SET_ZERO_DATA err = %v, 期望 %v", err, status.NotSupported)
	}

	req.CtlCode = wire.FSCTLQueryAllocatedRanges
	req.Input = encodeAllocatedRangeInput(0, 4)
	if err := ioctlQueryAllocatedRanges(ctx, req); err != status.NotSupported {
		t.Errorf("流句柄上的 QAR err = %v, 期望 %v", err, status.NotSupported)
	}
}
