package vfs

// stream_xattr_test.go —— 通用 named stream（user.DosStream.* xattr）。
//
// 这条路径是 Time Machine 验收的必经之路：macOS 的 SMB 客户端把
// com.apple.metadata:* / com.apple.TimeMachine.* 这类扩展属性**当作 ADS
// 发过来**（Samba vfs_fruit.c 文件头注释）。以前它们全部吃
// STATUS_NOT_SUPPORTED。

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openGeneric 打开一个通用流。
func openGeneric(t *testing.T, fs *LocalFS, path, stream string, d Disposition) Handle {
	t.Helper()
	h, _, err := fs.Open(&OpenRequest{
		Path: path, Stream: stream,
		Flags: OpenRead | OpenWrite, Disposition: d,
	})
	if err != nil {
		t.Fatalf("打开通用流 %s:%s: %v", path, stream, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// streamNames 把 Streams() 结果收成一个便于断言的 map。
func streamNames(t *testing.T, fs *LocalFS, p string) map[string]int64 {
	t.Helper()
	list, err := fs.Streams(p)
	if err != nil {
		t.Fatalf("Streams(%s): %v", p, err)
	}
	out := make(map[string]int64, len(list))
	for _, s := range list {
		out[s.Name] = s.Size
	}
	return out
}

// TestGenericStreamRoundTrip 是完整往返：写入 → 枚举 → 读回 → 删除。
func TestGenericStreamRoundTrip(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "doc.txt", "body")

	// macOS 真实会用的流名（Finder 的 Spotlight 注释）。
	//
	// 注意冒号写成 U+F022 而不是裸 ':' —— 这不是为了绕过校验，
	// 而是**线上真实的样子**：macOS 把 NTFS 非法字符映射到 Unicode
	// 私用区再发出来。裸冒号在 SMB 流名里是分隔符，客户端不可能发。
	//
	// 冒号是 U+F022，**不是** U+F03A：映射表是紧凑分配的，不是
	// 0xF000+字符（后者是老 SFM 方案）。出处 Samba
	// source3/lib/string_replace.c:186 的 `0x3a:0xf022`。
	// 用错这个字符，测试就测不到真实客户端会发的那个流名。
	const stream = "com.apple.metadata\uF022kMDItemFinderComment"
	payload := []byte("bplist00\x00\x01\x02hello finder")

	h := openGeneric(t, fs, "doc.txt", stream, OpenAlways)
	if n, err := h.WriteAt(payload, 0); err != nil || n != len(payload) {
		t.Fatalf("写通用流 = (%d, %v)", n, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// 枚举：主数据流 + 这个通用流都要在。写得进读不到是最坑的失败模式。
	got := streamNames(t, fs, "doc.txt")
	if _, ok := got[DefaultStreamName]; !ok {
		t.Errorf("主数据流缺失: %v", got)
	}
	want := StreamName(stream)
	if size, ok := got[want]; !ok {
		t.Fatalf("通用流 %q 未被枚举出来: %v", want, got)
	} else if size != int64(len(payload)) {
		t.Errorf("通用流大小 = %d；期望 %d", size, len(payload))
	}

	// 读回，逐字节比对。
	h2 := openGeneric(t, fs, "doc.txt", stream, OpenExisting)
	buf := make([]byte, len(payload)+8)
	n, err := h2.ReadAt(buf, 0)
	if err != nil && err.Error() != "EOF" {
		t.Fatalf("读通用流: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Errorf("读回 %q；期望 %q", buf[:n], payload)
	}

	// 删除（截到 0 长度之后流仍然存在，这是 Windows 语义；
	// 真正的删除走 SET_INFO disposition，不在 VFS 这一层）。
	if err := h2.Truncate(0); err != nil {
		t.Fatalf("Truncate(0): %v", err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}
	if size, ok := streamNames(t, fs, "doc.txt")[want]; !ok || size != 0 {
		t.Errorf("截断后流应仍存在且长度为 0，得到 size=%d ok=%v", size, ok)
	}
}

// TestGenericStreamSambaOnDiskFormat 钉住与 Samba 的二进制兼容。
//
// 落盘格式必须是 xattr 名 user.DosStream.<名>:$DATA、值为
// <数据><1 字节 marker=0>。写错了不会有任何测试失败，但用户在
// Samba 与 stupidSamba 之间切换时流会凭空消失。
func TestGenericStreamSambaOnDiskFormat(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "f", "x")

	const stream = "myattr"
	payload := []byte("VALUE")
	h := openGeneric(t, fs, "f", stream, OpenAlways)
	if _, err := h.WriteAt(payload, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// 直接从宿主机读原始 xattr，绕过我们自己的封装。
	x, err := newXattrAccessor(filepath.Join(fs.Root(), "f"), nil)
	if err != nil {
		t.Skipf("宿主机不支持 xattr: %v", err)
	}
	raw, err := x.Get("DosStream." + stream + ":$DATA")
	if err != nil {
		t.Fatalf("Samba 格式的 xattr 名读不到（落盘名字不对？）: %v", err)
	}
	wantRaw := append(append([]byte{}, payload...), 0x00)
	if !bytes.Equal(raw, wantRaw) {
		t.Errorf("落盘值 = % x；期望 % x（数据 + 1 字节 marker=0）", raw, wantRaw)
	}

	// 反过来：Samba 写的 marker!=0（多 xattr 续存）我们读不全，
	// 必须如实报 ErrNotSupported 而不是把截断的内容交出去。
	if err := x.Set("DosStream.multi:$DATA", []byte("frag\x01")); err != nil {
		t.Skipf("写探针 xattr 失败: %v", err)
	}
	if _, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: "multi", Flags: OpenRead, Disposition: OpenExisting,
	}); !errors.Is(err, ErrNotSupported) {
		t.Errorf("多 xattr 续存流应报 ErrNotSupported，得到 %v", err)
	}
}

// TestGenericStreamNameLimits 覆盖流名的长度与字符校验。
func TestGenericStreamNameLimits(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "f", "x")

	// 边界：正好到上限可用。
	okName := strings.Repeat("a", maxDosStreamNameLen)
	h, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: okName, Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	})
	if err != nil {
		t.Fatalf("长度正好为上限 (%d) 的流名应可用: %v", maxDosStreamNameLen, err)
	}
	if _, err := h.WriteAt([]byte("v"), 0); err != nil {
		t.Errorf("写上限长度的流: %v", err)
	}
	if err := h.Close(); err != nil {
		// 真写下去才知道内核认不认；认不了就说明预算算错了。
		t.Errorf("上限长度的流落盘失败，说明长度预算算大了: %v", err)
	}

	// 超一个字节就必须拒 —— **不能截断**，截断会让两个不同的流互相覆盖。
	tooLong := strings.Repeat("a", maxDosStreamNameLen+1)
	if _, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: tooLong, Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("超长流名 = %v；期望 ErrInvalidPath", err)
	}

	// 非法字符：会被拼进 xattr 名，放行就是越权写别的命名空间。
	// 不含空串 —— Stream:"" 的含义是「主数据流」，走不到通用流这条路。
	for _, bad := range []string{"a/b", `a\b`, "a\x00b", "..", ".", "a:b"} {
		if _, _, err := fs.Open(&OpenRequest{
			Path: "f", Stream: bad, Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
		}); err == nil {
			t.Errorf("非法流名 %q 竟然被接受", bad)
		}
	}

	// 私用区字符**必须全部放行**：那是 macOS 表达「非法 NTFS 字符」的
	// 正常方式，拒掉任何一个就等于拒掉一整类真实存在的流名。
	//
	// 这里把 8 个非控制字符的映射逐个钉住（Samba
	// source3/lib/string_replace.c:186 的 macos_string_replace_map），
	// 防止后来者"加固"ValidateStreamName 时顺手把它们一起禁掉 ——
	// 那会让 com.apple.metadata:* 这类流全线失效，而且现象是
	// 「Finder 注释莫名其妙保存不了」，极难联想到流名校验。
	pua := map[rune]byte{
		'\uF020': '"', '\uF021': '*', '\uF022': ':', '\uF023': '<',
		'\uF024': '>', '\uF025': '?', '\uF026': '\\', '\uF027': '|',
	}
	for r, orig := range pua {
		name := "pua" + string(r) + "x"
		h2, _, err := fs.Open(&OpenRequest{
			Path: "f", Stream: name, Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
		})
		if err != nil {
			t.Errorf("私用区字符 U+%04X（原字符 %q）的流名被拒: %v", r, orig, err)
			continue
		}
		_ = h2.Close()
	}
}

