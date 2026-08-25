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

// 本文件钉的是 bh3-F4 的写面收窄版：客户端对带 FILE_ATTRIBUTE_READONLY
// 属性的目标做 WRITE 必须被拒（Access Denied 类）。
// delete/setinfo 面不在本轮范围。

// runRW 构造一条 WRITE 请求喂给 handleWrite（open 走复合 FileId 注入）。
func runRW(t *testing.T, tree *Tree, open *Open, off uint64, data []byte) error {
	t.Helper()
	req := &wire.WriteRequest{Offset: off, FileID: compoundFID, Data: data}
	msg, err := req.Append(make([]byte, wire.HeaderSize))
	if err != nil {
		t.Fatalf("编码 WRITE Request: %v", err)
	}
	ctx := &Context{
		Conn:  &Conn{Settings: &Settings{}, MaxReadSize: rwTestMaxIO, MaxWriteSize: rwTestMaxIO},
		Chain: &Chain{},
		Tree:  tree,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Msg:   msg,
		Out:   make([]byte, wire.HeaderSize),
	}
	ctx.Chain.LastOpen = open
	return handleWrite(ctx)
}

// newAttrWriteEnv 建一个共享 + 一个指向 "f" 的写句柄；
// hostPerm 控制宿主文件的权限位，从而控制属性快照里的 READONLY 位。
func newAttrWriteEnv(t *testing.T, hostPerm os.FileMode) (*Tree, *Open, string) {
	t.Helper()

	root := t.TempDir()
	path := filepath.Join(root, "f")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
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

	// 先打开后改权限位 —— 属性允许晚于打开变化，客户端用
	// SET_INFO(FileBasicInformation) 把目标改成 READONLY 就是这个形态。
	if err := os.Chmod(path, hostPerm); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	attr, err := h.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	share := &Share{Name: "share", Type: wire.ShareTypeDisk, FS: fs}
	tree := &Tree{ID: 1, Share: share}
	open := &Open{
		Path:           "f",
		Handle:         h,
		Tree:           tree,
		GrantedAccess:  wire.FileReadData | wire.FileWriteData,
		FileAttributes: wire.FileAttributes(attr.FileAttributes),
	}
	return tree, open, path
}

// TestWriteToReadOnlyAttribDenied：READONLY 位的目标上，WRITE（含零长写）
// 必须被拒，且数据不被改动。
func TestWriteToReadOnlyAttribDenied(t *testing.T) {
	tree, open, path := newAttrWriteEnv(t, 0o444)

	if open.FileAttributes&wire.FileAttributeReadonly == 0 {
		t.Fatal("前置条件失败：0444 文件的属性快照应含 READONLY 位")
	}

	wantStatus(t, "对 READONLY 目标 WRITE",
		runRW(t, tree, open, 0, []byte("evil")), status.AccessDenied)
	wantStatus(t, "对 READONLY 目标零长 WRITE",
		runRW(t, tree, open, 0, nil), status.AccessDenied)

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("读回文件: %v", rerr)
	}
	if string(got) != "original" {
		t.Fatalf("被拒的写入不得改动数据，实际内容 %q", got)
	}
}

// TestWritableAttribFileStillWritable：对照组 —— 没有READONLY 位时同样的
// 写入照常成功。防止「一刀切全拒」的假修复混过来。
func TestWritableAttribFileStillWritable(t *testing.T) {
	tree, open, path := newAttrWriteEnv(t, 0o644)

	if open.FileAttributes&wire.FileAttributeReadonly != 0 {
		t.Fatal("前置条件失败：0644 文件的属性快照不应含 READONLY 位")
	}

	payload := []byte("replaced!")
	if err := runRW(t, tree, open, 0, payload); err != nil {
		t.Fatalf("无 READONLY 位时应正常写入: %v", err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil || !bytes.Equal(got, payload) {
		t.Fatalf("写入应生效，实际 %q (err=%v)", got, rerr)
	}
}
