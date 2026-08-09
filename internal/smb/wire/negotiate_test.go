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
// 布局已用真实抓包核对：testdata/capture/negotiate-smb311/001-c2s-NEGOTIATE.bin
// （smbclient 4.22）里前两条 context 的起点同样是 112 与 160，PREAUTH 的
// DataLength 同样是 38、SaltLength 同样是 32。真实请求比这里多 3 条
// （SIGNING_CAPABILITIES / NETNAME / Samba 的 0x0100），见
// TestCaptureNegotiate311Contexts。
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

// TestCaptureNegotiate311Contexts 用真实 smbclient / smbd 的 3.1.1 抓包
// 逐条核对 negotiate context 的解析（AGENTS.md §3「关键路径要有抓包比对」）。
//
// 这条测试比自造 golden 值钱的地方：真实请求里有一条 **规范里没有** 的
// context（Samba 的 0x0100 SMB2_POSIX_EXTENSIONS_AVAILABLE）。
// 未知类型必须被原样保留、不能让整条 NEGOTIATE 解析失败 —— 否则
// smbclient 连不上，而且这种失败在日志里看起来像「客户端报文损坏」。
func TestCaptureNegotiate311Contexts(t *testing.T) {
	req := readCaptureFrame(t, "negotiate-smb311", "001-c2s-NEGOTIATE.bin")
	r, err := ParseNegotiateRequest(req)
	if err != nil {
		t.Fatalf("真实 3.1.1 NEGOTIATE 请求解析失败: %v", err)
	}
	if !r.HasDialect(SMB311) {
		t.Fatal("请求应包含 3.1.1")
	}
	wantTypes := []NegotiateContextType{
		ContextPreauthIntegrityCapabilities,
		ContextEncryptionCapabilities,
		ContextSigningCapabilities,
		ContextNetnameNegotiateContextID,
		ContextPosixExtensions,
	}
	if len(r.Contexts) != len(wantTypes) {
		t.Fatalf("context 数 = %d, 期望 %d: %+v", len(r.Contexts), len(wantTypes), r.Contexts)
	}
	for i, want := range wantTypes {
		if r.Contexts[i].Type != want {
			t.Errorf("Contexts[%d].Type = %#04x, 期望 %#04x", i, r.Contexts[i].Type, want)
		}
	}

	pre, err := ParsePreauthIntegrityCapabilities(r.Contexts[0].Data)
	if err != nil {
		t.Fatalf("PREAUTH: %v", err)
	}
	if len(pre.HashAlgorithms) != 1 || pre.HashAlgorithms[0] != HashAlgorithmSHA512 {
		t.Errorf("HashAlgorithms = %v, 期望 [SHA-512]", pre.HashAlgorithms)
	}
	if len(pre.Salt) != 32 {
		t.Errorf("SaltLength = %d, 期望 32（Windows/Samba 都用 32）", len(pre.Salt))
	}

	enc, err := ParseEncryptionCapabilities(r.Contexts[1].Data)
	if err != nil {
		t.Fatalf("ENCRYPTION: %v", err)
	}
	// smbclient 4.22 按 GCM 优先的顺序报 4 个算法。
	want := []uint16{CipherAES128GCM, CipherAES128CCM, CipherAES256GCM, CipherAES256CCM}
	if len(enc.Ciphers) != len(want) {
		t.Fatalf("Ciphers = %v, 期望 %v", enc.Ciphers, want)
	}
	for i := range want {
		if enc.Ciphers[i] != want[i] {
			t.Errorf("Ciphers[%d] = %#04x, 期望 %#04x", i, enc.Ciphers[i], want[i])
		}
	}

	sign, err := ParseSigningCapabilities(r.Contexts[2].Data)
	if err != nil {
		t.Fatalf("SIGNING: %v", err)
	}
	wantSign := []uint16{SigningAlgorithmAESGMAC, SigningAlgorithmAESCMAC, SigningAlgorithmHMACSHA256}
	if len(sign.SigningAlgorithms) != len(wantSign) {
		t.Fatalf("SigningAlgorithms = %v, 期望 %v", sign.SigningAlgorithms, wantSign)
	}
	for i := range wantSign {
		if sign.SigningAlgorithms[i] != wantSign[i] {
			t.Errorf("SigningAlgorithms[%d] = %#04x, 期望 %#04x", i, sign.SigningAlgorithms[i], wantSign[i])
		}
	}

	if name, err := ParseNetnameContext(r.Contexts[3].Data); err != nil || name != "127.0.0.1" {
		t.Errorf("netname = %q, err = %v", name, err)
	}

	// Samba 的 POSIX 扩展 context：16 字节能力 GUID，我们只认不用。
	if got := r.Contexts[4].Data; !bytes.Equal(got, PosixExtensionsGUID[:]) {
		t.Errorf("POSIX 扩展 GUID = % X, 期望 % X", got, PosixExtensionsGUID[:])
	}

	// —— 服务端响应侧：真实 smbd 回了哪些 context ——
	resp := readCaptureFrame(t, "negotiate-smb311", "002-s2c-NEGOTIATE.bin")
	sr, err := ParseNegotiateResponse(resp)
	if err != nil {
		t.Fatalf("真实 3.1.1 NEGOTIATE 响应解析失败: %v", err)
	}
	if sr.DialectRevision != SMB311 {
		t.Fatalf("DialectRevision = %#04x", sr.DialectRevision)
	}
	if len(sr.Contexts) != 3 {
		t.Fatalf("响应 context 数 = %d, 期望 3", len(sr.Contexts))
	}
	// MS-SMB2 §3.3.5.4：响应里的 CipherCount / SigningAlgorithmCount 必须是 1。
	rEnc, err := ParseEncryptionCapabilities(sr.Contexts[1].Data)
	if err != nil || len(rEnc.Ciphers) != 1 || rEnc.Ciphers[0] != CipherAES128GCM {
		t.Errorf("响应 Ciphers = %v, err = %v, 期望 [AES-128-GCM]", rEnc, err)
	}
	rSign, err := ParseSigningCapabilities(sr.Contexts[2].Data)
	if err != nil || len(rSign.SigningAlgorithms) != 1 ||
		rSign.SigningAlgorithms[0] != SigningAlgorithmAESGMAC {
		t.Errorf("响应 SigningAlgorithms = %v, err = %v, 期望 [AES-GMAC]", rSign, err)
	}
}