// TestGenericStreamTooLarge：超过 xattr 能装下的量要如实报错。
func TestGenericStreamTooLarge(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "f", "x")

	h := openGeneric(t, fs, "f", "big", OpenAlways)
	if _, err := h.WriteAt(make([]byte, 16), maxDosStreamSize); !errors.Is(err, ErrTooLarge) {
		t.Errorf("越过上限的写 = %v；期望 ErrTooLarge", err)
	}
	if err := h.Truncate(maxDosStreamSize + 1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("越过上限的 Truncate = %v；期望 ErrTooLarge", err)
	}
}

// TestGenericStreamNotFound：没写过的通用流报「不存在」而不是「不支持」。
//
// 这个区分很重要：NOT_SUPPORTED 会让客户端认为整个共享不支持 ADS
// 从而完全放弃使用，而我们明明是支持的。
func TestGenericStreamNotFound(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "f", "x")

	if _, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: "never.written", Flags: OpenRead, Disposition: OpenExisting,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的通用流 = %v；期望 ErrNotFound", err)
	}
}

// TestGenericStreamReadOnlyShare：只读共享不能写流。
func TestGenericStreamReadOnlyShare(t *testing.T) {
	rw := newTestFS(t, false)
	requireXattr(t, rw)

	ro, err := NewLocalFS(LocalConfig{Root: rw.Root(), ReadOnly: true, CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	writeFile(t, rw, "f", "x")

	if _, _, err := ro.Open(&OpenRequest{
		Path: "f", Stream: "s", Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("只读共享上创建通用流 = %v；期望 ErrReadOnly", err)
	}
}

// TestStreamSurvivesRename 确认流跟着基础对象走。
//
// 通用流落 xattr，xattr 是 inode 的一部分，同设备 rename 一定跟着走。
// 这条同时覆盖了 team-lead 关心的「跨设备 rename 时 xattr 能不能跟着」——
// 共享内部的 rename 走 os.Rename，跨设备时它会直接 EXDEV 失败
// （上一轮已收敛成 ErrNotSupported），**根本不会发生「文件搬过去了但
// xattr 丢了」这种数据静默损坏**。这正是用 xattr 而不是旁路文件的好处。
func TestStreamSurvivesRename(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "old.txt", "body")

	const stream = "com.apple.quarantine"
	h := openGeneric(t, fs, "old.txt", stream, OpenAlways)
	if _, err := h.WriteAt([]byte("0081;deadbeef;Safari;"), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fs.Rename("old.txt", "new.txt", false); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, ok := streamNames(t, fs, "new.txt")[StreamName(stream)]; !ok {
		t.Errorf("rename 之后通用流丢了: %v", streamNames(t, fs, "new.txt"))
	}
}

// TestStreamGoneWithFile 确认删文件时流一起消失，不留垃圾。
func TestStreamGoneWithFile(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "tmp.txt", "x")

	h := openGeneric(t, fs, "tmp.txt", "s", OpenAlways)
	if _, err := h.WriteAt([]byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fs.Remove("tmp.txt"); err != nil {
		t.Fatal(err)
	}
	// xattr 随 inode 消失，目录里不该留下任何旁路文件
	// （这正是通用流选 xattr 而不是 ._ 文件的理由）。
	ents, err := os.ReadDir(fs.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("删除文件后残留了 %q", e.Name())
	}
}

// TestGenericStreamDegradesWithoutXattr 覆盖宿主机不支持 xattr 的降级。
//
// 不能 panic，也不能假装成功。Streams() 应当照常返回主数据流。
func TestGenericStreamDegradesWithoutXattr(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f", "x")

	// 探测宿主机到底支不支持；支持的话这条用例没什么可测的。
	x, err := newXattrAccessor(filepath.Join(fs.Root(), "f"), nil)
	if err == nil {
		if err = x.Set("DosStream.probe:$DATA", []byte{0}); err == nil {
			_ = x.Remove("DosStream.probe:$DATA")
			t.Skip("宿主机支持 xattr，降级路径无从触发")
		}
	}

	// 走到这里说明宿主机确实不支持。
	got := streamNames(t, fs, "f")
	if _, ok := got[DefaultStreamName]; !ok {
		t.Errorf("不支持 xattr 时仍应报告主数据流，得到 %v", got)
	}
	if _, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: "s", Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	}); !errors.Is(err, ErrNotSupported) {
		t.Errorf("不支持 xattr 时开流 = %v；期望 ErrNotSupported", err)
	}
}
