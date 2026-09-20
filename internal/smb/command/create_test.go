package command

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
	"github.com/idealisan/patrickSamba/internal/vfs"
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

// ---------------------------------------------------------------------------
// MxAc（SMB2_CREATE_QUERY_MAXIMAL_ACCESS）
// ---------------------------------------------------------------------------
//
// 权威行为取自 Samba `source3/smbd/smb2_create.c`（逐行核对）：
//   - L1608-1615：请求 Data 只接受 0 或 8 字节，其余长度整个 CREATE 回
//     STATUS_INVALID_PARAMETER。
//   - L1870-1889：仅当 `last_write_time != max_access_time` 才回响应 context；
//     响应是 `SIVAL(p,0,NT_STATUS_V(status)); SIVAL(p,4,max_access_granted)`。

// TestParseMxAcRequest 覆盖请求侧的三种长度。
func TestParseMxAcRequest(t *testing.T) {
	mk := func(data []byte) *wire.CreateRequest {
		return &wire.CreateRequest{Contexts: []wire.CreateContext{
			{Name: wire.CreateContextMxAc, Data: data},
		}}
	}
	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, 0x01d0_0000_dead_beef)

	t.Run("没有 MxAc", func(t *testing.T) {
		got, err := parseMxAcRequest(&wire.CreateRequest{})
		if err != nil || got.Present {
			t.Fatalf("= (%+v, %v), 期望 (不存在, nil)", got, err)
		}
	})

	// 空 Data 是**合法且常见**的形态（Windows 客户端大多这么发），
	// 绝不能当成畸形输入。
	t.Run("空 Data", func(t *testing.T) {
		got, err := parseMxAcRequest(mk(nil))
		if err != nil {
			t.Fatalf("空 Data 应当合法, err=%v", err)
		}
		if !got.Present || got.Timestamp != 0 {
			t.Errorf("= %+v, 期望 Present=true Timestamp=0", got)
		}
	})

	t.Run("8 字节 Timestamp", func(t *testing.T) {
		got, err := parseMxAcRequest(mk(ts))
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if !got.Present || got.Timestamp != 0x01d0_0000_dead_beef {
			t.Errorf("= %+v, 期望 Timestamp=0x01d00000deadbeef", got)
		}
	})

	for _, n := range []int{1, 4, 7, 9, 16} {
		t.Run(fmt.Sprintf("畸形长度 %d", n), func(t *testing.T) {
			if _, err := parseMxAcRequest(mk(make([]byte, n))); err != status.InvalidParameter {
				t.Errorf("err = %v, 期望 %v", err, status.InvalidParameter)
			}
		})
	}
}

