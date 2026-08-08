package auth

import (
	"bytes"
	"testing"
)

// negTokenInit2 的字节级 golden test。
//
// 期望值按 RFC 2743 §3.1 / RFC 4178 §4.2.1 / MS-SPNG §2.2.1 逐层手工推导，
// 与 Windows / Samba 服务端在 SMB2 NEGOTIATE Response 里发出的结构一致：
//
//	60 48                                  [APPLICATION 0] len=0x48
//	  06 06 2b 06 01 05 05 02              thisMech = 1.3.6.1.5.5.2 (SPNEGO)
//	  a0 3e                                [0] negTokenInit2
//	    30 3c                              SEQUENCE
//	      a0 0e 30 0c                      [0] mechTypes SEQUENCE
//	        06 0a 2b 06 01 04 01 82 37 02 02 0a   NTLMSSP OID
//	      a3 2a 30 28                      [3] negHints SEQUENCE
//	        a0 26 1b 24 "not_defined_in_RFC4178@please_ignore"
func TestNegTokenInit2Golden(t *testing.T) {
	got := NegTokenInit2([][]byte{oidNTLMSSP})

	want := []byte{
		0x60, 0x48,
		0x06, 0x06, 0x2b, 0x06, 0x01, 0x05, 0x05, 0x02,
		0xa0, 0x3e,
		0x30, 0x3c,
		0xa0, 0x0e,
		0x30, 0x0c,
		0x06, 0x0a, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a,
		0xa3, 0x2a,
		0x30, 0x28,
		0xa0, 0x26,
		0x1b, 0x24,
	}
	want = append(want, []byte(negHintName)...)

	if !bytes.Equal(got, want) {
		t.Fatalf("negTokenInit2 不符\n got: % x\nwant: % x", got, want)
	}
	if len(negHintName) != 0x24 {
		t.Fatalf("negHints 字符串长度 = %d, want 36", len(negHintName))
	}
}

// 服务端第一次回应：accept-incomplete + supportedMech=NTLM + responseToken。
func TestNegTokenRespGolden(t *testing.T) {
	challenge := []byte{0xde, 0xad, 0xbe, 0xef}
	got := NegTokenResp(NegAcceptIncomplete, oidNTLMSSP, challenge, nil)

	// 内容 = negState(5) + supportedMech(14) + responseToken(8) = 27 = 0x1b
	want := []byte{
		0xa1, 0x1d,
		0x30, 0x1b,
		0xa0, 0x03, 0x0a, 0x01, 0x01, // negState = accept-incomplete
		0xa1, 0x0c, 0x06, 0x0a, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x02, 0x0a,
		0xa2, 0x06, 0x04, 0x04, 0xde, 0xad, 0xbe, 0xef,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("negTokenResp 不符\n got: % x\nwant: % x", got, want)
	}

	// 最终成功：a1 07 30 05 a0 03 0a 01 00
	final := NegTokenResp(NegAcceptCompleted, nil, nil, nil)
	wantFinal := []byte{0xa1, 0x07, 0x30, 0x05, 0xa0, 0x03, 0x0a, 0x01, 0x00}
	if !bytes.Equal(final, wantFinal) {
		t.Fatalf("最终 negTokenResp 不符\n got: % x\nwant: % x", final, wantFinal)
	}
}

// 解析客户端的 negTokenInit（GSS 外壳 + mechTypes + mechToken）。
func TestParseClientNegTokenInit(t *testing.T) {
	ntlm := []byte("NTLMSSP\x00\x01\x00\x00\x00")

	mechList := append(derTLV(tagOID, oidKerberos5), derTLV(tagOID, oidNTLMSSP)...)
	mechTypes := derTLV(tagContext0, derTLV(tagSequence, mechList))
	mechToken := derTLV(tagContext2, derTLV(tagOctetString, ntlm))
	inner := derTLV(tagContext0, derTLV(tagSequence, append(mechTypes, mechToken...)))
	in := derTLV(tagApplication0, append(derTLV(tagOID, oidSPNEGO), inner...))

	tok, err := ParseSPNEGO(in)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if tok.Raw {
		t.Error("不应判定为裸 NTLMSSP")
	}
	if !tok.Init {
		t.Error("应判定为 negTokenInit")
	}
	if !tok.HasMech(oidNTLMSSP) || !tok.HasMech(oidKerberos5) {
		t.Errorf("mechTypes 解析错: %x", tok.MechTypes)
	}
	if !bytes.Equal(tok.Token, ntlm) {
		t.Errorf("mechToken = % x", tok.Token)
	}
	// mechListMIC 的计算范围是 mechTypes 内层 SEQUENCE 的完整 DER。
	if !bytes.Equal(tok.MechTypesDER, derTLV(tagSequence, mechList)) {
		t.Errorf("MechTypesDER = % x", tok.MechTypesDER)
	}
}

