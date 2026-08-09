package vfs

// stream_dir_test.go —— **目录**上的 alternate data stream。
//
// 为什么这是 Time Machine 的必经之路：`.sparsebundle` 本身就是一个
// **目录**，macOS 会往它上面设 com.apple.FinderInfo（= AFP_AfpInfo 流）
// 与 com.apple.metadata:* / com.apple.TimeMachine.* （= 通用流）。
// 早先 openStream 对任何目录一律 ErrNotSupported，这些全部走不通。
//
// 各流在目录上的可用性依据（Samba 源码，逐条核对过，不是猜的）：
//
//	AFP_AfpInfo   可用    fruit_open_meta_netatalk()      vfs_fruit.c:1455  无目录判断
//	              可用    fruit_streaminfo_meta_netatalk() vfs_fruit.c:3859  无目录判断
//	AFP_Resource  不可用  fruit_open_rsrc_adouble()       vfs_fruit.c:1561  对目录 errno=ENOENT
//	                      注释原话 "sorry, but directories don't have a resource fork"
//	              不列出  fruit_streaminfo_rsrc()         vfs_fruit.c:4047  对目录直接返回空
//	通用流         可用    vfs_streams_xattr.c 全文无目录判断

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// mkdirIn 在共享内建一个目录。
func mkdirIn(t *testing.T, fs *LocalFS, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(fs.Root(), filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDirAfpInfoStream：目录上的 FinderInfo 完整往返。
func TestDirAfpInfoStream(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	mkdirIn(t, fs, "Backup.sparsebundle")

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())

	h, _, err := fs.Open(&OpenRequest{
		Path: "Backup.sparsebundle", Stream: StreamAFPInfo,
		Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	})
	if err != nil {
		t.Fatalf("目录上的 AFP_AfpInfo 应可打开: %v", err)
	}
	if _, err := h.WriteAt(ai.Marshal(), 0); err != nil {
		t.Fatalf("写目录的 FinderInfo: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// 读回。
	fi, _, err := fs.AppleInfo("Backup.sparsebundle")
	if err != nil {
		t.Fatalf("AppleInfo(目录): %v", err)
	}
	if !bytes.Equal(fi[:], finderInfoPattern()) {
		t.Errorf("目录的 FinderInfo 没落对: % x", fi)
	}

	// 枚举里要有它，否则 Finder 认为目录没有 FinderInfo。
	got := streamNames(t, fs, "Backup.sparsebundle")
	if _, ok := got[StreamName(StreamAFPInfo)]; !ok {
		t.Errorf("目录的 AFP_AfpInfo 未被枚举: %v", got)
	}
	// 目录**没有**主数据流。
	if _, ok := got[DefaultStreamName]; ok {
		t.Errorf("目录不该报告主数据流: %v", got)
	}
}

// TestDirGenericStream：目录上的通用流完整往返。
func TestDirGenericStream(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	mkdirIn(t, fs, "Backup.sparsebundle")

	const stream = "com.apple.TimeMachine.SnapshotHistory.plist"
	payload := []byte("<?xml version=\"1.0\"?><plist/>")

	h := openGeneric(t, fs, "Backup.sparsebundle", stream, OpenAlways)
	if _, err := h.WriteAt(payload, 0); err != nil {
		t.Fatalf("写目录上的通用流: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	got := streamNames(t, fs, "Backup.sparsebundle")
	if size, ok := got[StreamName(stream)]; !ok {
		t.Fatalf("目录上的通用流未被枚举: %v", got)
	} else if size != int64(len(payload)) {
		t.Errorf("大小 = %d；期望 %d", size, len(payload))
	}

	h2 := openGeneric(t, fs, "Backup.sparsebundle", stream, OpenExisting)
	buf := make([]byte, len(payload))
	if n, err := h2.ReadAt(buf, 0); n != len(payload) {
		t.Fatalf("读回 n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf, payload) {
		t.Errorf("读回 %q；期望 %q", buf, payload)
	}
}

// TestDirResourceForkRejected：目录没有资源派生。
func TestDirResourceForkRejected(t *testing.T) {
	fs := newTestFS(t, false)
	mkdirIn(t, fs, "d")

	// 依据 fruit_open_rsrc_adouble()（vfs_fruit.c:1561）的 ENOENT。
	// 必须是 ErrNotFound 而不是 ErrNotSupported —— 后者会让客户端
	// 以为整个共享不支持 ADS 从而彻底放弃。
	for _, d := range []Disposition{OpenExisting, OpenAlways, CreateNew} {
		_, _, err := fs.Open(&OpenRequest{
			Path: "d", Stream: StreamAFPResource,
			Flags: OpenRead | OpenWrite, Disposition: d,
		})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("disposition=%v 目录上的 AFP_Resource = %v；期望 ErrNotFound", d, err)
		}
	}

	// 枚举里也不能出现，否则「列得出却打不开」自相矛盾。
	if _, ok := streamNames(t, fs, "d")[StreamName(StreamAFPResource)]; ok {
		t.Error("目录不该枚举出 AFP_Resource")
	}
}

// TestDirStreamNotADirectory：目录上的流句柄本身不是目录。
//
// 如果流的属性里带了 DIRECTORY 位，客户端会拿这个流句柄去发
// QUERY_DIRECTORY，得到一堆莫名其妙的错误。
func TestDirStreamNotADirectory(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	mkdirIn(t, fs, "d")

	h := openGeneric(t, fs, "d", "s", OpenAlways)
	if _, err := h.WriteAt([]byte("payload"), 0); err != nil {
		t.Fatal(err)
	}

	a, err := h.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if a.FileAttributes&FileAttributeDirectory != 0 {
		t.Errorf("流的属性不该带 DIRECTORY 位: %#x", a.FileAttributes)
	}
	if a.Size != int64(len("payload")) {
		t.Errorf("流长度 = %d；期望 %d", a.Size, len("payload"))
	}
	// 流上的 ReadDir 永远无效。
	if _, err := h.ReadDir("*", false, 10); !errors.Is(err, ErrNotDir) {
		t.Errorf("流句柄上的 ReadDir = %v；期望 ErrNotDir", err)
	}
}

// TestDirStreamsHiddenFromEnumeration 确认目录上的流不会污染目录枚举。
//
// 通用流落 xattr，本来就不会变成目录项 —— 这正是相对 `._` 旁路文件的
// 优势。钉住它，免得以后有人改成旁路文件实现时悄悄引入重影条目。
func TestDirStreamsHiddenFromEnumeration(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	mkdirIn(t, fs, "d")
	writeFile(t, fs, "d/real.txt", "x")

	h := openGeneric(t, fs, "d", "com.apple.FinderInfo.shadow", OpenAlways)
	if _, err := h.WriteAt([]byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	dh, _, err := fs.Open(&OpenRequest{
		Path: "d", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dh.Close() }()

	names := map[string]bool{}
	for {
		batch, err := dh.ReadDir("*", false, 100)
		for _, e := range batch {
			names[e.Name] = true
		}
		if err != nil {
			break
		}
	}
	delete(names, ".")
	delete(names, "..")
	if len(names) != 1 || !names["real.txt"] {
		t.Errorf("目录枚举被流污染了: %v", names)
	}
}
