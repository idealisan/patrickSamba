package wire

import (
	"bytes"
	"testing"
)

// dummyHeader 返回一个用于测试的 64 字节头，让报文体的 offset 基准正确。
func dummyHeader(cmd Command) []byte {
	return Header{Command: cmd, MessageID: 1}.Append(nil)
}

// goldenNegotiateRequest311 是一条 SMB 3.1.1 NEGOTIATE Request 的完整消息，
// 按 MS-SMB2 §2.2.3 / §2.2.3.1 的布局手工铺开：5 个方言 + 2 条 negotiate context
// （PREAUTH 与 ENCRYPTION）。重点验证 **context 之间的 8 字节对齐**。
//
// 布局推演：
//
//	头 64 + 固定部分 36 = 100 → 方言 5*2=10 → 110 → 补 2 字节到 112（8 的倍数）
//	PREAUTH  ：8 头 + 4 + 2(算法) + 32(盐) = 46 → 112+46 = 158 → 补 2 到 160
//	ENCRYPTION：8 头 + 2 + 2*2 = 14 → 160+14 = 174（最后一条不补）
//
// TODO: 待真实抓包验证（当前按规范表格构造，字段值取自 §2.2.3.1.1/§2.2.3.1.2）。
func goldenNegotiateRequest311() []byte {
	b := dummyHeader(CommandNegotiate)
	body := []byte{
		0x24, 0x00, // StructureSize = 36
		0x05, 0x00, // DialectCount = 5
		0x01, 0x00, // SecurityMode = SIGNING_ENABLED
		0x00, 0x00, // Reserved
		0x7F, 0x00, 0x00, 0x00, // Capabilities
		// ClientGuid
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
		0x70, 0x00, 0x00, 0x00, // NegotiateContextOffset = 112
		0x02, 0x00, // NegotiateContextCount = 2
		0x00, 0x00, // Reserved2
		// Dialects
		0x02, 0x02, 0x10, 0x02, 0x00, 0x03, 0x02, 0x03, 0x11, 0x03,
		0x00, 0x00, // 对齐填充到 112
		// —— PREAUTH_INTEGRITY_CAPABILITIES ——
		0x01, 0x00, // ContextType = 0x0001
		0x26, 0x00, // DataLength = 38 (2+2+2+32)
		0x00, 0x00, 0x00, 0x00, // Reserved
		0x01, 0x00, // HashAlgorithmCount = 1
		0x20, 0x00, // SaltLength = 32
		0x01, 0x00, // HashAlgorithms[0] = SHA-512
	}
	for i := 0; i < 32; i++ { // Salt
		body = append(body, byte(0xA0+i))
	}
	body = append(body,
		0x00, 0x00, // 对齐填充到 160
		// —— ENCRYPTION_CAPABILITIES ——
		0x02, 0x00, // ContextType = 0x0002
		0x06, 0x00, // DataLength = 6
		0x00, 0x00, 0x00, 0x00, // Reserved
		0x02, 0x00, // CipherCount = 2
		0x02, 0x00, // AES-128-GCM
		0x01, 0x00, // AES-128-CCM
	)
	return append(b, body...)
}

