package vfs

// afpinfo_test.go / stream / AppleDouble 的编解码测试。
//
// 断言里的字节向量与常量全部来自 Samba 源码，不是凭空编造
// （AGENTS.md §3「测试用例中的字节向量优先来自真实抓包或规范示例」）：
//   - source3/include/MacExtensions.h  AFP_INFO_SIZE / AFP_Signature / AFP_Version
//   - source3/lib/adouble.h            AD_APPLEDOUBLE_MAGIC / AD_VERSION2 / ADEDLEN_*
//   - source3/lib/adouble.c            AD_DATASZ_XATTR==402、AD_DATASZ_DOT_UND==82
//     （这两个在 Samba 里是编译期 #error 断言，等价于官方 golden 值）

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestAfpInfoRoundTrip(t *testing.T) {
	ai := NewAfpInfo()
	for i := range ai.FinderInfo {
		ai.FinderInfo[i] = byte(i + 1)
	}
	buf := ai.Marshal()

	if len(buf) != AfpInfoSize || AfpInfoSize != 0x3c {
		t.Fatalf("AfpInfo 长度 = %d, Samba AFP_INFO_SIZE = 0x3c", len(buf))
	}

	// 签名必须是 ASCII "AFP\0"，且以**大端**落盘（Samba 用 RSIVAL）。
	if !bytes.Equal(buf[0:4], []byte{'A', 'F', 'P', 0}) {
		t.Errorf("签名 = % x, want 41 46 50 00 (\"AFP\\0\")", buf[0:4])
	}
	if got := binary.BigEndian.Uint32(buf[4:8]); got != 0x00000100 {
		t.Errorf("版本 = %#08x, want 0x00000100 (Samba AFP_Version)", got)
	}
	if got := binary.BigEndian.Uint32(buf[12:16]); got != 0x80000000 {
		t.Errorf("BackupTime = %#08x, want 0x80000000 (AD_DATE_START)", got)
	}
	// Samba afpinfo_pack 只写 4 个字段，其余恒零。
	if !bytes.Equal(buf[8:12], make([]byte, 4)) {
		t.Errorf("Reserved1 应为零，得到 % x", buf[8:12])
	}
	if !bytes.Equal(buf[48:60], make([]byte, 12)) {
		t.Errorf("ProDosInfo+Reserved2 应为零，得到 % x", buf[48:60])
	}

	got, err := ParseAfpInfo(buf)
	if err != nil {
		t.Fatalf("ParseAfpInfo: %v", err)
	}
	if got.BackupTime != ai.BackupTime || got.FinderInfo != ai.FinderInfo {
		t.Errorf("往返不一致: %+v vs %+v", got, ai)
	}
}

// TestAfpInfoValidation：Samba 与 Apple 的服务器都会校验头部并拒绝坏数据，
// 我们必须照做，否则客户端会把损坏的元数据当成有效的。
func TestAfpInfoValidation(t *testing.T) {
	good := NewAfpInfo().Marshal()

	bad := append([]byte(nil), good...)
	bad[0] = 'X'
	if _, err := ParseAfpInfo(bad); !errors.Is(err, ErrBadAfpInfo) {
		t.Errorf("坏签名应被拒绝，得到 %v", err)
	}

	bad = append([]byte(nil), good...)
	binary.BigEndian.PutUint32(bad[4:8], 0x00010000) // MacExtensions.h 注释里那个过时的值
	if _, err := ParseAfpInfo(bad); !errors.Is(err, ErrBadAfpInfo) {
		t.Errorf("坏版本应被拒绝，得到 %v", err)
	}

	// 截断输入必须报错而不是 panic（AGENTS.md §5）
	for n := 0; n < AfpInfoSize; n++ {
		if _, err := ParseAfpInfo(good[:n]); !errors.Is(err, ErrBadAfpInfo) {
			t.Errorf("长度 %d 的残缺输入应被拒绝，得到 %v", n, err)
		}
	}
	if _, err := ParseAfpInfo(nil); !errors.Is(err, ErrBadAfpInfo) {
		t.Error("nil 输入应被拒绝")
	}
}

