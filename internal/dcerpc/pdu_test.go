package dcerpc

import (
	"encoding/binary"
	"testing"
)

var le = binary.LittleEndian

func TestMarshalBindAckRoundTrip(t *testing.T) {
	const sec = "\x5C\x50\x49\x50\x45\x5C\x73\x72\x76\x73\x76\x63" // "\PIPE\srvsvc"
	xfer := TransferSyntax{UUID: NDR32TransferSyntax, Version: NDR32TransferVersion}
	b := MarshalBindAck(0x1234, sec, []Result{{Result: ResultAcceptance, Syntax: xfer}})

	pdu, err := ParsePDU(b)
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	if pdu.PType != PTYPEBindAck {
		t.Errorf("PType = %d, want %d", pdu.PType, PTYPEBindAck)
	}
	if pdu.CallID != 0x1234 {
		t.Errorf("CallID = %#x, want 0x1234", pdu.CallID)
	}
	if pdu.SecAddr != sec {
		t.Errorf("SecAddr = %q, want %q", pdu.SecAddr, sec)
	}
	if len(pdu.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1", len(pdu.Results))
	}
	if pdu.Results[0].Result != ResultAcceptance {
		t.Errorf("Result = %d, want acceptance(0)", pdu.Results[0].Result)
	}
	if !pdu.Results[0].Syntax.UUID.Equal(NDR32TransferSyntax) {
		t.Errorf("transfer syntax = %v, want NDR32", pdu.Results[0].Syntax.UUID)
	}
}

func TestMarshalFaultRoundTrip(t *testing.T) {
	b := MarshalFault(0x9, NCAStatusOpRangeError)
	pdu, err := ParsePDU(b)
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	if pdu.PType != PTYPEFault {
		t.Errorf("PType = %d, want fault", pdu.PType)
	}
	if pdu.Status != NCAStatusOpRangeError {
		t.Errorf("Status = %#x, want %#x", pdu.Status, NCAStatusOpRangeError)
	}
	if pdu.CallID != 0x9 {
		t.Errorf("CallID = %#x", pdu.CallID)
	}
}

func TestMarshalResponseRoundTrip(t *testing.T) {
	stub := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0x01, 0x02}
	b := MarshalResponse(0x42, stub)
	pdu, err := ParsePDU(b)
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	if pdu.PType != PTYPEResponse {
		t.Errorf("PType = %d", pdu.PType)
	}
	if len(pdu.Stub) != len(stub) || pdu.Stub[0] != 0xAA {
		t.Errorf("Stub 往返失败: %v", pdu.Stub)
	}
	if pdu.Opnum != 0 {
		t.Errorf("Opnum 应为 0（response 不含 opnum）")
	}
}

// smbclientBind 是 smbclient 4.22（Samba）连 \PIPE\srvsvc 时发出的真实 bind PDU，
// 由本服务端在管道入口抓下来（AGENTS.md §3：字节向量取自真实抓包）。
//
// 要点：两个 presentation context —— ctx 0 用 NDR32，ctx 1 用
// "bind time feature negotiation" 伪 transfer syntax。
var smbclientBind = []byte{
	// —— 公共头 ——
	0x05, 0x00, 0x0b, 0x03, 0x10, 0x00, 0x00, 0x00, // ver/ptype=bind/flags/drep(小端)
	0x74, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, // frag_length=116 auth_length=0 call_id=1
	// —— bind body ——
	0xb8, 0x10, 0xb8, 0x10, // max_xmit=4280 max_recv=4280
	0x00, 0x00, 0x00, 0x00, // assoc_group_id
	0x02, 0x00, 0x00, 0x00, // n_context_elem=2 + reserved
	// ctx 0
	0x00, 0x00, 0x01, 0x00, // p_cont_id=0, n_transfer_syn=1, reserved
	0xc8, 0x4f, 0x32, 0x4b, 0x70, 0x16, 0xd3, 0x01,
	0x12, 0x78, 0x5a, 0x47, 0xbf, 0x6e, 0xe1, 0x88, // srvsvc UUID
	0x03, 0x00, 0x00, 0x00, // if_version = 3.0
	0x04, 0x5d, 0x88, 0x8a, 0xeb, 0x1c, 0xc9, 0x11,
	0x9f, 0xe8, 0x08, 0x00, 0x2b, 0x10, 0x48, 0x60, // NDR32
	0x02, 0x00, 0x00, 0x00, // if_version = 2.0（注意是 2，不是 0x00020000）
	// ctx 1
	0x01, 0x00, 0x01, 0x00, // p_cont_id=1, n_transfer_syn=1, reserved
	0xc8, 0x4f, 0x32, 0x4b, 0x70, 0x16, 0xd3, 0x01,
	0x12, 0x78, 0x5a, 0x47, 0xbf, 0x6e, 0xe1, 0x88, // srvsvc UUID
	0x03, 0x00, 0x00, 0x00,
	0x2c, 0x1c, 0xb7, 0x6c, 0x12, 0x98, 0x40, 0x45,
	0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // bind time feature negotiation, 特性位 0x0003
	0x01, 0x00, 0x00, 0x00,
}

