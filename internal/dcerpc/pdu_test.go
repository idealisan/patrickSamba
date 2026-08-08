package dcerpc

import (
	"encoding/binary"
	"testing"
)

var le = binary.LittleEndian

func TestMarshalBindAckRoundTrip(t *testing.T) {
	const sec = "\x5C\x50\x49\x50\x45\x5C\x73\x72\x76\x73\x76\x63" // "\PIPE\srvsvc"
	xfer := TransferSyntax{UUID: NDR32TransferSyntax, Version: NDR32TransferVersion}
	b := MarshalBindAck(0x1234, sec, xfer)

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

// TestParseBind 用一段手工构造的 bind PDU 验证解析（含接口 UUID 与 transfer syntax）。
func TestParseBind(t *testing.T) {
	srvsvcUUID := mustParseUUID("4b324fc8-1670-01d3-1278-5a47bf6ee188")
	xfer := TransferSyntax{UUID: NDR32TransferSyntax, Version: NDR32TransferVersion}

	// 构造 body。
	body := append([]byte{}, le16(5840)...) // max_xmit
	body = append(body, le16(5840)...) // max_recv
	body = append(body, le32(0)...) // assoc_group_id
	body = append(body, 1)              // n_context_elem
	body = append(body, 0, 0, 0)       // reserved(1) + reserved(2)
	body = append(body, srvsvcUUID[:]...) // abstract syntax
	body = append(body, le32(0x00030000)...) // abstract version = 3.0
	body = append(body, 1)             // n_transfer_syn
	body = append(body, 0)             // reserved
	body = append(body, xfer.UUID[:]...) // transfer syntax
	body = append(body, le32(xfer.Version)...)

	// 构造 header（小端 drep=0x10）。
	h := NewHeader(PTYPEBind, 0x55)
	h.FragLength = uint16(headerSize + len(body))
	out := make([]byte, 0, headerSize+len(body))
	out = append(out, make([]byte, headerSize)...)
	putHeader(out, h)
	le.PutUint16(out[8:10], h.FragLength)
	le.PutUint32(out[12:16], 0x55)
	out = append(out, body...)

	pdu, err := ParsePDU(out)
	if err != nil {
		t.Fatalf("ParsePDU: %v", err)
	}
	if len(pdu.ContextElems) != 1 {
		t.Fatalf("len(ContextElems) = %d, want 1", len(pdu.ContextElems))
	}
	if !pdu.ContextElems[0].AbstractSyntaxUUID.Equal(srvsvcUUID) {
		t.Errorf("abstract syntax = %v, want srvsvc", pdu.ContextElems[0].AbstractSyntaxUUID)
	}
	if len(pdu.ContextElems[0].TransferSyntaxes) != 1 ||
		!pdu.ContextElems[0].TransferSyntaxes[0].UUID.Equal(NDR32TransferSyntax) {
		t.Errorf("transfer syntax 不匹配 NDR32")
	}
}

func TestParsePDUTooShort(t *testing.T) {
	if _, err := ParsePDU([]byte{1, 2, 3}); err == nil {
		t.Error("短输入应当报错")
	}
}
