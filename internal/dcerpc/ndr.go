package dcerpc

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// NDR32（C706 Chapter 14）编解码。
//
// 最容易搞错的是**指针 referent 放在哪里**。C706 §14.3.12.3 的规则是：
// referent 延迟到「所在构造（construction）的末尾」，而不是延迟到整个 stub 的末尾。
//
//   - 顶层参数里的指针，其构造就是这个参数本身 → referent **紧跟在指针之后内联**；
//   - 结构体 / 数组内部的指针，其构造是这个结构体 / 整个数组
//     → referent 排到该结构体（数组则是整个数组）的固定部分之后。
//
// 真实抓包为证（smbclient 的 NetrShareEnum 请求 stub）：
//
//	00 00 02 00                        ServerName 指针
//	0a.. 00.. 0a.. "127.0.0.1\0"       ← referent 紧跟着，没有延迟到最后
//	01 00 00 00                        Level
//	01 00 00 00                        union switch
//	04 00 02 00                        Container 指针
//	00 00 00 00  00 00 00 00           ← 其 referent 又紧跟着
//	ff ff ff ff                        PreferedMaximumLength
//	08 00 02 00                        ResumeHandle 指针
//	00 00 00 00                        ← referent 紧跟着
//
// 因此本实现的 Ptr 默认**内联**展开 referent；只有显式包在 Deferred 作用域里
// （结构体、数组）时才延迟。
//
// 字节序由调用方传入（DCERPC 自描述，实际总是小端）。
// 本类型与 NdrDec 一起导出，供 internal/dcerpc/srvsvc 子包构建/解析各接口 stub。

// ---- 编码 ----

// NdrEnc 累积 NDR32 编码字节。
type NdrEnc struct {
	order  binary.ByteOrder
	buf    []byte
	nextID uint32
	// scopes 是延迟作用域栈；非空时 Ptr 把 referent 排进栈顶队列。
	scopes []*[]func()
}

// NewNdrEnc 创建编码器，order 为 NDR 字节序（DCERPC 通常为小端）。
func NewNdrEnc(order binary.ByteOrder) *NdrEnc {
	// referent id 只要非 0 且在同一 stub 内唯一即可；从 0x00020000 起步是
	// Windows/Samba 的惯例，抓包对比时更容易看。
	return &NdrEnc{order: order, nextID: 0x00020000}
}

func (e *NdrEnc) align(n int) {
	for len(e.buf)%n != 0 {
		e.buf = append(e.buf, 0)
	}
}

// U8/U16/U32/U64 写标量，并按其宽度对齐（C706 §14.2.2）。
func (e *NdrEnc) U8(v uint8) { e.buf = append(e.buf, v) }

