package dcerpc

import (
	"encoding/binary"
	"fmt"
)

// NdrEnc 累积 NDR32（C706 Chapter 14）编码字节。
//
// 布局要点（最反直觉处）：NDR 是"两趟"的——先写固定部分（含指针的 referent id），
// 所有指针指向的数据（referent）延迟到固定部分之后统一展开。本实现用两个缓冲：
//   main：固定部分（指针处只写一个 4 字节 referent id）
//   tail：引用部分（指针真正指向的数据，按指针"遇见顺序"追加）
// 最终经线字节 = main 紧接着 tail。
//
// 字节序：由调用方传入（DCERPC 自描述，通常小端）。
//
// 本类型与 NdrDec 一起导出，供 internal/dcerpc/srvsvc 子包构建/解析各接口的 stub。
type NdrEnc struct {
	order    binary.ByteOrder
	main     []byte
	tail     []byte
	sink     *[]byte // 当前写入目标：固定部分阶段为 main，referent 展开阶段为 tail
	nextID   uint32
	refQueue []func() // 延迟展开的 referent 队列（FIFO = 指针遇见顺序）
}

// NewNdrEnc 创建编码器，order 为 NDR 字节序（DCERPC 通常为小端）。
func NewNdrEnc(order binary.ByteOrder) *NdrEnc {
	e := &NdrEnc{order: order, nextID: 0x00020000}
	e.sink = &e.main
	return e
}

func (e *NdrEnc) align(n int) {
	s := *e.sink
	for len(s)%n != 0 {
		s = append(s, 0)
	}
	*e.sink = s
}

// U8/U16/U32/U64 把标量写入当前 sink（固定部分或 referent），并按其宽度对齐。
func (e *NdrEnc) U8(v uint8) { e.align(1); *e.sink = append(*e.sink, v) }

func (e *NdrEnc) U16(v uint16) {
	e.align(2)
	b := make([]byte, 2)
	e.order.PutUint16(b, v)
	*e.sink = append(*e.sink, b...)
}

func (e *NdrEnc) U32(v uint32) {
	e.align(4)
	b := make([]byte, 4)
	e.order.PutUint32(b, v)
	*e.sink = append(*e.sink, b...)
}

func (e *NdrEnc) U64(v uint64) {
	e.align(8)
	b := make([]byte, 8)
	e.order.PutUint64(b, v)
	*e.sink = append(*e.sink, b...)
}

// Ptr 写一个 unique pointer（C706 §14.3.4.2 / §14.3.12.3 "pointer marshalling"）。
//
// 关键：referent 数据**不立即写入**，而是入队。整段固定部分编完后，再按 FIFO 顺序
// 把 referent 展开到 tail —— 这就需要"先固定部分、后引用部分"的两趟布局：顶层指针
// 先入队，其 referent 在被展开时若又含嵌套指针，会追加到队列末尾。最终物理顺序正是
// 指针的遇见顺序（顶层 ResumeHandle 的 referent 出现在数组 referent 之前），与解码端
// 的 FIFO 队列一一对应。
// fn==nil 表示空指针，仅写 4 字节 0。
func (e *NdrEnc) Ptr(fn func()) {
	e.align(4)
	if fn == nil {
		*e.sink = append(*e.sink, 0, 0, 0, 0)
		return
	}
	id := e.nextID
	e.nextID += 4
	b := make([]byte, 4)
	e.order.PutUint32(b, id)
	*e.sink = append(*e.sink, b...)
	e.refQueue = append(e.refQueue, fn)
}

// WString 写 conformant varying wchar 字符串（C706 §14.3.4.3），用于 referent 内部。
// 布局：max_count(u32) | offset(u32) | actual_count(u32) | UTF-16LE 内容(含结尾 NUL) | 4 字节对齐填充。
func (e *NdrEnc) WString(s string) {
	n := uint32(len([]rune(s)) + 1) // 含结尾 NUL
	e.U32(n)
	e.U32(0)
	e.U32(n)
	for _, r := range s {
		e.U16(uint16(r))
	}
	e.U16(0) // 结尾 NUL
	e.align(4)
}