// TestAfpInfoEmptyFinderInfo 对应 Samba 的
// "delete AFP_AfpInfo by writing all 0" 用例：客户端就是靠写全零来删流的。
func TestAfpInfoEmptyFinderInfo(t *testing.T) {
	ai := NewAfpInfo()
	if !ai.EmptyFinderInfo() {
		t.Error("新建的 AfpInfo 的 FinderInfo 应为全零")
	}
	ai.FinderInfo[31] = 1
	if ai.EmptyFinderInfo() {
		t.Error("最后一个字节非零也应判定为非空")
	}
	ai.FinderInfo[31] = 0
	ai.FinderInfo[0] = 1
	if ai.EmptyFinderInfo() {
		t.Error("第一个字节非零也应判定为非空")
	}
}

// TestMetaXattrLayout 锁死 402 字节布局。
// Samba 用 `#if AD_DATASZ_XATTR != 402 #error` 保证这个值，等价于官方断言。
func TestMetaXattrLayout(t *testing.T) {
	var fi [FinderInfoSize]byte
	for i := range fi {
		fi[i] = byte(0xA0 + i)
	}
	buf := marshalMetaXattr(&fi)

	if len(buf) != 402 {
		t.Fatalf("metadata blob = %d 字节, Samba AD_DATASZ_XATTR = 402", len(buf))
	}
	if got := binary.BigEndian.Uint32(buf[0:4]); got != 0x00051607 {
		t.Errorf("magic = %#08x, want 0x00051607 (AD_APPLEDOUBLE_MAGIC)", got)
	}
	if got := binary.BigEndian.Uint32(buf[4:8]); got != 0x00020000 {
		t.Errorf("version = %#08x, want 0x00020000 (AD_VERSION2)", got)
	}
	// metadata xattr 的 filler 留零 —— ad_pack() 只在 ADOUBLE_RSRC 时写 tag。
	if !bytes.Equal(buf[8:24], make([]byte, 16)) {
		t.Errorf("metadata blob 的 filler 应为全零，得到 % x", buf[8:24])
	}
	if got := binary.BigEndian.Uint16(buf[24:26]); got != 8 {
		t.Errorf("nentries = %d, want 8 (ADEID_NUM_XATTR)", got)
	}
	// FinderInfo 落在 122，长度 32（ADEDOFF_FINDERI_XATTR）
	if !bytes.Equal(buf[122:154], fi[:]) {
		t.Errorf("FinderInfo 应在偏移 122，得到 % x", buf[122:154])
	}

	got, err := parseMetaXattr(buf)
	if err != nil {
		t.Fatalf("parseMetaXattr: %v", err)
	}
	if *got != fi {
		t.Errorf("往返不一致: % x vs % x", *got, fi)
	}
}

// TestMetaXattrEntryTable 校验 8 条 entry 的 (eid, off, len) 与
// Samba entry_order_meta_xattr[] 完全一致。
func TestMetaXattrEntryTable(t *testing.T) {
	buf := marshalMetaXattr(&[FinderInfoSize]byte{})
	want := []struct {
		eid, off, elen uint32
	}{
		{9, 122, 32},         // ADEID_FINDERI
		{4, 154, 0},          // ADEID_COMMENT
		{8, 354, 16},         // ADEID_FILEDATESI
		{14, 370, 4},         // ADEID_AFPFILEI
		{0x80444556, 374, 0}, // AD_DEV
		{0x80494E4F, 382, 0}, // AD_INO
		{0x8053594E, 390, 0}, // AD_SYN
		{0x8053567E, 398, 0}, // AD_ID
	}
	off := 26
	for i, w := range want {
		eid := binary.BigEndian.Uint32(buf[off:])
		eoff := binary.BigEndian.Uint32(buf[off+4:])
		elen := binary.BigEndian.Uint32(buf[off+8:])
		if eid != w.eid || eoff != w.off || elen != w.elen {
			t.Errorf("entry[%d] = (eid=%#x off=%d len=%d), want (eid=%#x off=%d len=%d)",
				i, eid, eoff, elen, w.eid, w.off, w.elen)
		}
		off += 12
	}
}