// 解析客户端的 negTokenResp（第二轮，携带 AUTHENTICATE）。
func TestParseClientNegTokenResp(t *testing.T) {
	ntlm := []byte("NTLMSSP\x00\x03\x00\x00\x00")
	mic := bytes.Repeat([]byte{0xAB}, 16)

	body := derTLV(tagContext2, derTLV(tagOctetString, ntlm))
	body = append(body, derTLV(tagContext3, derTLV(tagOctetString, mic))...)
	in := derTLV(tagContext1, derTLV(tagSequence, body))

	tok, err := ParseSPNEGO(in)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if tok.Init {
		t.Error("不应判定为 negTokenInit")
	}
	if !bytes.Equal(tok.Token, ntlm) {
		t.Errorf("responseToken = % x", tok.Token)
	}
	if !bytes.Equal(tok.MechListMIC, mic) {
		t.Errorf("mechListMIC = % x", tok.MechListMIC)
	}
}

// 裸 NTLMSSP（无 SPNEGO 外壳）。
func TestParseRawNTLMSSP(t *testing.T) {
	raw := []byte("NTLMSSP\x00\x01\x00\x00\x00rest")
	tok, err := ParseSPNEGO(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !tok.Raw || !bytes.Equal(tok.Token, raw) {
		t.Errorf("裸 NTLMSSP 识别失败: %+v", tok)
	}
}

// 畸形 DER 一律返回错误且不 panic。
func TestParseSPNEGOMalformed(t *testing.T) {
	valid := derTLV(tagApplication0, append(derTLV(tagOID, oidSPNEGO),
		derTLV(tagContext0, derTLV(tagSequence,
			derTLV(tagContext0, derTLV(tagSequence, derTLV(tagOID, oidNTLMSSP)))))...))

	for i := 0; i < len(valid); i++ {
		// 截断的输入不得 panic。
		_, _ = ParseSPNEGO(valid[:i])
	}
	// 每个字节翻转也不得 panic。
	for i := range valid {
		bad := make([]byte, len(valid))
		copy(bad, valid)
		bad[i] ^= 0xFF
		_, _ = ParseSPNEGO(bad)
	}

	if _, err := ParseSPNEGO(nil); err != ErrInvalidToken {
		t.Error("空输入应返回 ErrInvalidToken")
	}
	if _, err := ParseSPNEGO([]byte{0x30, 0x00}); err != ErrInvalidToken {
		t.Error("非 SPNEGO 顶层 tag 应被拒绝")
	}
	// 声明的长度超过实际字节。
	if _, err := ParseSPNEGO([]byte{0x60, 0x7f, 0x06}); err != ErrInvalidToken {
		t.Error("长度越界应被拒绝")
	}
	// 非 SPNEGO OID 的 GSS token（例如裸 Kerberos）。
	krb := derTLV(tagApplication0, derTLV(tagOID, oidKerberos5))
	if _, err := ParseSPNEGO(krb); err != ErrMechUnsupported {
		t.Errorf("非 SPNEGO OID 应返回 ErrMechUnsupported, got %v", err)
	}
}

// DER 长度编码的短型/长型边界。
func TestDERLen(t *testing.T) {
	cases := []struct {
		n    int
		want []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7f}},
		{128, []byte{0x81, 0x80}},
		{255, []byte{0x81, 0xff}},
		{256, []byte{0x82, 0x01, 0x00}},
		{65535, []byte{0x82, 0xff, 0xff}},
		{65536, []byte{0x83, 0x01, 0x00, 0x00}},
	}
	for _, c := range cases {
		if got := derLen(c.n); !bytes.Equal(got, c.want) {
			t.Errorf("derLen(%d) = % x, want % x", c.n, got, c.want)
		}
	}

	// 长型长度回环：构造一个 300 字节的 OCTET STRING 再解析出来。
	payload := bytes.Repeat([]byte{0x5a}, 300)
	tag, content, rest, err := derNext(derTLV(tagOctetString, payload))
	if err != nil || tag != tagOctetString || len(rest) != 0 || !bytes.Equal(content, payload) {
		t.Errorf("长型长度回环失败: tag=%x err=%v len=%d", tag, err, len(content))
	}
}