// Bytes 先输出固定部分（main），再按 FIFO 顺序展开所有 referent 到 tail，返回 main+tail。
func (e *NdrEnc) Bytes() []byte {
	for len(e.main)%4 != 0 {
		e.main = append(e.main, 0)
	}
	e.sink = &e.tail
	for i := 0; i < len(e.refQueue); i++ {
		e.align(4)
		e.refQueue[i]()
	}
	out := make([]byte, 0, len(e.main)+len(e.tail))
	out = append(out, e.main...)
	out = append(out, e.tail...)
	return out
}

// ---- NDR32 解码 ----

// NdrDec 对应 NdrEnc 的"两趟"布局：先读固定部分（head，偏移 0 起），指针数据在
// tail 区域（从 head 末尾起）。指针用 FIFO 队列在遇见顺序里延后解析。
type NdrDec struct {
	b       []byte
	order   binary.ByteOrder
	pos     int // head 游标
	tailPos int // referent 游标
	queue   []func()
	err     error
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

func (d *NdrDec) need(n int) bool {
	if d.pos+n > len(d.b) {
		d.setErr(fmt.Errorf("ndr: head 越界 pos=%d need=%d len=%d", d.pos, n, len(d.b)))
		return false
	}
	return true
}
func (d *NdrDec) needT(n int) bool {
	if d.tailPos+n > len(d.b) {
		d.setErr(fmt.Errorf("ndr: tail 越界 pos=%d need=%d len=%d", d.tailPos, n, len(d.b)))
		return false
	}
	return true
}

func (d *NdrDec) align(n int) {
	for d.pos%n != 0 {
		d.pos++
	}
}
func (d *NdrDec) alignT(n int) {
	for d.tailPos%n != 0 {
		d.tailPos++
	}
}

func (d *NdrDec) U8() uint8 {
	d.align(1)
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
func (d *NdrDec) U32T() uint32 {
	d.alignT(4)
	if !d.needT(4) {
		return 0
	}
	v := d.order.Uint32(d.b[d.tailPos:])
	d.tailPos += 4
	return v
}

// HeadPtr 读固定部分里的 unique pointer；非空则把 referent 消费者入队。
func (d *NdrDec) HeadPtr(fn func()) {
	d.align(4)
	if !d.need(4) {
		return
	}
	id := d.order.Uint32(d.b[d.pos:])
	d.pos += 4
	if id != 0 {
		d.queue = append(d.queue, fn)
	}
}

// TailPtr 读 referent 内部的 unique pointer（位于 tail 区域）；非空入队消费者。
func (d *NdrDec) TailPtr(fn func()) {
	d.alignT(4)
	if !d.needT(4) {
		return
	}
	id := d.order.Uint32(d.b[d.tailPos:])
	d.tailPos += 4
	if id != 0 {
		d.queue = append(d.queue, fn)
	}
}

// WStringT 在 tail 区域读 conformant varying wchar 字符串。
func (d *NdrDec) WStringT() string {
	d.alignT(4)
	if !d.needT(12) {
		return ""
	}
	maxCount := d.U32T()
	_ = d.U32T() // offset
	actual := d.U32T()
	if actual > maxCount {
		actual = maxCount
	}
	if int(actual)*2 > len(d.b)-d.tailPos {
		actual = uint32((len(d.b) - d.tailPos) / 2)
	}
	runes := make([]rune, 0, actual)
	for i := uint32(0); i < actual; i++ {
		r := d.U16T()
		runes = append(runes, rune(r))
	}
	for len(runes) > 0 && runes[len(runes)-1] == 0 {
		runes = runes[:len(runes)-1]
	}
	d.alignT(4)
	return string(runes)
}

// U16T 读 tail 区域的 16 位整数（供 referent 内的标量字段使用）。
func (d *NdrDec) U16T() uint16 {
	d.alignT(2)
	if !d.needT(2) {
		return 0
	}
	v := d.order.Uint16(d.b[d.tailPos:])
	d.tailPos += 2
	return v
}

// Run 按 FIFO 顺序解析所有 referent（含嵌套）；返回解析错误（如有）。
// 必须在读完固定部分后调用一次。
func (d *NdrDec) Run() error {
	d.tailPos = d.pos // referent 区域从固定部分末尾开始
	for i := 0; i < len(d.queue); i++ {
		if d.err != nil {
			return d.err
		}
		d.queue[i]()
	}
	return d.err
}
