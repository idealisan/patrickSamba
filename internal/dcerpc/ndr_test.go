package dcerpc

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestWStringGolden 校验 conformant varying wchar 字符串的字节布局（C706 §14.3.4.3）。
// "abc"：referent = max_count(4)=0x00000004, offset(4)=0, actual(4)=4,
// 然后 'a','b','c',NUL 的 UTF-16LE（8 字节），已 4 字节对齐。
func TestWStringGolden(t *testing.T) {
	e := NewNdrEnc(binary.LittleEndian)
	e.Ptr(func() { e.WString("abc") })
	got := e.Bytes()

	want := []byte{
		0x00, 0x00, 0x02, 0x00, // unique pointer referent id (0x00020000)
		0x04, 0x00, 0x00, 0x00, // max_count = 4
		0x00, 0x00, 0x00, 0x00, // offset = 0
		0x04, 0x00, 0x00, 0x00, // actual_count = 4
		0x61, 0x00, // 'a'
		0x62, 0x00, // 'b'
		0x63, 0x00, // 'c'
		0x00, 0x00, // NUL
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("WString 字节不符\n got=% x\nwant=% x", got, want)
	}
}

// TestPtrNull 校验空指针只写 4 字节 0。
func TestPtrNull(t *testing.T) {
	e := NewNdrEnc(binary.LittleEndian)
	e.Ptr(nil)
	got := e.Bytes()
	if !bytes.Equal(got, []byte{0, 0, 0, 0}) {
		t.Fatalf("空指针 = % x, want 00000000", got)
	}
}

// TestTopLevelPtrIsInline 是本文件最重要的一条：顶层参数的 referent 必须
// **紧跟指针内联**，不能延迟到 stub 末尾。
// 曾经因为把它当成全局延迟，导致 smbclient 解不出 srvsvc 响应。
func TestTopLevelPtrIsInline(t *testing.T) {
	e := NewNdrEnc(binary.LittleEndian)
	e.Ptr(func() { e.U32(0xAAAAAAAA) })
	e.U32(0x11111111)
	e.Ptr(func() { e.U32(0xBBBBBBBB) })

	want := []byte{
		0x00, 0x00, 0x02, 0x00, // 指针 1
		0xaa, 0xaa, 0xaa, 0xaa, // 其 referent，紧跟其后
		0x11, 0x11, 0x11, 0x11, // 中间的标量
		0x04, 0x00, 0x02, 0x00, // 指针 2
		0xbb, 0xbb, 0xbb, 0xbb, // 其 referent
	}
	got := e.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("顶层指针布局不符\n got=% x\nwant=% x", got, want)
	}

	// 解码端对称。
	d := NewNdrDec(got, binary.LittleEndian)
	var a, mid, b uint32
	d.Ptr(func() { a = d.U32() })
	mid = d.U32()
	d.Ptr(func() { b = d.U32() })
	if err := d.Err(); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if a != 0xAAAAAAAA || mid != 0x11111111 || b != 0xBBBBBBBB {
		t.Fatalf("解码 = %#x %#x %#x", a, mid, b)
	}
}

// TestDeferredArray 校验「元素含指针的 conformant 数组」布局（C706 §14.3.12.3）：
// 所有元素的固定部分先排完，字符串 referent 统一跟在整个数组之后。
func TestDeferredArray(t *testing.T) {
	e := NewNdrEnc(binary.LittleEndian)
	names := []string{"A", "B"}
	e.U32(uint32(len(names))) // max_count
	e.Deferred(func() {
		for _, n := range names {
			n := n
			e.Ptr(func() { e.WString(n) })
			e.U32(0x33)
		}
	})

	want := []byte{
		0x02, 0x00, 0x00, 0x00, // max_count = 2
		0x00, 0x00, 0x02, 0x00, // [0] name 指针
		0x33, 0x00, 0x00, 0x00, // [0] type
		0x04, 0x00, 0x02, 0x00, // [1] name 指针
		0x33, 0x00, 0x00, 0x00, // [1] type
		// 两个 referent 排在整个数组之后
		0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00,
		0x41, 0x00, 0x00, 0x00, // "A\0"
		0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00,
		0x42, 0x00, 0x00, 0x00, // "B\0"
	}
	got := e.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("数组延迟布局不符\n got=% x\nwant=% x", got, want)
	}

	d := NewNdrDec(got, binary.LittleEndian)
	mc := int(d.U32())
	decoded := make([]string, mc)
	d.Deferred(func() {
		for i := 0; i < mc; i++ {
			idx := i
			d.Ptr(func() { decoded[idx] = d.WString() })
			_ = d.U32()
		}
	})
	if err := d.Err(); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if decoded[0] != "A" || decoded[1] != "B" {
		t.Fatalf("解码 = %v", decoded)
	}
}

// TestWStringDecodeRoundTrip 反方向：解码 golden 再比对。
func TestWStringDecodeRoundTrip(t *testing.T) {
	raw := []byte{
		0x00, 0x00, 0x02, 0x00,
		0x04, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x04, 0x00, 0x00, 0x00,
		0x61, 0x00, 0x62, 0x00, 0x63, 0x00, 0x00, 0x00,
	}
	d := NewNdrDec(raw, binary.LittleEndian)
	var s string
	d.Ptr(func() { s = d.WString() })
	if err := d.Err(); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if s != "abc" {
		t.Fatalf("解码 = %q, want abc", s)
	}
}

// TestDecodeTruncated 校验越界输入只报错、不 panic（AGENTS.md §5）。
func TestDecodeTruncated(t *testing.T) {
	for _, raw := range [][]byte{
		{0x00, 0x00, 0x02},                                     // 指针都不完整
		{0x00, 0x00, 0x02, 0x00},                               // 指针有了，referent 没了
		{0x00, 0x00, 0x02, 0x00, 0xff, 0xff, 0xff, 0x7f},       // max_count 巨大
		{0x00, 0x00, 0x02, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00}, // 长度撒谎
	} {
		d := NewNdrDec(raw, binary.LittleEndian)
		var s string
		d.Ptr(func() { s = d.WString() })
		if d.Err() == nil {
			t.Errorf("输入 % x 应报错, 却解出 %q", raw, s)
		}
	}
}
