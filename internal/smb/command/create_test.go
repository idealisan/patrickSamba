package command

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// create_test.go —— CREATE 里 Time Machine 相关的行为。

// alsiContext 造一个 "AlSi" create context（MS-SMB2 §2.2.13.2.2：AllocationSize(8)，小端）。
func alsiContext(size uint64) wire.CreateContext {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, size)
	return wire.CreateContext{Name: wire.CreateContextAlSi, Data: data}
}

// TestApplyAllocationSize 验证 AlSi 真的落到了后端的预留调用上。
//
// 动机：.sparsebundle 的 band 文件（通常 8 MiB）建的时候 macOS 会带 AlSi，
// 不预留会让备份文件在磁盘上很碎。
func TestApplyAllocationSize(t *testing.T) {
	const size = 8 << 20

	tests := []struct {
		name     string
		ctxs     []wire.CreateContext
		action   vfs.Action
		wantOff  int64
		wantLen  int64
		wantCall bool
	}{
		{
			name:     "新建文件时预留",
			ctxs:     []wire.CreateContext{alsiContext(size)},
			action:   vfs.ActionCreated,
			wantOff:  0,
			wantLen:  size,
			wantCall: true,
		},
		{
			name:     "覆盖时预留",
			ctxs:     []wire.CreateContext{alsiContext(size)},
			action:   vfs.ActionOverwritten,
			wantOff:  0,
			wantLen:  size,
			wantCall: true,
		},
		{
			// 只是打开一个已有文件：不能悄悄改变它的磁盘占用。
			name:   "打开已有文件不预留",
			ctxs:   []wire.CreateContext{alsiContext(size)},
			action: vfs.ActionOpened,
		},
		{
			name:   "没有 AlSi",
			action: vfs.ActionCreated,
		},
		{
			name:   "AlSi 长度非法",
			ctxs:   []wire.CreateContext{{Name: wire.CreateContextAlSi, Data: []byte{1, 2, 3}}},
			action: vfs.ActionCreated,
		},
		{
			name:   "AlSi 为 0",
			ctxs:   []wire.CreateContext{alsiContext(0)},
			action: vfs.ActionCreated,
		},
		{
			// 超出 int64 只可能是畸形输入，必须在转成 int64 之前拦下。
			name:   "AlSi 超出 int64",
			ctxs:   []wire.CreateContext{alsiContext(1 << 63)},
			action: vfs.ActionCreated,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newCreateTestContext(t)
			h := &fakeSparseHandle{}
			applyAllocationSize(ctx, &wire.CreateRequest{Contexts: tc.ctxs}, h, tc.action)

			if h.calls != boolToInt(tc.wantCall) {
				t.Fatalf("Preallocate 调用了 %d 次, 期望 %d 次", h.calls, boolToInt(tc.wantCall))
			}
			if tc.wantCall && (h.off != tc.wantOff || h.length != tc.wantLen) {
				t.Errorf("Preallocate(%d, %d), 期望 (%d, %d)", h.off, h.length, tc.wantOff, tc.wantLen)
			}
		})
	}
}

// TestApplyAllocationSizeBackendUnsupported：后端不支持预留时不得让 CREATE 失败。
func TestApplyAllocationSizeBackendUnsupported(t *testing.T) {
	ctx := newCreateTestContext(t)
	// 不实现 vfs.SparseFile 的句柄：直接静默跳过，不能 panic。
	applyAllocationSize(ctx, &wire.CreateRequest{Contexts: []wire.CreateContext{alsiContext(1 << 20)}},
		&fakePlainHandle{}, vfs.ActionCreated)

	// 实现了但报错：同样只记日志。
	h := &fakeSparseHandle{err: vfs.ErrNotSupported}
	applyAllocationSize(ctx, &wire.CreateRequest{Contexts: []wire.CreateContext{alsiContext(1 << 20)}},
		h, vfs.ActionCreated)
	if h.calls != 1 {
		t.Errorf("Preallocate 调用了 %d 次, 期望 1 次", h.calls)
	}
}

// TestCreateAllocationSizeEndToEnd 走真实 LocalFS：AlSi 之后文件的
// AllocationSize 应当已经反映预留结果，而 EndOfFile 仍然是 0
// （FALLOC_FL_KEEP_SIZE 语义：只占块不改逻辑长度）。
func TestCreateAllocationSizeEndToEnd(t *testing.T) {
	root := t.TempDir()
	fs := newQueryDirTestFS(t, root)

	const size = 1 << 20
	h, action, err := fs.Open(&vfs.OpenRequest{
		Path:        "band-0",
		Flags:       vfs.OpenRead | vfs.OpenWrite,
		Disposition: vfs.CreateNew,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	ctx := newCreateTestContext(t)
	applyAllocationSize(ctx, &wire.CreateRequest{Contexts: []wire.CreateContext{alsiContext(size)}}, h, action)

	attr, err := h.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if attr.Size != 0 {
		t.Errorf("EndOfFile = %d, 期望 0（预留不得改变逻辑长度）", attr.Size)
	}
	if attr.Alloc < size {
		// 宿主文件系统不支持 fallocate 时会退化，此处只做提示而非失败：
		// 容器里的 overlayfs/tmpfs 行为不一。
		t.Logf("AllocationSize = %d < %d：宿主文件系统可能不支持预留", attr.Alloc, size)
	}

	// 预留过的文件必须仍然能被正常枚举与打开。
	if _, err := os.Stat(filepath.Join(root, "band-0")); err != nil {
		t.Errorf("预留后文件不见了: %v", err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func newCreateTestContext(t *testing.T) *Context {
	t.Helper()
	return newAAPLTestContext(t, true, false, true)
}

// fakePlainHandle 是不实现 vfs.SparseFile 的最小句柄。
type fakePlainHandle struct{}

func (*fakePlainHandle) Close() error                          { return nil }
func (*fakePlainHandle) ReadAt([]byte, int64) (int, error)     { return 0, vfs.ErrNotSupported }
func (*fakePlainHandle) WriteAt([]byte, int64) (int, error)    { return 0, vfs.ErrNotSupported }
func (*fakePlainHandle) Truncate(int64) error                  { return vfs.ErrNotSupported }
func (*fakePlainHandle) Sync(bool) error                       { return nil }
func (*fakePlainHandle) Stat() (*vfs.Attr, error)              { return &vfs.Attr{}, nil }
func (*fakePlainHandle) SetAttr(*vfs.Attr, vfs.AttrMask) error { return vfs.ErrNotSupported }
func (*fakePlainHandle) Xattr() (vfs.XattrAccessor, error)     { return nil, vfs.ErrNotSupported }
func (*fakePlainHandle) ReadDir(string, bool, int) ([]vfs.DirEntry, error) {
	return nil, vfs.ErrNotDir
}

// fakeSparseHandle 记录 Preallocate 的调用参数。
type fakeSparseHandle struct {
	fakePlainHandle
	calls  int
	off    int64
	length int64
	err    error
}

func (h *fakeSparseHandle) PunchHole(int64, int64) error { return vfs.ErrNotSupported }
func (h *fakeSparseHandle) SetSparse(bool) error         { return nil }

func (h *fakeSparseHandle) AllocatedRanges(int64, int64) ([]vfs.Range, error) { return nil, nil }

func (h *fakeSparseHandle) Preallocate(off, length int64) error {
	h.calls++
	h.off, h.length = off, length
	return h.err
}

var (
	_ vfs.Handle     = (*fakePlainHandle)(nil)
	_ vfs.SparseFile = (*fakeSparseHandle)(nil)
)
