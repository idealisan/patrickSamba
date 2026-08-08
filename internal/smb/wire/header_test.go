package wire

import (
	"bytes"
	"testing"
)

// goldenSyncHeader 是一条同步 SMB2 头，按 MS-SMB2 §2.2.1.2 的字段布局
// 逐字节铺开（每行一个字段，见注释）。它验证的是**偏移与字节序**，
// 每个字段都取了容易辨认的非对称值以暴露字节序错误。
//
// TODO: 待真实抓包验证（目前是按规范表格手工铺开，不是抓包原始字节）。
var goldenSyncHeader = []byte{
	0xFE, 'S', 'M', 'B', // ProtocolId
	0x40, 0x00, // StructureSize = 64
	0x02, 0x00, // CreditCharge = 2
	0x16, 0x00, 0x00, 0xC0, // Status = 0xC0000016 (MORE_PROCESSING_REQUIRED)
	0x01, 0x00, // Command = SESSION_SETUP
	0x21, 0x00, // CreditResponse = 33
	0x01, 0x00, 0x00, 0x00, // Flags = SERVER_TO_REDIR
	0x00, 0x00, 0x00, 0x00, // NextCommand = 0
	0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // MessageId = 3
	0x00, 0x00, 0x00, 0x00, // Reserved
	0x05, 0x00, 0x00, 0x00, // TreeId = 5
	0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, // SessionId
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // Signature[0:8]
	0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10, // Signature[8:16]
}

func TestParseHeaderGolden(t *testing.T) {
	h, err := ParseHeader(goldenSyncHeader)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.CreditCharge != 2 {
		t.Errorf("CreditCharge = %d, want 2", h.CreditCharge)
	}
	if h.Status != 0xC0000016 {
		t.Errorf("Status = %#08x, want 0xC0000016", h.Status)
	}
	if h.Command != CommandSessionSetup {
		t.Errorf("Command = %v, want SESSION_SETUP", h.Command)
	}
	if h.Credits != 33 {
		t.Errorf("Credits = %d, want 33", h.Credits)
	}
	if !h.IsResponse() {
		t.Error("IsResponse = false, want true")
	}
	if h.IsAsync() || h.IsSigned() || h.IsRelated() {
		t.Error("async/signed/related 标志不应置位")
	}
	if h.MessageID != 3 {
		t.Errorf("MessageID = %d, want 3", h.MessageID)
	}
	if h.TreeID != 5 {
		t.Errorf("TreeID = %d, want 5", h.TreeID)
	}
	if h.SessionID != 0x8877665544332211 {
		t.Errorf("SessionID = %#016x, want 0x8877665544332211", h.SessionID)
	}
	wantSig := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if h.Signature != wantSig {
		t.Errorf("Signature = %v, want %v", h.Signature, wantSig)
	}
	// ChannelSequence 是 Status 低 16 位的另一种解释（3.x 请求）。
	if h.ChannelSequence() != 0x0016 {
		t.Errorf("ChannelSequence = %#04x, want 0x0016", h.ChannelSequence())
	}

	if got := h.Append(nil); !bytes.Equal(got, goldenSyncHeader) {
		t.Errorf("Append 不等于 golden:\n got %x\nwant %x", got, goldenSyncHeader)
	}
}

func TestHeaderAsyncRoundTrip(t *testing.T) {
	h := Header{
		CreditCharge: 1,
		Status:       0x00000103, // STATUS_PENDING
		Command:      CommandCreate,
		Credits:      1,
		Flags:        FlagServerToRedir | FlagAsyncCommand | FlagSigned,
		MessageID:    0xDEADBEEF,
		AsyncID:      0x0102030405060708,
		SessionID:    0x1122334455667788,
		Signature:    [16]byte{0xFF},
	}
	b := h.Append(nil)
	if len(b) != HeaderSize {
		t.Fatalf("编码长度 = %d, want %d", len(b), HeaderSize)
	}
	// AsyncId 占 0x20..0x28，TreeId 字段不存在。
	if got := le.Uint64(b[0x20:]); got != h.AsyncID {
		t.Errorf("AsyncId 编码 = %#x, want %#x", got, h.AsyncID)
	}
	got, err := ParseHeader(b)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if got != h {
		t.Errorf("round-trip 不一致:\n got %+v\nwant %+v", got, h)
	}
}

