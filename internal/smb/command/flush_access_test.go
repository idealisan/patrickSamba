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

// 本文件钉的是 bh4-A#8：FLUSH 此前不做任何访问校验 —— 只读文件句柄、
// 无 ADD 权限的目录句柄都静默回成功。Samba smb2_flush.c:171-199：
//
//	IPC            ⇒ NOT_IMPLEMENTED（我们保持成功，更宽松且无害）
//	普通文件        ⇒ 需 FILE_WRITE_DATA|FILE_APPEND_DATA，否则 ACCESS_DENIED
//	目录           ⇒ 需 FILE_ADD_FILE|FILE_ADD_SUBDIRECTORY，否则 ACCESS_DENIED
//	fd == -1       ⇒ INVALID_HANDLE（对应我们的 Handle==nil → FILE_CLOSED）

// newFlushCtx 造一个可直接喂给 handleFlush 的 Context 与配套 Open。
func newFlushCtx(t *testing.T, isDir bool) (*Context, *Open) {
	t.Helper()

	root := t.TempDir()
	if !isDir {
		if err := os.WriteFile(filepath.Join(root, "f"), []byte("data"), 0o644); err != nil {
			t.Fatalf("准备测试文件: %v", err)
		}
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	var h vfs.Handle
	if !isDir {
		h, _, err = fs.Open(&vfs.OpenRequest{
			Path:        "f",
			Flags:       vfs.OpenRead | vfs.OpenWrite,
			Disposition: vfs.OpenExisting,
		})
	} else {
		h, _, err = fs.Open(&vfs.OpenRequest{
			Path:        "",
			Flags:       vfs.OpenRead,
			Disposition: vfs.OpenExisting,
		})
	}
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
		GrantedAccess: wire.FileReadData | wire.FileWriteData | wire.FileAppendData,
	}

	req := &wire.FlushRequest{FileID: compoundFID}
	msg := req.Append(make([]byte, wire.HeaderSize))
	ctx := &Context{
		Conn:  &Conn{Settings: &Settings{}},
		Chain: &Chain{},
		Tree:  tree,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Msg:   msg,
		Out:   make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = open
	return ctx, open
}

// runFlush 直接喂 handleFlush。
func runFlush(ctx *Context, open *Open) error {
	ctx.Out = make([]byte, wire.HeaderSize)
	ctx.Chain.LastOpen = open
	return handleFlush(ctx)
}

// TestFlushReadOnlyHandleDenied（bh4-A#8）：只有读权限的句柄上 FLUSH 必须
// 回 STATUS_ACCESS_DENIED —— Samba smb2_flush.c:174-199 要求
// FILE_WRITE_DATA|FILE_APPEND_DATA。
func TestFlushReadOnlyHandleDenied(t *testing.T) {
	ctx, open := newFlushCtx(t, false)
	open.GrantedAccess = wire.FileReadData // 有读无写
	wantStatus(t, "只读句柄的 FLUSH", runFlush(ctx, open), status.AccessDenied)

	// 对照：带写权限照常成功。
	open.GrantedAccess = wire.FileReadData | wire.FileWriteData
	if err := runFlush(ctx, open); err != nil {
		t.Fatalf("可写句柄的 FLUSH 不应失败: %v", err)
	}
}

// TestFlushDirectoryNeedsAddRight（bh4-A#8）：目录句柄需要
// FILE_ADD_FILE|FILE_ADD_SUBDIRECTORY 才能 FLUSH；无此权限回
// STATUS_ACCESS_DENIED 而不是静默成功。
//
// 注：MS-FSCT 的 FILE_ADD_FILE=0x2、FILE_ADD_SUBDIRECTORY=0x4 与
// FILE_WRITE_DATA/FILE_APPEND_DATA 同值，wire 包按同值复用常量。
func TestFlushDirectoryNeedsAddRight(t *testing.T) {
	ctx, dirOpen := newFlushCtx(t, true)

	dirOpen.GrantedAccess = wire.FileReadData
	wantStatus(t, "无 ADD 权限目录句柄的 FLUSH", runFlush(ctx, dirOpen), status.AccessDenied)

	dirOpen.GrantedAccess = wire.FileWriteData | wire.FileAppendData // 即 ADD_FILE|ADD_SUBDIR
	if err := runFlush(ctx, dirOpen); err != nil {
		t.Fatalf("有 ADD 权限的目录句柄 FLUSH 不应失败: %v", err)
	}
}

// TestFlushWithoutHandleRejected（bh4-A#8）：没有底层 fd 的句柄（attr-only
// 打开等）FLUSH 回 STATUS_FILE_CLOSED —— 对应 Samba 的 fd==-1 ⇒ INVALID_HANDLE，
// 不能静默成功假装刷过了。
func TestFlushWithoutHandleRejected(t *testing.T) {
	ctx, open := newFlushCtx(t, false)
	open.Handle = nil
	wantStatus(t, "无 fd 句柄的 FLUSH", runFlush(ctx, open), status.FileClosed)
}

// syncSpy 记录 Sync 的调用与参数，其余方法委托给真实句柄。
//
// 嵌入 vfs.Handle 接口：未覆盖的方法自动提升，于是它仍然是合法的 vfs.Handle，
// 不必为了做间谍去实现一整个接口。
type syncSpy struct {
	vfs.Handle
	calls int
	full  []bool
}

func (s *syncSpy) Sync(full bool) error {
	s.calls++
	s.full = append(s.full, full)
	return s.Handle.Sync(full)
}

// TestFlushReallyCallsFullSync：FLUSH 必须真的走到**强制刷盘**那一档。
//
// 现有的 vfs/sync_test.go 自承"数据真到盘片上要断电才知道"，它的
// TestSyncPersistsData 靠 os.ReadFile 读回验证 —— page cache 本来就是一致的，
// **把 Sync 改成 no-op 那条用例照样通过**。也就是说全量测试目前拦不住
// "FLUSH 被悄悄删掉"这种回归，而这正是 Time Machine 最要命的一条
// （AAPL 里宣告了 kAAPL_SUPPORTS_FULL_SYNC，客户端据此相信数据已落盘）。
//
// 本例不看落盘效果（那要断电），只钉住**调用契约**：FLUSH 必须调一次
// Sync，且 full=true（F_FULLFSYNC 语义那一档，不是普通 fsync）。
//
// 变异自检：把 read_write.go 里的 h.Sync(true) 删掉 → calls 断言变红；
// 把它改成 h.Sync(false) → full 断言变红（在 darwin 上两者不是一回事）。
func TestFlushReallyCallsFullSync(t *testing.T) {
	ctx, open := newFlushCtx(t, false)

	spy := &syncSpy{Handle: open.Handle}
	open.Handle = spy
	t.Cleanup(func() { open.Handle = spy.Handle })

	if err := runFlush(ctx, open); err != nil {
		t.Fatalf("FLUSH 不应失败: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("FLUSH 应当调一次 Sync，实际 %d 次", spy.calls)
	}
	if !spy.full[0] {
		t.Fatal("FLUSH 必须用 full=true（F_FULLFSYNC 语义），实际传的是 false")
	}
}