// TestParseBindFromCapture 用真实抓包验证 p_context_elem 的字段顺序
// （p_cont_id → n_transfer_syn → reserved → abstract_syntax → transfer_syntaxes）。
func TestParseBindFromCapture(t *testing.T) {
	srvsvcUUID := mustParseUUID("4b324fc8-1670-01d3-1278-5a47bf6ee188")

	pdu, err := ParsePDU(smbclientBind)
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	if pdu.PType != PTYPEBind || pdu.CallID != 1 {
		t.Fatalf("ptype=%d call_id=%d", pdu.PType, pdu.CallID)
	}
	if pdu.MaxXmitFrag != 4280 || pdu.MaxRecvFrag != 4280 {
		t.Errorf("max_xmit=%d max_recv=%d", pdu.MaxXmitFrag, pdu.MaxRecvFrag)
	}
	if len(pdu.ContextElems) != 2 {
		t.Fatalf("len(ContextElems) = %d, want 2", len(pdu.ContextElems))
	}
	for i, e := range pdu.ContextElems {
		if e.ContextID != uint16(i) {
			t.Errorf("ctx[%d].ContextID = %d", i, e.ContextID)
		}
		if !e.AbstractSyntaxUUID.Equal(srvsvcUUID) {
			t.Errorf("ctx[%d] abstract syntax = %v, want srvsvc", i, e.AbstractSyntaxUUID)
		}
		if e.AbstractSyntaxVer != 3 {
			t.Errorf("ctx[%d] if_version = %d, want 3", i, e.AbstractSyntaxVer)
		}
		if len(e.TransferSyntaxes) != 1 {
			t.Fatalf("ctx[%d] transfer 数 = %d", i, len(e.TransferSyntaxes))
		}
	}
	if !pdu.ContextElems[0].TransferSyntaxes[0].UUID.Equal(NDR32TransferSyntax) {
		t.Error("ctx0 应为 NDR32")
	}
	if pdu.ContextElems[0].TransferSyntaxes[0].Version != NDR32TransferVersion {
		t.Errorf("NDR32 if_version = %#x, want %#x",
			pdu.ContextElems[0].TransferSyntaxes[0].Version, NDR32TransferVersion)
	}
	flags, ok := IsBindTimeFeature(pdu.ContextElems[1].TransferSyntaxes[0].UUID)
	if !ok || flags != 0x0003 {
		t.Errorf("ctx1 应为 bind time feature negotiation, flags=%#x ok=%v", flags, ok)
	}
}

// TestNegotiateResultsFromCapture 验证 p_result_list 与 p_context_elem 一一对应：
// 少一项会让 Samba 按 frag_length 继续读并报 NT_STATUS_BUFFER_TOO_SMALL。
func TestNegotiateResultsFromCapture(t *testing.T) {
	srvsvcUUID := mustParseUUID("4b324fc8-1670-01d3-1278-5a47bf6ee188")
	pdu, err := ParsePDU(smbclientBind)
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	results, ok := NegotiateResults(pdu.ContextElems, srvsvcUUID)
	if !ok {
		t.Fatal("应至少接受一个 context")
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Result != ResultAcceptance ||
		!results[0].Syntax.UUID.Equal(NDR32TransferSyntax) ||
		results[0].Syntax.Version != 2 {
		t.Errorf("results[0] = %+v, want acceptance + NDR32 v2", results[0])
	}
	if results[1].Result != ResultNegotiateAck || results[1].Reason != 0 {
		t.Errorf("results[1] = %+v, want negotiate_ack + 特性位 0", results[1])
	}

	ack := MarshalBindAck(pdu.CallID, `\PIPE\srvsvc`, results)
	// 16 头 + (2+2+4) + (2 + 13 sec_addr + 1 对齐) + 4 + 2*24 = 92
	if len(ack) != 92 {
		t.Fatalf("bind_ack 长度 = %d, want 92", len(ack))
	}
	if got := le.Uint16(ack[8:10]); int(got) != len(ack) {
		t.Errorf("frag_length = %d, 实际 %d", got, len(ack))
	}
	// sec_addr 的 p_len 必须把结尾 NUL 算进去。
	if got := le.Uint16(ack[24:26]); got != 13 {
		t.Errorf("sec_addr p_len = %d, want 13", got)
	}
	if ack[26+12] != 0 {
		t.Error("sec_addr 必须以 NUL 结尾")
	}

	back, err := ParsePDU(ack)
	if err != nil {
		t.Fatalf("回读 bind_ack: %v", err)
	}
	if len(back.Results) != 2 {
		t.Fatalf("回读 results = %d", len(back.Results))
	}
	if back.SecAddr != `\PIPE\srvsvc` {
		t.Errorf("SecAddr = %q", back.SecAddr)
	}
}

func TestParsePDUTooShort(t *testing.T) {
	if _, err := ParsePDU([]byte{1, 2, 3}); err == nil {
		t.Error("短输入应当报错")
	}
}
