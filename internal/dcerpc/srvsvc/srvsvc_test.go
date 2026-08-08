package srvsvc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/dcerpc"
)

type fakeLister struct{ shares []dcerpc.ShareEntry }

func (f fakeLister) Shares() []dcerpc.ShareEntry { return f.shares }

func sampleShares() []dcerpc.ShareEntry {
	return []dcerpc.ShareEntry{
		{Name: "public", Type: 0x00000000, Remark: "Public files"},
		{Name: "IPC$", Type: 0x00000003 | 0x80000000, Remark: "IPC"},
	}
}

// ---- 请求构造（测试用，与 dcerpc.MarshalRequest 配合） ----

// buildEnumAllReq 复刻 smbclient 真实发出的 NetrShareEnum 请求布局
// （见 TestDecodeEnumAllReqFromCapture 的抓包）。
func buildEnumAllReq(callID, level uint32) []byte {
	e := dcerpc.NewNdrEnc(binary.LittleEndian)
	e.Ptr(func() { e.WString("127.0.0.1") }) // ServerName
	e.U32(level)                             // SHARE_ENUM_STRUCT.Level
	e.U32(level)                             // 联合 switch
	e.Ptr(func() {                           // 容器：客户端传空
		e.U32(0)
		e.Ptr(nil)
	})
	e.U32(0xFFFFFFFF)          // PreferedMaximumLength
	e.Ptr(func() { e.U32(0) }) // ResumeHandle
	return dcerpc.MarshalRequest(callID, 0, opnumNetShareEnumAll, e.Bytes())
}

func buildGetInfoReq(callID uint32, netName string, level uint32) []byte {
	e := dcerpc.NewNdrEnc(binary.LittleEndian)
	e.Ptr(func() { e.WString("") })   // ServerName
	e.Ptr(func() { e.WString(netName) }) // NetName
	e.U32(level)
	return dcerpc.MarshalRequest(callID, 0, opnumNetShareGetInfo, e.Bytes())
}

func buildServerGetInfoReq(callID, level uint32) []byte {
	e := dcerpc.NewNdrEnc(binary.LittleEndian)
	e.Ptr(func() { e.WString("") }) // ServerName
	e.U32(level)
	return dcerpc.MarshalRequest(callID, 0, opnumNetServerGetInfo, e.Bytes())
}

// ---- 响应解码（测试用，镜像 marshal 结构） ----

func decodeEnumAllResp(t *testing.T, stub []byte) (level uint32, entries []dcerpc.ShareEntry, werr uint32) {
	t.Helper()
	d := dcerpc.NewNdrDec(stub, binary.LittleEndian)
	level = d.U32()
	if sw := d.U32(); sw != level {
		t.Fatalf("联合 switch = %d, 应等于 Level %d", sw, level)
	}
	d.Ptr(func() { // 容器
		_ = d.U32() // EntriesRead
		d.Ptr(func() {
			mc := int(d.U32())
			entries = make([]dcerpc.ShareEntry, mc)
			d.Deferred(func() {
				for i := 0; i < mc; i++ {
					idx := i // 避免闭包捕获循环变量
					d.Ptr(func() { entries[idx].Name = d.WString() })
					if level == 1 {
						entries[idx].Type = d.U32()
						d.Ptr(func() { entries[idx].Remark = d.WString() })
					}
				}
			})
		})
	})
	_ = d.U32() // TotalEntries（ref 指针，直接是值）
	d.Ptr(func() { d.U32() })
	werr = d.U32()
	if err := d.Err(); err != nil {
		t.Fatalf("decode EnumAll: %v", err)
	}
	return
}

func decodeGetInfoResp(t *testing.T, stub []byte) (present bool, e dcerpc.ShareEntry, werr uint32) {
	t.Helper()
	d := dcerpc.NewNdrDec(stub, binary.LittleEndian)
	level := d.U32()
	d.Ptr(func() {
		present = true
		d.Deferred(func() {
			d.Ptr(func() { e.Name = d.WString() })
			if level != 0 {
				e.Type = d.U32()
				d.Ptr(func() { e.Remark = d.WString() })
			}
		})
	})
	werr = d.U32()
	if err := d.Err(); err != nil {
		t.Fatalf("decode GetInfo: %v", err)
	}
	return
}

func decodeServerGetInfoResp(t *testing.T, stub []byte) (present bool, name, comment string, svType uint32, werr uint32) {
	t.Helper()
	d := dcerpc.NewNdrDec(stub, binary.LittleEndian)
	_ = d.U32() // 联合 switch
	d.Ptr(func() {
		present = true
		d.Deferred(func() {
			_ = d.U32() // platform_id
			d.Ptr(func() { name = d.WString() })
			_ = d.U32() // version_major
			_ = d.U32() // version_minor
			svType = d.U32()
			d.Ptr(func() { comment = d.WString() })
		})
	})
	werr = d.U32()
	if err := d.Err(); err != nil {
		t.Fatalf("decode ServerGetInfo: %v", err)
	}
	return
}