func TestHeaderSyncRoundTrip(t *testing.T) {
	h := Header{
		CreditCharge: 8,
		Status:       0,
		Command:      CommandQueryDirectory,
		Credits:      64,
		Flags:        FlagRelatedOps | FlagDFSOperations,
		NextCommand:  0x78,
		MessageID:    1 << 40,
		Reserved:     0,
		TreeID:       0xABCD,
		SessionID:    1,
	}
	got, err := ParseHeader(h.Append(nil))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if got != h {
		t.Errorf("round-trip 不一致:\n got %+v\nwant %+v", got, h)
	}
}

func TestParseHeaderErrors(t *testing.T) {
	good := goldenSyncHeader

	t.Run("truncated", func(t *testing.T) {
		for n := 0; n < HeaderSize; n++ {
			if _, err := ParseHeader(good[:n]); err == nil {
				t.Fatalf("长度 %d 应报错", n)
			}
		}
	})

	t.Run("bad protocol id", func(t *testing.T) {
		bad := bytes.Clone(good)
		bad[0] = 0xFF
		if _, err := ParseHeader(bad); err == nil {
			t.Fatal("ProtocolId 错误应报错")
		}
	})

	t.Run("bad structure size", func(t *testing.T) {
		bad := bytes.Clone(good)
		le.PutUint16(bad[4:], 65)
		if _, err := ParseHeader(bad); err == nil {
			t.Fatal("StructureSize 错误应报错")
		}
	})
}

func TestHeaderReply(t *testing.T) {
	req := Header{
		Command:      CommandTreeConnect,
		CreditCharge: 1,
		Credits:      10,
		Flags:        FlagSigned | FlagRelatedOps | FlagDFSOperations,
		MessageID:    7,
		TreeID:       3,
		SessionID:    9,
		Signature:    [16]byte{1},
		NextCommand:  0x40,
	}
	r := req.Reply()
	if !r.IsResponse() {
		t.Error("响应头必须置 SERVER_TO_REDIR")
	}
	if r.Flags.Has(FlagSigned) || r.Flags.Has(FlagRelatedOps) {
		t.Error("Reply 不应继承 SIGNED / RELATED_OPERATIONS")
	}
	if !r.Flags.Has(FlagDFSOperations) {
		t.Error("Reply 应继承 DFS_OPERATIONS")
	}
	if r.NextCommand != 0 || r.Signature != [16]byte{} {
		t.Error("Reply 应清空 NextCommand 与 Signature")
	}
	if r.MessageID != 7 || r.TreeID != 3 || r.SessionID != 9 || r.Command != CommandTreeConnect {
		t.Errorf("Reply 字段未沿用: %+v", r)
	}
}

func TestIsSMB1SMB2(t *testing.T) {
	if !IsSMB2(goldenSyncHeader) || IsSMB1(goldenSyncHeader) {
		t.Error("SMB2 魔数识别错误")
	}
	smb1 := []byte{0xFF, 'S', 'M', 'B', 0x72}
	if !IsSMB1(smb1) || IsSMB2(smb1) {
		t.Error("SMB1 魔数识别错误")
	}
	if IsSMB1(nil) || IsSMB2([]byte{0xFE}) {
		t.Error("短缓冲区不应误判")
	}
	if !IsTransform([]byte{0xFD, 'S', 'M', 'B'}) || IsTransform(goldenSyncHeader) {
		t.Error("TRANSFORM 魔数识别错误")
	}
}

// TestHeaderPutAt 验证就地写入与 Append 产出一致（复合链回填要用）。
func TestHeaderPutAt(t *testing.T) {
	h := Header{Command: CommandCreate, Credits: 8, Flags: FlagServerToRedir,
		MessageID: 42, TreeID: 1, SessionID: 0x1122334455667788, NextCommand: 0x70}

	buf := make([]byte, HeaderSize+16)
	for i := range buf {
		buf[i] = 0xAA
	}
	if err := h.PutAt(buf); err != nil {
		t.Fatalf("PutAt: %v", err)
	}
	if got, want := buf[:HeaderSize], h.Append(nil); !bytes.Equal(got, want) {
		t.Errorf("PutAt 与 Append 结果不一致\n got=% X\nwant=% X", got, want)
	}
	for _, c := range buf[HeaderSize:] {
		if c != 0xAA {
			t.Fatal("PutAt 越界写入了 64 字节之后的数据")
		}
	}
	if err := h.PutAt(buf[:HeaderSize-1]); err == nil {
		t.Error("缓冲区不足 64 字节应报错")
	}
}
