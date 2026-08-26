package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 本文件钉的是 bh3-F4 的删除面收窄版：客户端对带 FILE_ATTRIBUTE_READONLY
// 属性的目标做删除（SET_INFO(FileDispositionInformation) 与
// CREATE+FILE_DELETE_ON_CLOSE）必须被拒，回 STATUS_CANNOT_DELETE。
//
// Windows「打开时定生死」：判据是**打开时**的属性快照（open.FileAttributes，
// create.go 填充），与 #196 已修的写面（read_write.go）同一口径。
// Samba 对照：can_set_delete_on_close（source3/smbd/file_access.c:192–224）
// 对 READONLY 常规文件回 NT_STATUS_CANNOT_DELETE（`delete readonly` 默认 no）；
// 目录豁免 —— 目录的只读位语义不同（与 MxAc 的口径一致）。

// newAttrDeleteEnv 建一个可写共享与一个指向 "f" 的句柄骨架；
// hostPerm 控制宿主文件的权限位，从而控制属性快照里的 READONLY 位。
func newAttrDeleteEnv(t *testing.T, hostPerm os.FileMode) (*Tree, *Open, string) {
	t.Helper()

	root := t.TempDir()
	path := filepath.Join(root, "f")
	if err := os.WriteFile(path, []byte("keep"), 0o644); err != nil {
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
		GrantedAccess:  wire.FileReadData | wire.Delete,
		FileAttributes: wire.FileAttributes(attr.FileAttributes),
	}
	return tree, open, path
}

func dispositionBuf(deleteFlag bool) []byte {
	buf := make([]byte, wire.FileDispositionInfoSize)
	if deleteFlag {
		buf[0] = 1
	}
	return buf
}

// TestSetDispositionReadOnlyCannotDelete：READONLY 目标上置
// FileDispositionInformation 必须被拒，且文件不得被标记删除。
func TestSetDispositionReadOnlyCannotDelete(t *testing.T) {
	_, open, path := newAttrDeleteEnv(t, 0o444)

	if open.FileAttributes&wire.FileAttributeReadonly == 0 {
		t.Fatal("前置条件失败：0444 文件的属性快照应含 READONLY 位")
	}

	wantStatus(t, "对 READONLY 目标设 disposition",
		setDisposition(open, dispositionBuf(true)), status.CannotDelete)

	if open.DeleteOnClose() {
		t.Fatal("被拒的 disposition 不得置位 delete-on-close")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("目标应原样存在: %v", err)
	}

	// 清除 pending delete 永远允许（客户端可能想撤销之前的请求）。
	if err := setDisposition(open, dispositionBuf(false)); err != nil {
		t.Fatalf("清除 disposition 应成功: %v", err)
	}
}

// TestSetDispositionWritableStillAllowed：对照组 —— 没有 READONLY 位时
// 同样的请求照常成功。防止「一刀切全拒」的假修复混过来。
func TestSetDispositionWritableStillAllowed(t *testing.T) {
	_, open, path := newAttrDeleteEnv(t, 0o644)

	if open.FileAttributes&wire.FileAttributeReadonly != 0 {
		t.Fatal("前置条件失败：0644 文件的属性快照不应含 READONLY 位")
	}
	if err := setDisposition(open, dispositionBuf(true)); err != nil {
		t.Fatalf("普通目标设 disposition 应成功: %v", err)
	}
	if !open.DeleteOnClose() {
		t.Fatal("delete-on-close 应已置位")
	}
	_ = path
}

// TestSetDispositionReadOnlyDirExempt：目录的 READONLY 位不阻止删除标记
// （Samba 的 can_set_delete_on_close 对目录直接放行，非空检查另走
// STATUS_DIRECTORY_NOT_EMPTY；Windows 的目录只读位是自定义标记）。
func TestSetDispositionReadOnlyDirExempt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	h, _, err := fs.Open(&vfs.OpenRequest{
		Path: "d", Flags: vfs.OpenDirectory | vfs.OpenRead, Disposition: vfs.OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	attr, err := h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	open := &Open{
		Path:           "d",
		Handle:         h,
		IsDir:          true,
		GrantedAccess:  wire.Delete,
		FileAttributes: wire.FileAttributes(attr.FileAttributes),
	}
	if open.FileAttributes&wire.FileAttributeReadonly == 0 ||
		open.FileAttributes&wire.FileAttributeDirectory == 0 {
		t.Fatalf("前置条件失败：只读目录的快照 = %#x", open.FileAttributes)
	}
	if err := setDisposition(open, dispositionBuf(true)); err != nil {
		t.Fatalf("READONLY 目录设 disposition 应放行（目录豁免）: %v", err)
	}
}

// TestCreateDeleteOnCloseReadOnlyDenied：CREATE 带 FILE_DELETE_ON_CLOSE 打开
// READONLY 目标必须在打开之前就被拒。
//
// 为什么必须赶在 fs.Open **之前**：OpenRequest 已带上 OpenDeleteOnClose，
// 句柄一旦建立，vfs 层的关闭路径就会真的删掉文件 —— 那时再回错，
// 客户端收到 CANNOT_DELETE 而文件已经没了，比放行还糟。
func TestCreateDeleteOnCloseReadOnlyDenied(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ro.txt")
	if err := os.WriteFile(path, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	fs := newQueryDirTestFS(t, root)
	share := &Share{Name: "data", Type: wire.ShareTypeDisk, FS: fs}
	ctx := newShareTestClient(t, share)

	ctx.Out = make([]byte, wire.HeaderSize)
	ctx.Chain = &Chain{}
	err := createFile(ctx, &wire.CreateRequest{
		Name:              "ro.txt",
		DesiredAccess:     wire.FileReadData | wire.Delete,
		CreateDisposition: wire.FileOpen,
		CreateOptions:     wire.FileDeleteOnClose,
	})
	if err == nil {
		t.Fatal("对 READONLY 目标 CREATE+DELETE_ON_CLOSE 应当被拒")
	}
	if err != status.CannotDelete {
		t.Errorf("status = %v, want %v", err, status.CannotDelete)
	}
	// 关键回归点：拒绝之后文件必须还在（vfs 的 delete-on-close 不得误触发）。
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("被拒后目标丢失 —— 回滚路径误删了文件: %v", serr)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil || string(got) != "precious" {
		t.Fatalf("内容应完好，实际 %q (err=%v)", got, rerr)
	}
}

// TestCreateDeleteOnCloseWritableAllowed：对照组 —— 可写目标的同款 CREATE
// 照常成功并真正进入 delete-on-close。
func TestCreateDeleteOnCloseWritableAllowed(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rw.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newQueryDirTestFS(t, root)
	share := &Share{Name: "data", Type: wire.ShareTypeDisk, FS: fs}
	ctx := newShareTestClient(t, share)

	ctx.Out = make([]byte, wire.HeaderSize)
	ctx.Chain = &Chain{}
	if err := createFile(ctx, &wire.CreateRequest{
		Name:              "rw.txt",
		DesiredAccess:     wire.FileReadData | wire.Delete,
		CreateDisposition: wire.FileOpen,
		CreateOptions:     wire.FileDeleteOnClose,
	}); err != nil {
		t.Fatalf("可写目标应成功打开: %v", err)
	}
	o := ctx.Chain.LastOpen
	if o == nil || !o.DeleteOnClose() {
		t.Fatal("delete-on-close 应已置位")
	}
	_ = path
}