// realEnumAllReqStub 是 smbclient 4.22 发出的 NetrShareEnum(level=1) 请求 stub，
// 由本服务端在管道入口抓下（AGENTS.md §3：字节向量取自真实抓包）。
//
// 它同时证明了 NDR 的一个关键点：**顶层参数的指针 referent 紧跟指针内联**，
// 而不是延迟到整个 stub 末尾。
var realEnumAllReqStub = []byte{
	0x00, 0x00, 0x02, 0x00, // ServerName referent id
	0x0a, 0x00, 0x00, 0x00, // max_count = 10
	0x00, 0x00, 0x00, 0x00, // offset
	0x0a, 0x00, 0x00, 0x00, // actual_count = 10
	0x31, 0x00, 0x32, 0x00, 0x37, 0x00, 0x2e, 0x00, 0x30, 0x00,
	0x2e, 0x00, 0x30, 0x00, 0x2e, 0x00, 0x31, 0x00, 0x00, 0x00, // "127.0.0.1\0"
	0x01, 0x00, 0x00, 0x00, // SHARE_ENUM_STRUCT.Level = 1
	0x01, 0x00, 0x00, 0x00, // 联合 switch = 1
	0x04, 0x00, 0x02, 0x00, // Level1 容器 referent id
	0x00, 0x00, 0x00, 0x00, // EntriesRead = 0
	0x00, 0x00, 0x00, 0x00, // Buffer = NULL
	0xff, 0xff, 0xff, 0xff, // PreferedMaximumLength = 0xFFFFFFFF
	0x08, 0x00, 0x02, 0x00, // ResumeHandle referent id
	0x00, 0x00, 0x00, 0x00, // *ResumeHandle = 0
}

func TestDecodeEnumAllReqFromCapture(t *testing.T) {
	r, err := decodeEnumAllReq(realEnumAllReqStub)
	if err != nil {
		t.Fatalf("decodeEnumAllReq: %v", err)
	}
	if r.ServerName != "127.0.0.1" {
		t.Errorf("ServerName = %q, want 127.0.0.1", r.ServerName)
	}
	if r.Level != 1 {
		t.Errorf("Level = %d, want 1", r.Level)
	}
	if !r.HasResume || r.Resume != 0 {
		t.Errorf("ResumeHandle: has=%v v=%d", r.HasResume, r.Resume)
	}
}

// ---- 测试 ----

func TestNetShareEnumAllLevel1(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})
	resp, _ := h.Handle(buildEnumAllReq(1, 1))
	pdu, err := dcerpc.ParsePDU(resp)
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	if pdu.PType != dcerpc.PTYPEResponse {
		t.Fatalf("PType = %d, want response", pdu.PType)
	}
	level, entries, werr := decodeEnumAllResp(t, pdu.Stub)
	if werr != werrOK {
		t.Fatalf("WERROR = %#x, want OK", werr)
	}
	if level != 1 {
		t.Fatalf("level = %d", level)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Name != "public" || entries[0].Type != 0x00000000 || entries[0].Remark != "Public files" {
		t.Errorf("entry[0] 不符: %+v", entries[0])
	}
	if entries[1].Name != "IPC$" || entries[1].Type != (0x00000003|0x80000000) || entries[1].Remark != "IPC" {
		t.Errorf("entry[1] 不符: %+v", entries[1])
	}
}

func TestNetShareEnumAllLevel0(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})
	resp, _ := h.Handle(buildEnumAllReq(2, 0))
	pdu, _ := dcerpc.ParsePDU(resp)
	level, entries, werr := decodeEnumAllResp(t, pdu.Stub)
	if werr != werrOK || level != 0 || len(entries) != 2 {
		t.Fatalf("level=%d werr=%#x entries=%d", level, werr, len(entries))
	}
	if entries[0].Name != "public" || entries[0].Type != 0 || entries[0].Remark != "" {
		t.Errorf("level0 应只有名称: %+v", entries[0])
	}
}

func TestNetShareEnumAllUnsupportedLevel(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})
	resp, _ := h.Handle(buildEnumAllReq(3, 2))
	pdu, _ := dcerpc.ParsePDU(resp)
	_, entries, werr := decodeEnumAllResp(t, pdu.Stub)
	if werr != werrNotSupported {
		t.Fatalf("WERROR = %#x, want WERR_NOT_SUPPORTED", werr)
	}
	if len(entries) != 0 {
		t.Fatalf("不支持 level 时 union 应为空，得到 %d 项", len(entries))
	}
}

func TestUnknownOpnumReturnsFault(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})
	req := dcerpc.MarshalRequest(9, 0, 99, []byte{0x01, 0x02})
	resp, _ := h.Handle(req)
	pdu, err := dcerpc.ParsePDU(resp)
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	if pdu.PType != dcerpc.PTYPEFault {
		t.Fatalf("PType = %d, want fault", pdu.PType)
	}
	if pdu.Status != dcerpc.NCAStatusOpRangeError {
		t.Errorf("status = %#x", pdu.Status)
	}
}