func TestMetaXattrRejectsGarbage(t *testing.T) {
	good := marshalMetaXattr(&[FinderInfoSize]byte{})

	bad := append([]byte(nil), good...)
	binary.BigEndian.PutUint32(bad[0:4], 0xDEADBEEF)
	if _, err := parseMetaXattr(bad); !errors.Is(err, ErrBadAppleDouble) {
		t.Errorf("坏 magic 应被拒绝，得到 %v", err)
	}

	bad = append([]byte(nil), good...)
	binary.BigEndian.PutUint32(bad[4:8], 0x00030000)
	if _, err := parseMetaXattr(bad); !errors.Is(err, ErrBadAppleDouble) {
		t.Errorf("坏 version 应被拒绝，得到 %v", err)
	}

	// nentries 撒谎成一个巨大的值，不能越界读或 panic
	bad = append([]byte(nil), good...)
	binary.BigEndian.PutUint16(bad[24:26], 0xFFFF)
	if _, err := parseMetaXattr(bad); !errors.Is(err, ErrBadAppleDouble) {
		t.Errorf("撑爆的 nentries 应被拒绝，得到 %v", err)
	}

	// entry 声称 FinderInfo 在文件尾之外，同样不能越界
	bad = append([]byte(nil), good...)
	binary.BigEndian.PutUint32(bad[26+4:], 0xFFFFFF00)
	if _, err := parseMetaXattr(bad); !errors.Is(err, ErrBadAppleDouble) {
		t.Errorf("越界的 entry offset 应被拒绝，得到 %v", err)
	}

	// 任意截断都不能 panic
	for n := 0; n < len(good); n++ {
		_, _ = parseMetaXattr(good[:n])
	}
}

// TestRsrcHeaderLayout 锁死 `._` 文件的 82 字节头。
// Samba `#if AD_DATASZ_DOT_UND != 82 #error`。
func TestRsrcHeaderLayout(t *testing.T) {
	buf := marshalRsrcHeader(nil, 12345)
	if len(buf) != 82 {
		t.Fatalf("._ 头 = %d 字节, Samba AD_DATASZ_DOT_UND = 82", len(buf))
	}
	if got := binary.BigEndian.Uint16(buf[24:26]); got != 2 {
		t.Errorf("nentries = %d, want 2 (ADEID_NUM_DOT_UND)", got)
	}
	// **这里 filler 必须是 "Netatalk        "** —— 与 metadata xattr 的唯一头部差异
	if got := string(buf[8:24]); got != "Netatalk        " {
		t.Errorf("filler = %q, want %q (AD_FILLER_TAG)", got, "Netatalk        ")
	}
	// entry[0] = FINDERI@50 len32, entry[1] = RFORK@82 len=资源长度
	if eid, off, l := binary.BigEndian.Uint32(buf[26:]), binary.BigEndian.Uint32(buf[30:]), binary.BigEndian.Uint32(buf[34:]); eid != 9 || off != 50 || l != 32 {
		t.Errorf("entry[0] = (%d,%d,%d), want (9,50,32)", eid, off, l)
	}
	if eid, off, l := binary.BigEndian.Uint32(buf[38:]), binary.BigEndian.Uint32(buf[42:]), binary.BigEndian.Uint32(buf[46:]); eid != 2 || off != 82 || l != 12345 {
		t.Errorf("entry[1] = (%d,%d,%d), want (2,82,12345)", eid, off, l)
	}

	off, n, err := parseRsrcHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if off != 82 || n != 12345 {
		t.Errorf("parseRsrcHeader = (%d,%d), want (82,12345)", off, n)
	}
}

func TestSplitStreamPath(t *testing.T) {
	cases := []struct {
		in         string
		base, name string
		wantErr    bool
	}{
		{in: "f.txt", base: "f.txt"},
		{in: "dir/f.txt", base: "dir/f.txt"},
		{in: "", base: ""},
		// 主数据流的显式写法
		{in: "f.txt::$DATA", base: "f.txt"},
		{in: "dir/f.txt::$DATA", base: "dir/f.txt"},
		// 特殊流
		{in: "f.txt:AFP_AfpInfo:$DATA", base: "f.txt", name: "AFP_AfpInfo"},
		{in: "f.txt:AFP_Resource:$DATA", base: "f.txt", name: "AFP_Resource"},
		// 类型可省略
		{in: "f.txt:AFP_AfpInfo", base: "f.txt", name: "AFP_AfpInfo"},
		// 目录流类型
		{in: "d:s:$INDEX_ALLOCATION", base: "d", name: "s"},
		// 大小写不敏感的类型后缀
		{in: "f.txt:s:$data", base: "f.txt", name: "s"},
		// 共享根上的流
		{in: ":AFP_AfpInfo:$DATA", base: "", name: "AFP_AfpInfo"},
		// 非法：多余的冒号
		{in: "f.txt:s:$DATA:extra", wantErr: true},
		// 非法：未知流类型
		{in: "f.txt:s:$BOGUS", wantErr: true},
		// 非法：空流名且无类型
		{in: "f.txt:", wantErr: true},
		// 非法：流名里带路径分隔符（否则是路径穿越）
		{in: `f.txt:..\..\evil`, wantErr: true},
		// 注意 "f.txt:../evil"：冒号**不在**最后一个分量里，所以它不是流语法，
		// 而是一个含非法字符 ':' 的普通路径，由 Resolver 拒绝。
		// 见 TestStreamPathTraversalDefenseInDepth。
		{in: "f.txt:../evil", base: "f.txt:../evil"},
	}
	for _, c := range cases {
		base, name, err := SplitStreamPath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("SplitStreamPath(%q) 应报错，得到 (%q,%q)", c.in, base, name)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitStreamPath(%q) 意外报错: %v", c.in, err)
			continue
		}
		if base != c.base || name != c.name {
			t.Errorf("SplitStreamPath(%q) = (%q,%q), want (%q,%q)", c.in, base, name, c.base, c.name)
		}
	}
}

