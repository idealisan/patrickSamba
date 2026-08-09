//go:build linux || darwin

package vfs

// oscap_seam_unix_test.go —— 拿**内核**当裁判，证明 filesystem_mode 真的改变了
// 数据落在哪里。
//
// # 为什么必须有这一层
//
// 「我们自己的接口读得回来」只能证明数据存在于某处，证明不了它存在于**哪里**。
// 而 portable 档的全部承诺恰恰是「完全不碰 OS 的可选能力」——
// 这句话只有一种证伪方式：写完之后直接用 listxattr 去问内核，宿主机上必须
// 一个属性都没有。
//
// 反过来 native 档必须**看得见**，否则「两档不同」就退化成「两档都走 builtin」，
// 而那种同义反复照样能让所有功能用例全绿。所以两个方向都要断言，
// 缺任何一边都是假阳性（本仓库在 encryption_required 上栽过同型的跟头）。

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// requireHostXattr 确认这台机器**能**承载「宿主落盘」类断言。
//
// 这不是在掩盖失败：宿主没有扩展属性时，native 档本来就无从谈起，
// 该场景由 portable 档的用例覆盖。而把「环境不具备」与「代码坏了」混成
// 同一个红是更糟的，排查的人会去改代码。
func requireHostXattr(t *testing.T, path string) {
	t.Helper()
	if !hostXattrSupported(path) {
		t.Skipf("宿主 %s 所在文件系统不支持扩展属性；native 落盘断言在此环境无从成立", path)
	}
}