// TestNegotiateContextCountLies 覆盖「计数字段撒谎」的各种姿势。
// 全部必须返回 error，一个 panic 都不许有 —— 这些字段完全由网络对端控制。
func TestNegotiateContextCountLies(t *testing.T) {
	// NegotiateContextCount 声称远多于实际。
	msg := goldenNegotiateRequest311()
	le.PutUint16(msg[HeaderSize+32:], 0xFFFF)
	if _, err := ParseNegotiateRequest(msg); err == nil {
		t.Error("越界的 NegotiateContextCount 应报错")
	}

	// NegotiateContextOffset 指到消息之外。
	msg = goldenNegotiateRequest311()
	le.PutUint32(msg[HeaderSize+28:], 0xFFFFFF00)
	if _, err := ParseNegotiateRequest(msg); err == nil {
		t.Error("越界的 NegotiateContextOffset 应报错")
	}

	// DialectCount 声称远多于实际。
	msg = goldenNegotiateRequest311()
	le.PutUint16(msg[HeaderSize+2:], 0xFFFF)
	if _, err := ParseNegotiateRequest(msg); err == nil {
		t.Error("越界的 DialectCount 应报错")
	}
}

// TestNegotiateContextPayloadLies 覆盖各 context 载荷内部的计数字段撒谎。
func TestNegotiateContextPayloadLies(t *testing.T) {
	// PREAUTH：HashAlgorithmCount 撒谎。
	pre, err := (&PreauthIntegrityCapabilities{
		HashAlgorithms: []uint16{HashAlgorithmSHA512},
		Salt:           bytes.Repeat([]byte{7}, 32),
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePreauthIntegrityCapabilities(pre); err != nil {
		t.Fatalf("正常载荷应能解析: %v", err)
	}
	bad := append([]byte(nil), pre...)
	le.PutUint16(bad[0:], 0xFFFF) // HashAlgorithmCount
	if _, err := ParsePreauthIntegrityCapabilities(bad); err == nil {
		t.Error("越界的 HashAlgorithmCount 应报错")
	}
	bad = append([]byte(nil), pre...)
	le.PutUint16(bad[2:], 0xFFFF) // SaltLength
	if _, err := ParsePreauthIntegrityCapabilities(bad); err == nil {
		t.Error("越界的 SaltLength 应报错")
	}
	// 截断到任意长度都不许 panic。
	for n := 0; n < len(pre); n++ {
		if _, err := ParsePreauthIntegrityCapabilities(pre[:n]); err == nil {
			t.Fatalf("PREAUTH 截断到 %d 应报错", n)
		}
	}

	// ENCRYPTION / SIGNING：Count 撒谎。
	enc, err := (&EncryptionCapabilities{Ciphers: []uint16{CipherAES128GCM}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	le.PutUint16(enc[0:], 0xFFFF)
	if _, err := ParseEncryptionCapabilities(enc); err == nil {
		t.Error("越界的 CipherCount 应报错")
	}
	sign, err := (&SigningCapabilities{SigningAlgorithms: []uint16{SigningAlgorithmAESCMAC}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	le.PutUint16(sign[0:], 0xFFFF)
	if _, err := ParseSigningCapabilities(sign); err == nil {
		t.Error("越界的 SigningAlgorithmCount 应报错")
	}

	// COMPRESSION：CompressionAlgorithmCount 撒谎。
	comp, err := (&CompressionCapabilities{CompressionAlgorithms: []uint16{CompressionLZ77}}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	le.PutUint16(comp[0:], 0xFFFF)
	if _, err := ParseCompressionCapabilities(comp); err == nil {
		t.Error("越界的 CompressionAlgorithmCount 应报错")
	}

	// Count = 0 是**合法**的报文形态，wire 层必须解析成功、交由策略层拒绝
	// （MS-SMB2 §3.3.5.4：SigningAlgorithmCount == 0 → STATUS_INVALID_PARAMETER，
	// 那是 command 层的判断，不是编解码错误）。
	if s, err := ParseSigningCapabilities([]byte{0x00, 0x00}); err != nil || len(s.SigningAlgorithms) != 0 {
		t.Errorf("SigningAlgorithmCount=0 应解析为空表, 得到 %+v, %v", s, err)
	}
	if p, err := ParsePreauthIntegrityCapabilities([]byte{0x00, 0x00, 0x00, 0x00}); err != nil ||
		len(p.HashAlgorithms) != 0 || len(p.Salt) != 0 {
		t.Errorf("空 PREAUTH 应解析为空, 得到 %+v, %v", p, err)
	}
}
