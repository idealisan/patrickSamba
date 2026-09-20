package command

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/wire"
	"github.com/idealisan/patrickSamba/internal/vfs"
)

// stream_delete_test.go —— 流句柄上的 delete-on-close 粒度。
//
// Samba 对照（bh5 报告 F1）：删除按 fsp 粒度走，命名流 fsp 就是流本身。
// vfs_streams_xattr.c:1056-1110 streams_xattr_unlinkat() 对命名流只删对应
// xattr；Windows 同语义（对 ADS 句柄设 FileDispositionInformation 只删该流）。
//
// 回归背景：close.go 曾一律取 open.Path（不含流名的基础路径）执行
// fs.Remove —— 客户端删一个流会把**整个基础文件连带所有流**一起删掉，
// 数据丢失级故障。

// newStreamCloseCtx 造一个「已打开的流句柄」场景：
// 基础文件 f 存在且带内容，stream 流也已打开，可直接喂 handleClose。
func newStreamCloseCtx(t *testing.T, stream string) (ctx *Context, open *Open, root string) {
	t.Helper()

	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("base-data"), 0o644); err != nil {
		t.Fatalf("准备基础文件: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	h, _, err := fs.Open(&vfs.OpenRequest{
		Path:        "f",
		Stream:      stream,
		Flags:       vfs.OpenRead | vfs.OpenWrite,
		Disposition: vfs.OpenAlways,
	})
	if err != nil {
		t.Fatalf("vfs 打开流 %s: %v", stream, err)
	}
	// 写点内容让流真实可见（0 长度的资源派生按 Finder 语义等于不存在，
	// 流清单不会报告它 —— 那样断言「删掉了」就没有意义）。
	if n, werr := h.WriteAt([]byte("sdata"), 0); werr != nil || n != 5 {
		t.Fatalf("准备流内容: n=%d err=%v", n, werr)
	}
	if serr := h.Sync(false); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}

	share := &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}
	tree := &Tree{ID: 1, Share: share}
	open = &Open{
		Path:          "f",
		Stream:        stream,
		Handle:        h,
		Tree:          tree,
		GrantedAccess: wire.FileReadData | wire.FileWriteData | wire.Delete,
	}
	req := &wire.CloseRequest{FileID: wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}}
	ctx = &Context{
		Session: &Session{},
		Tree:    tree,
		Chain:   &Chain{},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Msg:     req.Append(make([]byte, wire.HeaderSize)),
		Out:     make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = open
	return ctx, open, root
}

// requireBaseIntact 断言基础文件还在且内容原封不动。
func requireBaseIntact(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "f"))
	if err != nil {
		t.Fatalf("基础文件竟被删除/不可读: %v", err)
	}
	if string(data) != "base-data" {
		t.Fatalf("基础文件内容被破坏: %q", data)
	}
}

// requireStreamGone 断言流已经从流清单里消失。
func requireStreamGone(t *testing.T, fs vfs.FileSystem, name string) {
	t.Helper()
	list, err := fs.Streams("f")
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	for _, si := range list {
		if si.Name == name {
			t.Fatalf("流 %s 在 CLOSE 后仍然存在: %v", name, list)
		}
	}
}

// TestSetInfoDeleteOnStreamKeepsBase：对**流句柄**做
// SET_INFO(FileDispositionInformation, delete=true) 再 CLOSE，
// 必须只删这个流，绝不能动基础文件（Samba streams_xattr_unlinkat 语义）。
func TestSetInfoDeleteOnStreamKeepsBase(t *testing.T) {
	for _, tc := range []struct {
		stream     string
		wireStream string // FileStreamInformation 里的写法
	}{
		{"AFP_Resource", ":AFP_Resource:$DATA"},
		{"Meta", ":Meta:$DATA"},
	} {
		t.Run(tc.stream, func(t *testing.T) {
			ctx, open, root := newStreamCloseCtx(t, tc.stream)

			// SET_INFO(FileDispositionInformation) delete=true，
			// FileId 全 FF 复用链上最后一个句柄（MS-SMB2 §3.3.5.2.7）。
			setReq := &wire.SetInfoRequest{
				InfoType:      wire.InfoTypeFile,
				FileInfoClass: uint8(wire.FileDispositionInformation),
				FileID:        wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)},
				Buffer:        []byte{1},
			}
			ctx.Msg, _ = setReq.Append(make([]byte, wire.HeaderSize))
			if err := handleSetInfo(ctx); err != nil {
				t.Fatalf("handleSetInfo: %v", err)
			}
			if !open.DeleteOnClose() {
				t.Fatal("SET_INFO 后 delete-on-close 标志应置位")
			}

			// CLOSE 触发实际删除。
			closeReq := &wire.CloseRequest{FileID: wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}}
			ctx.Msg = closeReq.Append(make([]byte, wire.HeaderSize))
			if err := handleClose(ctx); err != nil {
				t.Fatalf("handleClose: %v", err)
			}

			requireBaseIntact(t, root)
			requireStreamGone(t, open.Tree.FS(), tc.wireStream)
		})
	}
}

// TestCreateDeleteOnCloseStreamKeepsBase：CREATE 带 FILE_DELETE_ON_CLOSE 打开流
// （create.go 走 vfsOwnsDelete 路径），CLOSE 后流消失、基础文件健在。
//
// 回归背景：streamHandle.Close() 曾没有任何删除逻辑 —— 该标志静默无效，
// 什么都不删。修复后由 VFS 句柄在 Close 时删自己的存储；
// 这里验证命令层的让路逻辑（vfsOwnsDelete）与之衔接后端到端正确。
func TestCreateDeleteOnCloseStreamKeepsBase(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("base-data"), 0o644); err != nil {
		t.Fatalf("准备基础文件: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	// CREATE 带 FILE_DELETE_ON_CLOSE：openFlags 会把 OpenDeleteOnClose
	// 传进 vfs.OpenRequest（create.go:365）。
	h, _, err := fs.Open(&vfs.OpenRequest{
		Path:        "f",
		Stream:      "AFP_Resource",
		Flags:       vfs.OpenRead | vfs.OpenWrite | vfs.OpenDeleteOnClose,
		Disposition: vfs.OpenAlways,
	})
	if err != nil {
		t.Fatalf("vfs 打开流: %v", err)
	}
	if _, werr := h.WriteAt([]byte("sdata"), 0); werr != nil {
		t.Fatalf("准备流内容: %v", werr)
	}

	share := &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}
	tree := &Tree{ID: 1, Share: share}
	open := &Open{
		Path:          "f",
		Stream:        "AFP_Resource",
		Handle:        h,
		Tree:          tree,
		GrantedAccess: wire.FileReadData | wire.FileWriteData | wire.Delete,
	}
	// 模拟 create.go 对 FILE_DELETE_ON_CLOSE 的处理。
	open.SetDeleteOnClose(true)
	open.vfsOwnsDelete = true

	req := &wire.CloseRequest{FileID: wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}}
	ctx := &Context{
		Session: &Session{},
		Tree:    tree,
		Chain:   &Chain{},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Msg:     req.Append(make([]byte, wire.HeaderSize)),
		Out:     make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = open

	if err := handleClose(ctx); err != nil {
		t.Fatalf("handleClose: %v", err)
	}

	requireBaseIntact(t, root)
	requireStreamGone(t, fs, ":AFP_Resource:$DATA")
}
