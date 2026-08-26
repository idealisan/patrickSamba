package command

import (
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 本文件钉的是 bh4-A#7/A#9/A#10 在 **handler 层**的行为：
//
//	A#7  回绕锁区间 → STATUS_INVALID_LOCK_RANGE（table 层已验，此处钉透传）
//	A#9  多元素加锁含任一非 FAIL_IMMEDIATELY 元素 → STATUS_INVALID_PARAMETER；
//	     flags 必须精确匹配合法枚举（Samba smb2_lock.c:341-378）
//	A#10 目录句柄上的 LOCK → STATUS_INVALID_DEVICE_REQUEST
//	     （Samba locking/locking.c do_lock：!can_lock && is_directory）

// newLockHandlerCtx 造一个可以直接喂给 handleLock 的 Context。
// 用复合 FileId（全 FF）+ Chain.LastOpen 绕开会话句柄表（同 lock_close_test）。
func newLockHandlerCtx(t *testing.T, isDir bool) (*Context, *Open, *lockTable) {
	t.Helper()

	root := t.TempDir()
	file := filepath.Join(root, "f")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatalf("准备测试文件: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	h, _, err := fs.Open(&vfs.OpenRequest{
		Path:        "f",
		Flags:       vfs.OpenRead | vfs.OpenWrite,
		Disposition: vfs.OpenExisting,
	})
	if err != nil {
		t.Fatalf("vfs Open: %v", err)
	}

	share := &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}
	tree := &Tree{ID: 1, Share: share}
	open := &Open{
		Path:          "f",
		Handle:        h,
		Tree:          tree,
		IsDir:         isDir,
		GrantedAccess: wire.FileReadData | wire.FileWriteData,
	}
	return newTestLockContext(tree), open, &share.locks
}

// runLock 构造一条 LOCK 请求并直接喂给 handleLock。
func runLock(t *testing.T, ctx *Context, open *Open, elems ...wire.LockElement) error {
	t.Helper()
	req := &wire.LockRequest{FileID: compoundFID, Locks: elems}
	msg, err := req.Append(make([]byte, wire.HeaderSize))
	if err != nil {
		t.Fatalf("编码 LOCK Request: %v", err)
	}
	ctx.Msg = msg
	ctx.Out = make([]byte, wire.HeaderSize)
	ctx.Chain.LastOpen = open
	return handleLock(ctx)
}

// TestLockWrapRangeInvalidAtHandler（bh4-A#7）：handler 层把锁表的
// STATUS_INVALID_LOCK_RANGE 原样透传，且不留部分状态。
func TestLockWrapRangeInvalidAtHandler(t *testing.T) {
	ctx, open, tbl := newLockHandlerCtx(t, false)
	const max = uint64(math.MaxUint64)

	wantStatus(t, "回绕区间 LOCK",
		runLock(t, ctx, open, el(max-10, 20, true)), status.InvalidLockRange)
	if n := countLocks(tbl, open); n != 0 {
		t.Fatalf("被拒请求不得留下锁，实际 %d 条", n)
	}

	// 合法区间照常授予 —— 拒绝逻辑没有扩大化。
	if err := runLock(t, ctx, open, el(0, 10, true)); err != nil {
		t.Fatalf("正常加锁不应失败: %v", err)
	}
}

// TestLockMultiElementMustFailImmediately（bh4-A#9）：多元素**加锁**里
// 只要有一个元素未置 FAIL_IMMEDIATELY 就必须回 STATUS_INVALID_PARAMETER
// （MS-SMB2 §3.3.5.14.2 的 SHOULD；Samba smb2_lock.c:364-378 同判）。
func TestLockMultiElementMustFailImmediately(t *testing.T) {
	ctx, open, _ := newLockHandlerCtx(t, false)

	nonBlocking := el(0, 10, true)                       // 未置 FAIL_IMMEDIATELY
	blocking := el(20, 10, false)                        // 未置
	blocking.Flags |= wire.LockFlagFailImmediately       // 置位版
	blockingExcl := el(40, 10, true)                     // 未置
	blockingExcl.Flags |= wire.LockFlagFailImmediately   // 置位版

	// [blocking, nonBlocking] → 拒。
	wantStatus(t, "第二条未置 FAIL_IMMEDIATELY",
		runLock(t, ctx, open, blocking, nonBlocking), status.InvalidParameter)
	// [nonBlocking, blocking] → 同样拒（顺序无关）。
	wantStatus(t, "第一条未置 FAIL_IMMEDIATELY",
		runLock(t, ctx, open, nonBlocking, blockingExcl), status.InvalidParameter)

	// 对照组：全部置了 FAIL_IMMEDIATELY 的多元素请求照常接受。
	if err := runLock(t, ctx, open, blocking, blockingExcl); err != nil {
		t.Fatalf("全 FAIL_IMMEDIATELY 的多元素加锁不应失败: %v", err)
	}
	// 对照组：单元素不置 FAIL_IMMEDIATELY 是合法的（阻塞语义由 A#4 处理）。
	var tbl2 lockTable
	if st := tbl2.lock("f", open, []wire.LockElement{el(100, 10, true)}); st != status.Success {
		t.Fatalf("单元素非阻塞加锁在表层应授予，实际 %v", st)
	}
}