// TestSplitStreamPathOnlyLastComponent：目录名里的冒号不该被当成流分隔符，
// 应该原样传给 Resolver 去拒绝，VFS 不越权解释。
func TestSplitStreamPathOnlyLastComponent(t *testing.T) {
	base, name, err := SplitStreamPath("a:b/c.txt")
	if err != nil {
		t.Fatalf("意外报错: %v", err)
	}
	if base != "a:b/c.txt" || name != "" {
		t.Errorf("= (%q,%q), want (\"a:b/c.txt\",\"\")", base, name)
	}
}

// TestStreamPathTraversalDefenseInDepth 证明「非最后分量的冒号」这条路
// 最终仍然被拦住 —— 分层防御，SplitStreamPath 不越权，Resolver 兜底。
func TestStreamPathTraversalDefenseInDepth(t *testing.T) {
	fs := newTestFS(t, false)
	for _, v := range []string{
		"f.txt:../evil",
		"f.txt:..",
		`f.txt:..\..\evil`,
		"a:b/c.txt",
	} {
		if _, _, err := fs.Open(&OpenRequest{
			Path: v, Flags: OpenRead, Disposition: OpenExisting,
		}); err == nil {
			t.Errorf("带冒号的穿越向量 %q 竟然打开成功", v)
		}
	}
}

func TestValidateStreamName(t *testing.T) {
	for _, ok := range []string{"AFP_AfpInfo", "AFP_Resource", "foo", "a b", "x.y"} {
		if err := ValidateStreamName(ok); err != nil {
			t.Errorf("%q 应合法: %v", ok, err)
		}
	}
	bad := []string{"", ".", "..", "a/b", `a\b`, "a:b", "a*b", "a?b", "a|b", "a<b", "a>b", "a\"b", "a\x00b", "a\x1fb"}
	for _, b := range bad {
		if err := ValidateStreamName(b); err == nil {
			t.Errorf("%q 应被拒绝", b)
		}
	}
	long := make([]byte, MaxComponentLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateStreamName(string(long)); err == nil {
		t.Error("超长流名应被拒绝")
	}
}

func TestStreamNameHelpers(t *testing.T) {
	if got := StreamName(""); got != "::$DATA" {
		t.Errorf("StreamName(\"\") = %q, want \"::$DATA\"", got)
	}
	if got := StreamName("AFP_Resource"); got != ":AFP_Resource:$DATA" {
		t.Errorf("StreamName = %q", got)
	}
	// 流名大小写不敏感：macOS 各版本拼写并不一致
	for _, n := range []string{"AFP_AfpInfo", "afp_afpinfo", "AFP_RESOURCE"} {
		if !IsAFPStream(n) {
			t.Errorf("%q 应被识别为 AFP 特殊流", n)
		}
	}
	if IsAFPStream("AFP_Other") {
		t.Error("AFP_Other 不是特殊流")
	}
	if got := canonicalStreamName("afp_afpinfo"); got != StreamAFPInfo {
		t.Errorf("canonicalStreamName = %q, want %q", got, StreamAFPInfo)
	}
	if got := canonicalStreamName("Custom"); got != "Custom" {
		t.Errorf("未知流名应原样返回，得到 %q", got)
	}
}