// TestMxAcWantResponse 覆盖 Samba 的 Timestamp 缓存语义：
// 客户端带上「上次查询时的 LastWriteTime」，文件没变就不回 context。
func TestMxAcWantResponse(t *testing.T) {
	const mtime = uint64(0x01d0_1234_5678_9abc)

	tests := []struct {
		name string
		req  mxAcRequest
		want bool
	}{
		{"没请求 MxAc", mxAcRequest{}, false},
		{"空 Data（Timestamp=0）恒回", mxAcRequest{Present: true, Timestamp: 0}, true},
		{"Timestamp 与 mtime 相同 → 客户端缓存仍有效，不回", mxAcRequest{Present: true, Timestamp: mtime}, false},
		{"Timestamp 与 mtime 不同 → 文件变过，要回", mxAcRequest{Present: true, Timestamp: mtime - 1}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.wantResponse(mtime); got != tc.want {
				t.Errorf("wantResponse = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// TestMxAcMaximalAccess 验证回报的掩码是**真实**授权结果。
//
// 这是 MxAc 唯一真正重要的地方：无脑回 0x001F01FF 会让只读共享在
// Finder/Explorer 里显示成可写，用户点了写才报错。
func TestMxAcMaximalAccess(t *testing.T) {
	writeBits := uint32(wire.FileWriteData | wire.FileAppendData |
		wire.FileWriteEA | wire.FileWriteAttributes | wire.Delete)

	tests := []struct {
		name      string
		readOnly  bool
		fileAttrs uint32
		want      uint32
	}{
		{
			name: "可写共享上的普通文件 → 全权限",
			want: wire.MaximalAccessReadWrite,
		},
		{
			name:     "只读共享 → 去掉所有写位",
			readOnly: true,
			want:     wire.MaximalAccessReadOnly,
		},
		{
			// 宿主文件属主写位被清掉时 vfs 会置 DOS 只读位，
			// 此时写数据一定 EACCES —— 报可写就是撒谎。
			name:      "可写共享 + DOS 只读文件 → 剥掉写位",
			fileAttrs: vfs.FileAttributeReadonly,
			want:      wire.MaximalAccessReadWrite &^ writeBits,
		},
		{
			// 目录的只读位在 Windows 上是「自定义文件夹」标记，
			// 不表示不可写，不能据此剥权限。
			name:      "目录带只读位 → 不剥",
			fileAttrs: vfs.FileAttributeReadonly | vfs.FileAttributeDirectory,
			want:      wire.MaximalAccessReadWrite,
		},
		{
			name:      "只读共享 + 只读文件 → 仍是只读掩码",
			readOnly:  true,
			fileAttrs: vfs.FileAttributeReadonly,
			want:      wire.MaximalAccessReadOnly,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tree := &Tree{Share: &Share{Name: "s", Type: wire.ShareTypeDisk, ReadOnly: tc.readOnly}}
			got := mxAcMaximalAccess(tree, &vfs.Attr{FileAttributes: tc.fileAttrs})
			if got != tc.want {
				t.Errorf("MaximalAccess = %#08x, 期望 %#08x", got, tc.want)
			}
			// 无论如何都不能把写位报给只读共享。
			if tc.readOnly && got&writeBits != 0 {
				t.Errorf("只读共享回了写位: %#08x", got&writeBits)
			}
		})
	}
}

// TestCreateResponseContextsMxAc 做响应侧的字节级比对，走注册表完整路径。
func TestCreateResponseContextsMxAc(t *testing.T) {
	const mtime = uint64(0x01d0_1111_2222_3333)
	attr := &vfs.Attr{
		FileID:    0x42,
		WriteTime: vfs.FiletimeToTime(mtime),
	}

	// 只读共享：mxAcMaximalAccess 应回 MaximalAccessReadOnly。
	newCtx := func() *Context {
		share := &Share{Name: "s", Type: wire.ShareTypeDisk, ReadOnly: true}
		return &Context{
			Tree: &Tree{Share: share},
			Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	// 造一条带给定 Timestamp 的 MxAc 请求 context（Data 为 8 字节 FILETIME）。
	mxAcReq := func(ts uint64) *wire.CreateRequest {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint64(data, ts)
		return &wire.CreateRequest{Contexts: []wire.CreateContext{
			{Name: wire.CreateContextMxAc, Data: data},
		}}
	}
	respond := func(ctx *Context, req *wire.CreateRequest) []wire.CreateContext {
		t.Helper()
		cc, err := newCreateContexts(ctx, req)
		if err != nil {
			t.Fatalf("newCreateContexts: %v", err)
		}
		resp := &wire.CreateResponse{}
		if err := cc.respond(ctx, &Open{}, resp, attr); err != nil {
			t.Fatalf("respond: %v", err)
		}
		return resp.Contexts
	}

	// mtime 与请求里的 Timestamp 不同 → 应当回只读 MaximalAccess。
	out := respond(newCtx(), mxAcReq(1))
	data, ok := findCreateCtx(out, wire.CreateContextMxAc)
	if !ok {
		t.Fatal("响应里没有 MxAc context")
	}
	if len(data) != 8 {
		t.Fatalf("MxAc 载荷 %d 字节, 期望 8", len(data))
	}
	if v := binary.LittleEndian.Uint32(data[0:4]); v != 0 {
		t.Errorf("QueryStatus = %#x, 期望 STATUS_SUCCESS(0)", v)
	}
	if v := binary.LittleEndian.Uint32(data[4:8]); v != wire.MaximalAccessReadOnly {
		t.Errorf("MaximalAccess = %#08x, 期望 %#08x", v, wire.MaximalAccessReadOnly)
	}

	// Timestamp 与 mtime 相同 → 不回（客户端缓存仍有效）。
	out = respond(newCtx(), mxAcReq(mtime))
	if _, ok := findCreateCtx(out, wire.CreateContextMxAc); ok {
		t.Error("mtime 未变时不应回 MxAc context")
	}

	// 客户端没请求 → 不回。
	out = respond(newCtx(), &wire.CreateRequest{})
	if _, ok := findCreateCtx(out, wire.CreateContextMxAc); ok {
		t.Error("客户端没请求时不应回 MxAc context")
	}
}

func findCreateCtx(ctxs []wire.CreateContext, name string) ([]byte, bool) {
	for _, c := range ctxs {
		if c.Name == name {
			return c.Data, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// .sparsebundle：目录上的 alternate data stream
// ---------------------------------------------------------------------------
//
// Time Machine 的备份包 `<host>.sparsebundle` 是一个**目录**，macOS 会往它
// 身上设 FinderInfo（走 `AFP_AfpInfo` 流）。这条路径以前在 vfs 层被
// 「目录不支持流」挡掉，现在放开了 —— 这个测试钉住命令层不会再把它挡回去。
//
// 依据（Samba 源码逐条核对）：`fruit_open_meta_netatalk()`(vfs_fruit.c:1455)
// 与 `fruit_streaminfo_meta_netatalk()`(:3859) 都**没有**目录判断；
// 而 `fruit_open_rsrc_adouble()`(:1561) 对目录直接 ENOENT
// （原注释「sorry, but directories don't have a resource fork」）。

// TestCreateStreamOnDirectory 走真实 handler：在目录上开 AFP_AfpInfo 流。
func TestCreateStreamOnDirectory(t *testing.T) {
	root := t.TempDir()
	const bundle = "host.sparsebundle"
	if err := os.Mkdir(filepath.Join(root, bundle), 0o755); err != nil {
		t.Fatal(err)
	}
	fs := newQueryDirTestFS(t, root)
	ctx, _ := newQueryDirTestContext(t, fs, false)

	open := createForTest(t, ctx, bundle+":AFP_AfpInfo", 0)

	// 关键断言：目标是**流**，不是目录。
	//
	// 若这里为 true，客户端会转头对这个句柄发 QUERY_DIRECTORY —— 而它
	// 其实是个 60 字节的数据流。vfs 侧靠「目录上的流句柄 Stat() 显式清掉
	// DIRECTORY 位」来保证，这条测试就是钉住那个约定。
	if open.IsDir {
		t.Error("目录上的流句柄不该被判定为目录")
	}
	if open.Stream != "AFP_AfpInfo" {
		t.Errorf("Stream = %q, 期望 AFP_AfpInfo", open.Stream)
	}

	// 流要能真的读写：写进去的 FinderInfo 必须原样读回来。
	ai := vfs.NewAfpInfo()
	ai.FinderInfo = sampleFinderInfo
	if _, err := open.Handle.WriteAt(ai.Marshal(), 0); err != nil {
		t.Fatalf("写目录上的 AFP_AfpInfo: %v", err)
	}
	buf := make([]byte, vfs.AfpInfoSize)
	if _, err := open.Handle.ReadAt(buf, 0); err != nil {
		t.Fatalf("读回: %v", err)
	}
	got, err := vfs.ParseAfpInfo(buf)
	if err != nil {
		t.Fatalf("解析 AfpInfo: %v", err)
	}
	if got.FinderInfo != sampleFinderInfo {
		t.Errorf("FinderInfo 未原样返回:\n got %x\nwant %x", got.FinderInfo, sampleFinderInfo)
	}
}

// TestCreateStreamOnDirectoryNonDirectoryFlag 覆盖 FILE_NON_DIRECTORY_FILE。
//
// 客户端打开 `dir:AFP_AfpInfo` 时目标是**流**，即使基础对象是目录，
// 带上 FILE_NON_DIRECTORY_FILE 也**不该**被拒 —— 否则 macOS 设不上
// .sparsebundle 的 FinderInfo。
func TestCreateStreamOnDirectoryNonDirectoryFlag(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "b.sparsebundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs := newQueryDirTestFS(t, root)
	ctx, _ := newQueryDirTestContext(t, fs, false)

	open := createForTest(t, ctx, "b.sparsebundle:AFP_AfpInfo", wire.FileNonDirectoryFile)
	if open.IsDir {
		t.Error("流句柄不该被判定为目录")
	}
}

// TestCreateResourceForkOnDirectoryRejected：目录没有资源派生。
//
// Samba `fruit_open_rsrc_adouble()` 对目录直接 ENOENT。放行的话
// 磁盘上会冒出一个 `._host.sparsebundle` 旁路文件，Finder 会显示重影。
func TestCreateResourceForkOnDirectoryRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs := newQueryDirTestFS(t, root)
	ctx, _ := newQueryDirTestContext(t, fs, false)

	ctx.Out = make([]byte, wire.HeaderSize)
	err := createFile(ctx, &wire.CreateRequest{
		Name:              `d:AFP_Resource`,
		DesiredAccess:     wire.Access(wire.MaximalAccessReadWrite),
		CreateDisposition: wire.FileOpenIf,
	})
	if err == nil {
		t.Error("目录上的 AFP_Resource 应当被拒绝（目录没有资源派生）")
	}
}

// createForTest 走真实 createFile，返回建立的句柄。
func createForTest(t *testing.T, ctx *Context, name string, opts wire.CreateOptions) *Open {
	t.Helper()

	ctx.Out = make([]byte, wire.HeaderSize)
	ctx.Chain = &Chain{}
	req := &wire.CreateRequest{
		Name:              name,
		DesiredAccess:     wire.Access(wire.MaximalAccessReadWrite),
		CreateDisposition: wire.FileOpenIf,
		CreateOptions:     opts,
	}
	if err := createFile(ctx, req); err != nil {
		t.Fatalf("createFile(%q): %v", name, err)
	}
	open := ctx.Chain.LastOpen
	if open == nil {
		t.Fatalf("createFile(%q) 没有建立句柄", name)
	}
	t.Cleanup(open.close)
	return open
}