func TestParseNegotiateRequestGolden(t *testing.T) {
	msg := goldenNegotiateRequest311()
	if len(msg) != 174 {
		t.Fatalf("golden 消息长度 = %d, want 174", len(msg))
	}
	r, err := ParseNegotiateRequest(msg)
	if err != nil {
		t.Fatalf("ParseNegotiateRequest: %v", err)
	}
	want := []Dialect{SMB202, SMB210, SMB300, SMB302, SMB311}
	if len(r.Dialects) != len(want) {
		t.Fatalf("方言数 = %d, want %d", len(r.Dialects), len(want))
	}
	for i := range want {
		if r.Dialects[i] != want[i] {
			t.Errorf("Dialects[%d] = %v, want %v", i, r.Dialects[i], want[i])
		}
	}
	if r.SecurityMode != NegotiateSigningEnabled {
		t.Errorf("SecurityMode = %#x", r.SecurityMode)
	}
	if !r.HasDialect(SMB311) || r.HasDialect(SMB2Wildcard) {
		t.Error("HasDialect 判断错误")
	}
	if len(r.Contexts) != 2 {
		t.Fatalf("context 数 = %d, want 2", len(r.Contexts))
	}
	if r.Contexts[0].Type != ContextPreauthIntegrityCapabilities {
		t.Errorf("Contexts[0].Type = %#x", r.Contexts[0].Type)
	}
	p, err := ParsePreauthIntegrityCapabilities(r.Contexts[0].Data)
	if err != nil {
		t.Fatalf("ParsePreauthIntegrityCapabilities: %v", err)
	}
	if len(p.HashAlgorithms) != 1 || p.HashAlgorithms[0] != HashAlgorithmSHA512 {
		t.Errorf("HashAlgorithms = %v", p.HashAlgorithms)
	}
	if len(p.Salt) != 32 || p.Salt[0] != 0xA0 || p.Salt[31] != 0xA0+31 {
		t.Errorf("Salt 解析错误: %x", p.Salt)
	}
	if r.Contexts[1].Type != ContextEncryptionCapabilities {
		t.Errorf("Contexts[1].Type = %#x", r.Contexts[1].Type)
	}
	e, err := ParseEncryptionCapabilities(r.Contexts[1].Data)
	if err != nil {
		t.Fatalf("ParseEncryptionCapabilities: %v", err)
	}
	if len(e.Ciphers) != 2 || e.Ciphers[0] != CipherAES128GCM || e.Ciphers[1] != CipherAES128CCM {
		t.Errorf("Ciphers = %v", e.Ciphers)
	}

	// 重新编码应当与 golden 完全一致（含对齐填充）。
	out, err := r.Append(dummyHeader(CommandNegotiate))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !bytes.Equal(out, msg) {
		t.Errorf("重新编码不等于 golden:\n got %x\nwant %x", out, msg)
	}
}

func TestNegotiateRequestNoContexts(t *testing.T) {
	r := &NegotiateRequest{
		SecurityMode:    NegotiateSigningEnabled,
		Dialects:        []Dialect{SMB202, SMB210},
		ClientStartTime: 0x1122334455667788,
		ClientGUID:      [16]byte{0xAA},
	}
	msg, err := r.Append(dummyHeader(CommandNegotiate))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := ParseNegotiateRequest(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.ClientStartTime != r.ClientStartTime {
		t.Errorf("ClientStartTime = %#x, want %#x", got.ClientStartTime, r.ClientStartTime)
	}
	if len(got.Contexts) != 0 {
		t.Errorf("不应解析出 context: %v", got.Contexts)
	}
	if got.ClientGUID != r.ClientGUID {
		t.Errorf("ClientGUID = %x", got.ClientGUID)
	}
}

func TestNegotiateResponseRoundTrip(t *testing.T) {
	pre := &PreauthIntegrityCapabilities{
		HashAlgorithms: []uint16{HashAlgorithmSHA512},
		Salt:           bytes.Repeat([]byte{0x5A}, 32),
	}
	preData, err := pre.Encode()
	if err != nil {
		t.Fatal(err)
	}
	enc := &EncryptionCapabilities{Ciphers: []uint16{CipherAES128GCM}}
	encData, err := enc.Encode()
	if err != nil {
		t.Fatal(err)
	}
	sig := &SigningCapabilities{SigningAlgorithms: []uint16{SigningAlgorithmAESCMAC}}
	sigData, err := sig.Encode()
	if err != nil {
		t.Fatal(err)
	}

	r := &NegotiateResponse{
		SecurityMode:    NegotiateSigningEnabled,
		DialectRevision: SMB311,
		ServerGUID:      [16]byte{1, 2, 3},
		Capabilities:    CapLargeMTU | CapEncryption,
		MaxTransactSize: 0x00100000,
		MaxReadSize:     0x00100000,
		MaxWriteSize:    0x00100000,
		SystemTime:      0x01D8000000000000,
		SecurityBuffer:  []byte("spnego-token-xyz"), // 16 字节，非 8 的倍数则触发填充
		Contexts: []NegotiateContext{
			{Type: ContextPreauthIntegrityCapabilities, Data: preData},
			{Type: ContextEncryptionCapabilities, Data: encData},
			{Type: ContextSigningCapabilities, Data: sigData},
		},
	}
	msg, err := r.Append(dummyHeader(CommandNegotiate))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	body := msg[HeaderSize:]
	if le.Uint16(body) != 65 {
		t.Errorf("StructureSize = %d, want 65", le.Uint16(body))
	}
	if got := le.Uint16(body[6:]); got != 3 {
		t.Errorf("NegotiateContextCount = %d, want 3", got)
	}
	ctxOff := le.Uint32(body[60:])
	if ctxOff%8 != 0 {
		t.Errorf("NegotiateContextOffset = %d，必须 8 字节对齐", ctxOff)
	}

	got, err := ParseNegotiateResponse(msg)
	if err != nil {
		t.Fatalf("ParseNegotiateResponse: %v", err)
	}
	if got.DialectRevision != SMB311 || got.Capabilities != r.Capabilities ||
		got.MaxReadSize != r.MaxReadSize || got.SystemTime != r.SystemTime ||
		got.ServerGUID != r.ServerGUID {
		t.Errorf("固定字段 round-trip 失败: %+v", got)
	}
	if !bytes.Equal(got.SecurityBuffer, r.SecurityBuffer) {
		t.Errorf("SecurityBuffer = %q", got.SecurityBuffer)
	}
	if len(got.Contexts) != 3 {
		t.Fatalf("context 数 = %d, want 3", len(got.Contexts))
	}
	for i := range r.Contexts {
		if got.Contexts[i].Type != r.Contexts[i].Type ||
			!bytes.Equal(got.Contexts[i].Data, r.Contexts[i].Data) {
			t.Errorf("Contexts[%d] 不一致: %+v", i, got.Contexts[i])
		}
	}
}

// TestNegotiateContextAlignment 逐条核对 context 起点确实 8 字节对齐，
// 且对齐是相对 **SMB2 头起点** 而非报文体起点。
func TestNegotiateContextAlignment(t *testing.T) {
	// 造 3 条长度都不是 8 倍数的 context。
	ctxs := []NegotiateContext{
		{Type: ContextPreauthIntegrityCapabilities, Data: bytes.Repeat([]byte{1}, 3)},
		{Type: ContextEncryptionCapabilities, Data: bytes.Repeat([]byte{2}, 5)},
		{Type: ContextSigningCapabilities, Data: bytes.Repeat([]byte{3}, 1)},
	}
	r := &NegotiateResponse{DialectRevision: SMB311, Contexts: ctxs}
	msg, err := r.Append(dummyHeader(CommandNegotiate))
	if err != nil {
		t.Fatal(err)
	}
	off := int(le.Uint32(msg[HeaderSize+60:]))
	for i := range ctxs {
		if off%8 != 0 {
			t.Fatalf("Contexts[%d] 起点 %d 未 8 字节对齐", i, off)
		}
		dataLen := int(le.Uint16(msg[off+2:]))
		off = align8(off + negotiateContextHeaderSize + dataLen)
	}
	got, err := ParseNegotiateResponse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Contexts) != 3 {
		t.Fatalf("context 数 = %d", len(got.Contexts))
	}
	for i := range ctxs {
		if !bytes.Equal(got.Contexts[i].Data, ctxs[i].Data) {
			t.Errorf("Contexts[%d].Data = %x, want %x", i, got.Contexts[i].Data, ctxs[i].Data)
		}
	}
}

