package auth

import (
	"bytes"
	"testing"
)

// 本文件的字节向量**全部来自 MS-NLMP §4.2 的 worked example**（规范原文），
// 不是自造的（AGENTS.md §3）。

// specRandomSessionKey 是 MS-NLMP §4.2.1 里 worked example 使用的
// RandomSessionKey / ExportedSessionKey：16 个 0x55。
var specRandomSessionKey = bytes.Repeat([]byte{0x55}, 16)

// plaintextExample 是 §4.2.3.4 / §4.2.4.4 里被封装的明文
// "Plaintext" 的 UTF-16LE 编码。
var plaintextExample = []byte{
	0x50, 0x00, 0x6c, 0x00, 0x61, 0x00, 0x69, 0x00, 0x6e, 0x00,
	0x74, 0x00, 0x65, 0x00, 0x78, 0x00, 0x74, 0x00,
}

// MS-NLMP §4.2.4.4（NTLMv2，协商 128 + KEY_EXCH）。
//
// 该例同时覆盖 SIGNKEY、SEALKEY(128)、MAC 与 **RC4 句柄的有状态性**：
// 先用同一个句柄封装 18 字节明文，再用它继续加密 8 字节 checksum。
func TestGSSWrapExNTLMv2Golden(t *testing.T) {
	// §4.2.4.4 里的 flags 隐含 EXTENDED_SESSIONSECURITY（用了 HMAC 型 MAC）、
	// 128（sealkey 用了全长密钥）与 KEY_EXCH（checksum 被 RC4 再加密一次）。
	flags := NegotiateExtendedSessionSecurity | Negotiate128 | NegotiateKeyExch

	wantSeal := []byte{
		0x59, 0xf6, 0x00, 0x97, 0x3c, 0xc4, 0x96, 0x0a,
		0x25, 0x48, 0x0a, 0x7c, 0x19, 0x6e, 0x4c, 0x58,
	}
	wantSign := []byte{
		0x47, 0x88, 0xdc, 0x86, 0x1b, 0x47, 0x82, 0xf3,
		0x5d, 0x43, 0xfd, 0x98, 0xfe, 0x1a, 0x2d, 0x39,
	}
	if got := SealKey(specRandomSessionKey, flags, true); !bytes.Equal(got, wantSeal) {
		t.Errorf("SEALKEY = % x, want % x", got, wantSeal)
	}
	if got := SignKey(specRandomSessionKey, flags, true); !bytes.Equal(got, wantSign) {
		t.Errorf("SIGNKEY = % x, want % x", got, wantSign)
	}

	sc, err := NewSigningContext(specRandomSessionKey, flags, true)
	if err != nil {
		t.Fatalf("NewSigningContext: %v", err)
	}

	// SEAL()：先用封装句柄加密明文（消耗 18 字节 keystream）。
	sealed := make([]byte, len(plaintextExample))
	sc.seal.XORKeyStream(sealed, plaintextExample)
	wantSealed := []byte{
		0x54, 0xe5, 0x01, 0x65, 0xbf, 0x19, 0x36, 0xdc, 0x99,
		0x60, 0x20, 0xc1, 0x81, 0x1b, 0x0f, 0x06, 0xfb, 0x5f,
	}
	if !bytes.Equal(sealed, wantSealed) {
		t.Errorf("sealed data = % x, want % x", sealed, wantSealed)
	}

	// 再算签名：checksum 走同一个句柄的后 8 字节 keystream。
	got := sc.MIC(plaintextExample)
	want := [SignatureLen]byte{
		0x01, 0x00, 0x00, 0x00,
		0x7f, 0xb3, 0x8e, 0xc5, 0xc5, 0x5d, 0x49, 0x76,
		0x00, 0x00, 0x00, 0x00,
	}
	if got != want {
		t.Errorf("signature = % x, want % x", got, want)
	}
	if sc.seqNum != 1 {
		t.Errorf("seqNum = %d, want 1", sc.seqNum)
	}
}

