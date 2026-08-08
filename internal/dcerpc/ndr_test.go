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

// TestStructArrayTwoPass 校验"先固定部分、后引用部分"的两趟布局：
// 编码一个 [2]string 的容器，确认顶层 ResumeHandle referent 出现在数组 referent 之前，
// 与解码端的 FIFO 队列一致。
func TestStructArrayTwoPass(t *testing.T) {
	e := NewNdrEnc(binary.LittleEndian)
	names := []string{"SHARE_A", "SHARE_B"}
	e.Ptr(func() { // 容器（union）
		e.U32(uint32(len(names))) // EntriesRead
		e.Ptr(func() { // Buffer 数组
			e.U32(uint32(len(names))) // conformant max_count
			for _, n := range names {
				e.Ptr(func() { e.WString(n) })
			}
		})
	})
	e.Ptr(func() { e.U32(0) }) // ResumeHandle referent（顶层指针，应在数组 referent 之前）

	got := e.Bytes()

	// 解码端应按同一顺序解析。
	d := NewNdrDec(got, binary.LittleEndian)
	var decoded []string
	d.HeadPtr(func() {
		n := int(d.U32T())
		d.TailPtr(func() {
			mc := int(d.U32T())
			for i := 0; i < mc; i++ {
				d.TailPtr(func() { decoded = append(decoded, d.WStringT()) })
				_ = n
			}
		})
	})
	d.HeadPtr(func() { d.U32T() })
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(decoded) != 2 || decoded[0] != "SHARE_A" || decoded[1] != "SHARE_B" {
		t.Fatalf("解码结果 = %v, want [SHARE_A SHARE_B]", decoded)
	}
}

// TestWStringDecodeRoundTrip 反方向：解码上述 golden 再比对。
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
	d.HeadPtr(func() { s = d.WStringT() })
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s != "abc" {
		t.Fatalf("解码 = %q, want abc", s)
	}
}