func TestNegotiateParseTruncated(t *testing.T) {
	msg := goldenNegotiateRequest311()
	for n := 0; n < len(msg); n++ {
		// 任何截断都必须返回 error 而不是 panic。
		if _, err := ParseNegotiateRequest(msg[:n]); err == nil {
			t.Fatalf("截断到 %d 字节应报错", n)
		}
	}
}

func TestNegotiateContextTruncated(t *testing.T) {
	// DataLength 撒谎（声称 0xFFFF）必须被拒绝而不是 panic。
	msg := goldenNegotiateRequest311()
	off := int(le.Uint32(msg[HeaderSize+28:]))
	le.PutUint16(msg[off+2:], 0xFFFF)
	if _, err := ParseNegotiateRequest(msg); err == nil {
		t.Fatal("越界的 DataLength 应报错")
	}
}

func TestCompressionAndNetnameContext(t *testing.T) {
	c := &CompressionCapabilities{
		Flags:                 CompressionChainedFlg,
		CompressionAlgorithms: []uint16{CompressionLZ77, CompressionPatternV1},
	}
	data, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCompressionCapabilities(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Flags != c.Flags || len(got.CompressionAlgorithms) != 2 ||
		got.CompressionAlgorithms[1] != CompressionPatternV1 {
		t.Errorf("压缩 context round-trip 失败: %+v", got)
	}
	if _, err := ParseCompressionCapabilities(data[:4]); err == nil {
		t.Error("截断的压缩 context 应报错")
	}

	name, err := ParseNetnameContext(EncodeUTF16LE("SERVER01"))
	if err != nil || name != "SERVER01" {
		t.Errorf("netname = %q, err = %v", name, err)
	}
}