// MS-NLMP §4.2.3.4（NTLMv1 + extended session security，协商 56，无 KEY_EXCH）。
//
// 覆盖 SEALKEY 的 56 位截断分支，以及"未协商 KEY_EXCH 时 checksum 不做 RC4"。
// 该例的会话密钥即 §4.2.3.1.3 的 KXKEY，其前 7 字节在规范里被显式列出。
func TestGSSWrapExNTLMv1ESSGolden(t *testing.T) {
	flags := NegotiateExtendedSessionSecurity | Negotiate56

	// §4.2.3.1.3 KXKEY = HMAC_MD5(SessionBaseKey, ServerChallenge || ClientChallenge[0..7])。
	// 规范 §4.2.3.4 只给出"cut key exchange key to 56 bits"的结果 eb 93 42 9a 8b d9 52，
	// 这里用完整 KXKEY 参与 SIGNKEY，用其前 7 字节参与 SEALKEY。
	kxKey := []byte{
		0xeb, 0x93, 0x42, 0x9a, 0x8b, 0xd9, 0x52, 0xf8,
		0xb8, 0x9c, 0x55, 0xb8, 0x7f, 0x47, 0x5e, 0xdc,
	}
	wantSeal := []byte{
		0x04, 0xdd, 0x7f, 0x01, 0x4d, 0x85, 0x04, 0xd2,
		0x65, 0xa2, 0x5c, 0xc8, 0x6a, 0x3a, 0x7c, 0x06,
	}
	wantSign := []byte{
		0x60, 0xe7, 0x99, 0xbe, 0x5c, 0x72, 0xfc, 0x92,
		0x92, 0x2a, 0xe8, 0xeb, 0xe9, 0x61, 0xfb, 0x8d,
	}
	if got := SealKey(kxKey, flags, true); !bytes.Equal(got, wantSeal) {
		t.Errorf("SEALKEY(56) = % x, want % x", got, wantSeal)
	}
	if got := SignKey(kxKey, flags, true); !bytes.Equal(got, wantSign) {
		t.Errorf("SIGNKEY = % x, want % x", got, wantSign)
	}

	sc, err := NewSigningContext(kxKey, flags, true)
	if err != nil {
		t.Fatalf("NewSigningContext: %v", err)
	}
	if sc.seal != nil {
		t.Error("未协商 KEY_EXCH 时不应建立 RC4 句柄")
	}
	got := sc.MIC(plaintextExample)
	want := [SignatureLen]byte{
		0x01, 0x00, 0x00, 0x00,
		0xff, 0x2a, 0xeb, 0x52, 0xf6, 0x81, 0x79, 0x3a,
		0x00, 0x00, 0x00, 0x00,
	}
	if got != want {
		t.Errorf("signature = % x, want % x", got, want)
	}
}

// 两个方向的密钥必须不同，否则会把自己的签名当成对端的接受。
func TestSignKeyDirectionsDiffer(t *testing.T) {
	flags := NegotiateExtendedSessionSecurity | Negotiate128 | NegotiateKeyExch
	c := SignKey(specRandomSessionKey, flags, true)
	s := SignKey(specRandomSessionKey, flags, false)
	if bytes.Equal(c, s) {
		t.Fatal("client/server 方向的 SIGNKEY 相同")
	}
	if bytes.Equal(SealKey(specRandomSessionKey, flags, true), SealKey(specRandomSessionKey, flags, false)) {
		t.Fatal("client/server 方向的 SEALKEY 相同")
	}
}

// 序号必须逐条递增，且体现在签名尾部。
func TestMICSeqNumAdvances(t *testing.T) {
	flags := NegotiateExtendedSessionSecurity | Negotiate128
	sc, err := NewSigningContext(specRandomSessionKey, flags, false)
	if err != nil {
		t.Fatalf("NewSigningContext: %v", err)
	}
	first := sc.MIC([]byte("a"))
	second := sc.MIC([]byte("a"))
	if !bytes.Equal(first[12:], []byte{0, 0, 0, 0}) {
		t.Errorf("第一条 SeqNum = % x, want 0", first[12:])
	}
	if !bytes.Equal(second[12:], []byte{1, 0, 0, 0}) {
		t.Errorf("第二条 SeqNum = % x, want 1", second[12:])
	}
	if bytes.Equal(first[4:12], second[4:12]) {
		t.Error("同一消息不同序号的 checksum 不应相同")
	}
}

// 未协商 EXTENDED_SESSIONSECURITY 时没有 SignKey（MS-NLMP §3.4.5.2）。
func TestNoExtendedSessionSecurity(t *testing.T) {
	if k := SignKey(specRandomSessionKey, Negotiate128, true); k != nil {
		t.Errorf("SignKey = % x, want nil", k)
	}
	if _, err := NewSigningContext(specRandomSessionKey, Negotiate128, true); err != ErrNoSessionSecurity {
		t.Errorf("err = %v, want ErrNoSessionSecurity", err)
	}
}