// TestLockFlagsExactEnumeration（bh4-A#9）：flags 必须精确匹配合法枚举 ——
// SHARED 与 EXCLUSIVE 同时置位、未知位（如裸 0x8）、UNLOCK 混入其它位，
// 都回 STATUS_INVALID_PARAMETER（Samba smb2_lock.c:341-360 精确 switch）。
func TestLockFlagsExactEnumeration(t *testing.T) {
	ctx, open, _ := newLockHandlerCtx(t, false)

	bad := []wire.LockElement{
		{Offset: 0, Length: 10, Flags: wire.LockFlagSharedLock | wire.LockFlagExclusiveLock},
		{Offset: 0, Length: 10, Flags: wire.LockFlags(0x8)}, // 未知位
		{Offset: 0, Length: 10, Flags: wire.LockFlagExclusiveLock | wire.LockFlags(0x20)},
		{Offset: 0, Length: 10, Flags: wire.LockFlagUnlock | wire.LockFlagSharedLock}, // 解锁混加锁位
	}
	for i, e := range bad {
		wantStatus(t, "非法 flags 组合", runLock(t, ctx, open, e), status.InvalidParameter)
		_ = i
	}

	// 合法四态对照（各用不同区间，避免撞上 A#6 的同句柄独占互斥）。
	good := []wire.LockFlags{
		wire.LockFlagSharedLock,
		wire.LockFlagExclusiveLock,
		wire.LockFlagSharedLock | wire.LockFlagFailImmediately,
		wire.LockFlagExclusiveLock | wire.LockFlagFailImmediately,
	}
	for i, f := range good {
		off := uint64(200 + 20*i)
		if err := runLock(t, ctx, open, wire.LockElement{Offset: off, Length: 10, Flags: f}); err != nil {
			t.Fatalf("合法 flags %#x 不应失败: %v", f, err)
		}
	}
}

// TestLockOnDirectoryRejected（bh4-A#10）：目录句柄上的字节范围锁无意义，
// 必须回 STATUS_INVALID_DEVICE_REQUEST（Samba do_lock：
// !fsp->can_lock && is_directory ⇒ NT_STATUS_INVALID_DEVICE_REQUEST）。
// 此前目录句柄会照常授锁并占用表项，与句柄消失路径的泄漏叠加放大。
func TestLockOnDirectoryRejected(t *testing.T) {
	ctx, dirOpen, tbl := newLockHandlerCtx(t, true)

	wantStatus(t, "目录句柄 LOCK",
		runLock(t, ctx, dirOpen, el(0, 10, true)), status.InvalidDeviceRequest)
	if n := countLocks(tbl, dirOpen); n != 0 {
		t.Fatalf("目录句柄不得在锁表留下任何条目，实际 %d 条", n)
	}
	wantStatus(t, "目录句柄 UNLOCK",
		runLock(t, ctx, dirOpen, unlockEl(0, 10)), status.InvalidDeviceRequest)
}

// newTestLockContext 给 tree 造一个可直接调用 handler 的空壳 Context
// （不含具体请求报文，runLock 会填）。
func newTestLockContext(tree *Tree) *Context {
	ctx := &Context{
		Conn:  &Conn{Settings: &Settings{}},
		Chain: &Chain{},
		Tree:  tree,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Out:   make([]byte, wire.HeaderSize),
	}
	return ctx
}
