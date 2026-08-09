package vfs

// sync_test.go —— 强制刷盘语义（Handle.Sync / F_FULLFSYNC）。
//
// 「数据真的到了盘片上」在用户态测不出来（要断电才知道）。
// 能测、也值得钉住的是**调用路径**：Sync(true) 必须走到 platformFullSync，
// 且在文件/目录/AttrOnly/只读/已关闭这几类句柄上给出正确结果 ——
// 早期的实现对已关闭句柄静默返回 nil，等于对客户端撒谎说「已落盘」。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncKinds(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "hello")
	if err := os.Mkdir(filepath.Join(fs.Root(), "d"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		req  OpenRequest
	}{
		{"读写文件", OpenRequest{Path: "f", Flags: OpenRead | OpenWrite, Disposition: OpenExisting}},
		{"只读文件", OpenRequest{Path: "f", Flags: OpenRead, Disposition: OpenExisting}},
		{"目录", OpenRequest{Path: "d", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting}},
		{"AttrOnly", OpenRequest{Path: "f", Flags: OpenRead | OpenAttrOnly, Disposition: OpenExisting}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := c.req
			h, _, err := fs.Open(&req)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer h.Close()
			// 两种强度都不能报错：SMB2 FLUSH 失败会让 Time Machine
			// 直接判定备份目标不可靠并中止备份。
			if err := h.Sync(false); err != nil {
				t.Errorf("Sync(false) = %v", err)
			}
			if err := h.Sync(true); err != nil {
				t.Errorf("Sync(true) = %v", err)
			}
		})
	}
}

func TestSyncAfterClose(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "x")
	h, _, err := fs.Open(&OpenRequest{Path: "f", Flags: OpenRead, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// 对已关闭的句柄回 nil 等于谎称「已落盘」，必须报 ErrClosed。
	if err := h.Sync(true); !errors.Is(err, ErrClosed) {
		t.Errorf("已关闭句柄的 Sync(true) = %v；期望 ErrClosed", err)
	}
}

func TestSyncPersistsData(t *testing.T) {
	fs := newTestFS(t, false)
	h, _, err := fs.Open(&OpenRequest{
		Path: "band", Flags: OpenRead | OpenWrite, Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	want := []byte("time machine band payload")
	if _, err := h.WriteAt(want, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Sync(true); err != nil {
		t.Fatalf("Sync(true): %v", err)
	}

	got, err := os.ReadFile(filepath.Join(fs.Root(), "band"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("落盘内容 = %q；期望 %q", got, want)
	}
}

func TestWriteThroughSyncs(t *testing.T) {
	fs := newTestFS(t, false)
	// FILE_WRITE_THROUGH：每次 WriteAt 内部都要刷盘。
	// 这里只能验证它不报错且数据可见 —— 真正的持久性要断电才测得出。
	h, _, err := fs.Open(&OpenRequest{
		Path:        "wt",
		Flags:       OpenRead | OpenWrite | OpenWriteThrough,
		Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if _, err := h.WriteAt([]byte("abcd"), 0); err != nil {
		t.Fatalf("write-through 写入失败: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(fs.Root(), "wt"))
	if err != nil || string(got) != "abcd" {
		t.Errorf("write-through 之后读回 %q, %v；期望 \"abcd\"", got, err)
	}
}

func TestStreamSyncFull(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "base")

	// 资源派生流走 ._ 旁路文件，也必须支持强制刷盘：
	// Finder 复制带资源派生的文件时会 FLUSH 流句柄。
	h, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: "AFP_Resource",
		Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	})
	if err != nil {
		t.Fatalf("打开 AFP_Resource: %v", err)
	}
	defer h.Close()
	if _, err := h.WriteAt([]byte("rsrc"), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Sync(true); err != nil {
		t.Errorf("流句柄 Sync(true) = %v", err)
	}
}