func TestNetShareGetInfo(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})

	// 找到存在的共享。
	resp, _ := h.Handle(buildGetInfoReq(4, "public", 1))
	pdu, _ := dcerpc.ParsePDU(resp)
	present, e, werr := decodeGetInfoResp(t, pdu.Stub)
	if !present || werr != werrOK {
		t.Fatalf("present=%v werr=%#x", present, werr)
	}
	if e.Name != "public" || e.Remark != "Public files" {
		t.Errorf("GetInfo 结果不符: %+v", e)
	}

	// 不存在的共享 → NERR_NetNameNotFound，union 为空。
	resp2, _ := h.Handle(buildGetInfoReq(5, "nope", 1))
	pdu2, _ := dcerpc.ParsePDU(resp2)
	present2, _, werr2 := decodeGetInfoResp(t, pdu2.Stub)
	if present2 || werr2 != nerrNetNameNotFound {
		t.Errorf("不存在共享应回 NERR_NetNameNotFound: present=%v werr=%#x", present2, werr2)
	}
}

func TestNetServerGetInfo101(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})
	h.ServerName = "STUPIDSAMBA"
	h.ServerComment = "my server"
	resp, _ := h.Handle(buildServerGetInfoReq(6, 101))
	pdu, _ := dcerpc.ParsePDU(resp)
	present, name, comment, svType, werr := decodeServerGetInfoResp(t, pdu.Stub)
	if !present || werr != werrOK {
		t.Fatalf("present=%v werr=%#x", present, werr)
	}
	if name != "STUPIDSAMBA" {
		t.Errorf("name = %q", name)
	}
	if comment != "my server" {
		t.Errorf("comment = %q", comment)
	}
	if svType != 0x00800002 {
		t.Errorf("type = %#x", svType)
	}
}

func TestPipeIntegration(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})
	pipe, err := dcerpc.OpenPipe("srvsvc", h)
	if err != nil {
		t.Fatalf("OpenPipe: %v", err)
	}
	// 把完整请求写进管道，应能从 Read 拿到完整响应。
	req := buildEnumAllReq(7, 1)
	if _, err := pipe.Write(req); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := make([]byte, 4096)
	n, err := pipe.Read(out)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n == 0 {
		t.Fatal("Read 返回 0 字节")
	}
	pdu, err := dcerpc.ParsePDU(out[:n])
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	if pdu.PType != dcerpc.PTYPEResponse {
		t.Fatalf("PType = %d", pdu.PType)
	}
	_, entries, werr := decodeEnumAllResp(t, pdu.Stub)
	if werr != werrOK || len(entries) != 2 {
		t.Fatalf("经 Pipe 往返失败: werr=%#x entries=%d", werr, len(entries))
	}
}

// TestPipeReadAll 覆盖 server 层的实际用法：Write 之后用 io.ReadAll 一次读完。
// Read 在缓冲取空后必须返回 io.EOF，否则 io.ReadAll 会死循环。
func TestPipeReadAll(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})
	pipe, err := dcerpc.OpenPipe("srvsvc", h)
	if err != nil {
		t.Fatalf("OpenPipe: %v", err)
	}
	if _, err := pipe.Write(buildEnumAllReq(7, 1)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	done := make(chan []byte, 1)
	go func() {
		b, err := io.ReadAll(pipe)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
		}
		done <- b
	}()
	select {
	case b := <-done:
		if _, err := dcerpc.ParsePDU(b); err != nil {
			t.Fatalf("ReadAll 结果不是完整 PDU: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("io.ReadAll 死循环：Read 在缓冲取空后没有返回 io.EOF")
	}
}

// TestPipeTransactTruncate 验证 maxOut 截断不会丢字节：
// NetShareEnumAll 的响应通常超过客户端首个 READ 缓冲，剩余部分必须还能取到。
func TestPipeTransactTruncate(t *testing.T) {
	h := NewHandler(fakeLister{sampleShares()})
	pipe, err := dcerpc.OpenPipe("srvsvc", h)
	if err != nil {
		t.Fatalf("OpenPipe: %v", err)
	}

	// 先量一次完整响应长度。
	full, err := dcerpc.OpenPipe("srvsvc", NewHandler(fakeLister{sampleShares()}))
	if err != nil {
		t.Fatalf("OpenPipe: %v", err)
	}
	whole, err := full.Transact(buildEnumAllReq(7, 1), 0)
	if err != nil {
		t.Fatalf("Transact(maxOut=0): %v", err)
	}
	if len(whole) < 32 {
		t.Fatalf("完整响应过短: %d", len(whole))
	}

	// 再用一个小得多的 maxOut 分两次取。
	cut := 16
	head, err := pipe.Transact(buildEnumAllReq(7, 1), cut)
	if !errors.Is(err, dcerpc.ErrMoreData) {
		t.Fatalf("截断时应返回 ErrMoreData，得到 %v", err)
	}
	if len(head) != cut {
		t.Fatalf("首段长度 = %d, want %d", len(head), cut)
	}
	tail, err := pipe.Transact(nil, 0)
	if err != nil {
		t.Fatalf("取剩余: %v", err)
	}
	got := append(append([]byte{}, head...), tail...)
	if !bytes.Equal(got, whole) {
		t.Fatalf("截断丢字节: 拼回 %d 字节, 完整 %d 字节", len(got), len(whole))
	}
}