// TestPortableWritesNothingToHostXattr 是本次接线**最硬的一条证据**。
//
// portable 档下写扩展属性与命名流，然后直接问内核：这个文件上有几个扩展属性？
// 答案必须是 0。同时我们自己的接口必须读得回来 —— 两条合起来才说明数据
// 确实落进了 builtin 的旁路存储，而不是「压根没写成功」。
func TestPortableWritesNothingToHostXattr(t *testing.T) {
	fs := newModeFS(t, oscap.ModePortable)
	writeFile(t, fs, "doc.txt", "main data")
	host := hostPath(fs, "doc.txt")
	requireHostXattr(t, host)

	// --- 1. 扩展属性
	x := fs.xattrAt(host, nil)
	if err := x.Set("com.apple.quarantine", []byte("q-value")); err != nil {
		t.Fatalf("portable 档写扩展属性: %v", err)
	}

	// --- 2. 命名流（走 CapNamedStream，落盘格式与 xattr 是两回事）
	h := openGeneric(t, fs, "doc.txt", "meta", OpenAlways)
	if _, err := h.WriteAt([]byte("STREAMDATA"), 0); err != nil {
		t.Fatalf("portable 档写命名流: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// --- 3. FinderInfo（AFP_AfpInfo，同样承载在 xattr 能力上）
	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	if err := fs.writeAfpInfo(host, ai); err != nil {
		t.Fatalf("portable 档写 FinderInfo: %v", err)
	}

	// --- 判据 A：内核视角，宿主机上一个扩展属性都不该有。
	if got := hostXattrList(t, host); len(got) != 0 {
		t.Errorf("portable 档在宿主机上留下了扩展属性 %v —— "+
			"「完全不碰 OS 可选能力」这个承诺没兑现", got)
	}

	// --- 判据 B：与 A 配对。数据必须真的还在，否则 A 可以靠「什么都没写成功」
	// 蒙混过关 —— 那样的 A 是一条永远为真的断言。
	if v, err := x.Get("com.apple.quarantine"); err != nil || string(v) != "q-value" {
		t.Errorf("portable 档读回扩展属性 = %q, err=%v；期望 q-value", v, err)
	}
	if size, ok := streamNames(t, fs, "doc.txt")[StreamName("meta")]; !ok || size != int64(len("STREAMDATA")) {
		t.Errorf("portable 档读回命名流 size=%d ok=%v；期望 10", size, ok)
	}
	if got, err := fs.readAfpInfo(host); err != nil {
		t.Errorf("portable 档读回 FinderInfo: %v", err)
	} else if !bytes.Equal(got.FinderInfo[:], ai.FinderInfo[:]) {
		t.Errorf("portable 档 FinderInfo 往返不一致")
	}
}

// TestNativeWritesToHostXattr 是上一条的**反向对照**。
//
// 没有它，「portable 下宿主属性为空」可能只是因为两个档位都走了 builtin ——
// 那种情况下 filesystem_mode 依旧是个摆设，而所有用例照样全绿。
func TestNativeWritesToHostXattr(t *testing.T) {
	fs := newModeFS(t, oscap.ModeAuto)
	writeFile(t, fs, "doc.txt", "main data")
	host := hostPath(fs, "doc.txt")
	requireHostXattr(t, host)

	if got := fs.caps.Matrix().Kind(oscap.CapXattr); got != oscap.KindNative {
		t.Fatalf("宿主支持扩展属性，auto 档却把 CapXattr 交给 %v；矩阵: %s",
			got, fs.caps.Matrix())
	}

	if err := fs.xattrAt(host, nil).Set("com.apple.quarantine", []byte("q")); err != nil {
		t.Fatalf("native 档写扩展属性: %v", err)
	}
	h := openGeneric(t, fs, "doc.txt", "meta", OpenAlways)
	if _, err := h.WriteAt([]byte("STREAMDATA"), 0); err != nil {
		t.Fatalf("native 档写命名流: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	names := hostXattrList(t, host)
	var sawAttr, sawStream bool
	for _, n := range names {
		if strings.HasSuffix(n, "com.apple.quarantine") {
			sawAttr = true
		}
		if strings.Contains(n, "DosStream.meta") {
			sawStream = true
		}
	}
	if !sawAttr {
		t.Errorf("native 档写的扩展属性在宿主机上看不到，实际属性列表 %v", names)
	}
	if !sawStream {
		t.Errorf("native 档写的命名流在宿主机上看不到 user.DosStream.meta，实际 %v", names)
	}
}

// TestNamedStreamOnDiskFormatSamba 钉住与 Samba / Netatalk 的二进制兼容。
//
// 做法是**用别人的写路径造数据**：直接 setxattr 写一个
// `user.DosStream.<名>:$DATA` = `<数据><marker=0>`，完全绕开我们自己的
// 写入代码，再用我们的读路径读出来。这样「读」与「写」不会一起长歪 ——
// 两边都用同一个错误格式时，往返用例照样是绿的。
//
// 这条断言的现实意义：用户在 Samba 与 stupidSamba 之间切换时，
// 磁盘上已有的命名流必须仍然读得出来。
func TestNamedStreamOnDiskFormatSamba(t *testing.T) {
	fs := newModeFS(t, oscap.ModeAuto)
	writeFile(t, fs, "f", "x")
	host := hostPath(fs, "f")
	requireHostXattr(t, host)

	if got := fs.caps.Matrix().Kind(oscap.CapNamedStream); got != oscap.KindNative {
		t.Fatalf("本用例校验宿主落盘格式，但命名流由 %v 承载", got)
	}

	// 1. 别人写的流，我们要读得出来（值 = 数据 + 1 字节 marker=0）。
	payload := []byte("FROM-SAMBA")
	hostXattrSet(t, host, "DosStream.legacy:$DATA", append(append([]byte{}, payload...), 0x00))

	h, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: "legacy", Flags: OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("读 Samba 既有格式的命名流失败（落盘约定改了？）: %v", err)
	}
	buf := make([]byte, 32)
	n, _ := h.ReadAt(buf, 0)
	_ = h.Close()
	if string(buf[:n]) != string(payload) {
		t.Errorf("读出 %q，期望 %q", buf[:n], payload)
	}

	// 2. 我们写的流，落盘字节必须与上面同构。
	h2 := openGeneric(t, fs, "f", "ours", OpenAlways)
	if _, err := h2.WriteAt([]byte("MINE"), 0); err != nil {
		t.Fatal(err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := hostXattrGet(t, host, "DosStream.ours:$DATA")
	if err != nil {
		t.Fatalf("我们写的流在宿主机上找不到 user.DosStream.ours:$DATA: %v", err)
	}
	if want := append([]byte("MINE"), 0x00); !bytes.Equal(raw, want) {
		t.Errorf("落盘值 = % x，期望 % x（数据 + 1 字节 marker=0）", raw, want)
	}

	// 3. Samba 的多 xattr 续存形态（marker != 0）我们读不全，
	//    必须如实报错而不是把截断的内容交出去。
	hostXattrSet(t, host, "DosStream.multi:$DATA", []byte("frag\x01"))
	if _, _, err := fs.Open(&OpenRequest{
		Path: "f", Stream: "multi", Flags: OpenRead, Disposition: OpenExisting,
	}); !errors.Is(err, ErrNotSupported) {
		t.Errorf("多 xattr 续存流应报 ErrNotSupported，得到 %v", err)
	}
}

// TestFinderInfoOnDiskFormatNetatalk 钉住 FinderInfo 的落盘位置与长度。
//
// 名字必须是 user.org.netatalk.Metadata（macOS 上无前缀），值是 402 字节的
// AppleDouble blob —— 与 Samba 的 `fruit:metadata=netatalk` 默认模式一致。
// 写错了没有任何功能用例会失败，但 Netatalk / 既有 Samba 共享会读不出来。
func TestFinderInfoOnDiskFormatNetatalk(t *testing.T) {
	fs := newModeFS(t, oscap.ModeAuto)
	writeFile(t, fs, "f", "x")
	host := hostPath(fs, "f")
	requireHostXattr(t, host)

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	if err := fs.writeAfpInfo(host, ai); err != nil {
		t.Fatalf("写 FinderInfo: %v", err)
	}

	raw, err := hostXattrGet(t, host, netatalkMetaXattr)
	if err != nil {
		t.Fatalf("落盘属性 %s%s 读不到: %v", hostXattrNamespace(), netatalkMetaXattr, err)
	}
	if len(raw) != adMetaSize {
		t.Errorf("落盘 blob = %d 字节，期望 %d (AD_DATASZ_XATTR)", len(raw), adMetaSize)
	}
}

// TestPortableKeepsHostXattrInvisible 确认两档之间**互不串味**：
// native 档写下的宿主属性，不会被 portable 档当成自己的数据读出来。
//
// 这条防的是一种很隐蔽的假通过：如果 builtin 实现偷偷回落去读宿主 xattr
// （"读不到就去问问系统"），portable 的功能用例会更容易通过，而
// 「完全不碰 OS 可选能力」的承诺已经破了。
func TestPortableKeepsHostXattrInvisible(t *testing.T) {
	root := t.TempDir()
	meta := t.TempDir()
	writeHostFile(t, root, "f", "x")
	host := hostJoin(root, "f")
	requireHostXattr(t, host)

	// 用内核直接写一个宿主属性 —— 模拟「别的工具留下的数据」。
	hostXattrSet(t, host, "com.apple.quarantine", []byte("from-host"))

	fs, err := NewLocalFS(LocalConfig{
		Root: root, CaseInsensitive: true,
		FilesystemMode: oscap.ModePortable, MetadataPath: meta,
	})
	if err != nil {
		t.Fatalf("NewLocalFS(portable): %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	if v, err := fs.xattrAt(host, nil).Get("com.apple.quarantine"); !errors.Is(err, ErrNotFound) {
		t.Errorf("portable 档读到了宿主机上的扩展属性 %q (err=%v) —— "+
			"builtin 偷偷回落去读了 OS 的可选能力", v, err)
	}
	if names, err := fs.xattrAt(host, nil).List(); err != nil || len(names) != 0 {
		t.Errorf("portable 档列出了宿主机的属性 %v (err=%v)", names, err)
	}
}