func (e *NdrEnc) U16(v uint16) {
	e.align(2)
	var b [2]byte
	e.order.PutUint16(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

func (e *NdrEnc) U32(v uint32) {
	e.align(4)
	var b [4]byte
	e.order.PutUint32(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

func (e *NdrEnc) U64(v uint64) {
	e.align(8)
	var b [8]byte
	e.order.PutUint64(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

// Ptr 写一个 unique pointer（C706 §14.3.4.2）。fn==nil 表示空指针，只写 4 字节 0。
//
// 不在 Deferred 作用域内时，referent 紧跟指针内联展开；
// 在 Deferred 作用域内时，referent 排队到该作用域末尾统一展开。
func (e *NdrEnc) Ptr(fn func()) {
	e.align(4)
	if fn == nil {
		e.buf = append(e.buf, 0, 0, 0, 0)
		return
	}
	var b [4]byte
	e.order.PutUint32(b[:], e.nextID)
	e.buf = append(e.buf, b[:]...)
	e.nextID += 4

	if n := len(e.scopes); n > 0 {
		q := e.scopes[n-1]
		*q = append(*q, fn)
		return
	}
	fn()
}

// Deferred 开启一个延迟作用域，用于结构体与「元素含指针的数组」：
// body 里 Ptr 登记的 referent 会在 body 返回后按登记顺序统一写出
// （C706 §14.3.12.3）。作用域内嵌套的结构体应再包一层 Deferred。
func (e *NdrEnc) Deferred(body func()) {
	var q []func()
	e.scopes = append(e.scopes, &q)
	body()
	e.scopes = e.scopes[:len(e.scopes)-1]
	// 展开时作用域已弹出，因此 referent 内部的指针默认内联，
	// 需要再延迟的由调用方自己包 Deferred。
	for i := 0; i < len(q); i++ {
		q[i]()
	}
}

// WString 写 conformant varying wchar 字符串（C706 §14.3.3.4）：
// max_count(u32) | offset(u32) | actual_count(u32) | UTF-16LE 内容（含结尾 NUL）。
// 计数单位是 **UTF-16 码元**，不是 rune —— 增补平面字符占两个码元。
func (e *NdrEnc) WString(s string) {
	u := utf16.Encode([]rune(s))
	n := uint32(len(u) + 1) // 含结尾 NUL
	e.U32(n)
	e.U32(0)
	e.U32(n)
	for _, c := range u {
		e.U16(c)
	}
	e.U16(0)
}

// Bytes 返回编码结果。stub 整体补齐到 4 字节边界（C706 §12.6.4.7）。
func (e *NdrEnc) Bytes() []byte {
	e.align(4)
	out := make([]byte, len(e.buf))
	copy(out, e.buf)
	return out
}

// ---- 解码 ----

// NdrDec 解析 NDR32 字节流。指针语义与 NdrEnc 对称：默认内联，
// Deferred 作用域内延迟。
//
// 所有读取都先校验长度，越界只记录错误并返回零值，**绝不 panic**
// （AGENTS.md §5：外部输入解析失败一律返回错误）。
type NdrDec struct {
	b      []byte
	order  binary.ByteOrder
	pos    int
	scopes []*[]func()
	err    error
}

// NewNdrDec 创建解码器。
func NewNdrDec(b []byte, order binary.ByteOrder) *NdrDec {
	return &NdrDec{b: b, order: order}
}

func (d *NdrDec) setErr(err error) {
	if d.err == nil {
		d.err = err
	}
}

// Err 返回第一个解析错误。
func (d *NdrDec) Err() error { return d.err }

func (d *NdrDec) need(n int) bool {
	if d.err != nil {
		return false
	}
	if d.pos < 0 || d.pos+n > len(d.b) {
		d.setErr(fmt.Errorf("ndr: 越界 pos=%d need=%d len=%d", d.pos, n, len(d.b)))
		return false
	}
	return true
}

func (d *NdrDec) align(n int) {
	for d.pos%n != 0 && d.pos < len(d.b) {
		d.pos++
	}
}

func (d *NdrDec) U8() uint8 {
	if !d.need(1) {
		return 0
	}
	v := d.b[d.pos]
	d.pos++
	return v
}

func (d *NdrDec) U16() uint16 {
	d.align(2)
	if !d.need(2) {
		return 0
	}
	v := d.order.Uint16(d.b[d.pos:])
	d.pos += 2
	return v
}

func (d *NdrDec) U32() uint32 {
	d.align(4)
	if !d.need(4) {
		return 0
	}
	v := d.order.Uint32(d.b[d.pos:])
	d.pos += 4
	return v
}

// Ptr 读 unique pointer：referent id 非 0 时消费 referent
// （不在 Deferred 作用域内则立即消费，否则排队）。
func (d *NdrDec) Ptr(fn func()) {
	d.align(4)
	if !d.need(4) {
		return
	}
	id := d.order.Uint32(d.b[d.pos:])
	d.pos += 4
	if id == 0 {
		return
	}
	if n := len(d.scopes); n > 0 {
		q := d.scopes[n-1]
		*q = append(*q, fn)
		return
	}
	fn()
}

// Deferred 与 NdrEnc.Deferred 对称。
func (d *NdrDec) Deferred(body func()) {
	var q []func()
	d.scopes = append(d.scopes, &q)
	body()
	d.scopes = d.scopes[:len(d.scopes)-1]
	for i := 0; i < len(q) && d.err == nil; i++ {
		q[i]()
	}
}

// WString 读 conformant varying wchar 字符串，去掉结尾 NUL。
func (d *NdrDec) WString() string {
	maxCount := d.U32()
	_ = d.U32() // offset
	actual := d.U32()
	if d.err != nil {
		return ""
	}
	if actual > maxCount {
		d.setErr(fmt.Errorf("ndr: 字符串 actual_count %d > max_count %d", actual, maxCount))
		return ""
	}
	// 先校验长度再切片：actual 来自网络，直接乘 2 可能溢出。
	if int64(actual)*2 > int64(len(d.b)-d.pos) {
		d.setErr(fmt.Errorf("ndr: 字符串长度 %d 超出剩余 %d 字节", actual, len(d.b)-d.pos))
		return ""
	}
	u := make([]uint16, 0, actual)
	for i := uint32(0); i < actual; i++ {
		u = append(u, d.U16())
	}
	for len(u) > 0 && u[len(u)-1] == 0 {
		u = u[:len(u)-1]
	}
	return string(utf16.Decode(u))
}
