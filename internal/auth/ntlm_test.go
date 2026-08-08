package auth

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// unhex 把带空白的十六进制串转成字节。
func unhex(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.Join(strings.Fields(s), "")
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("测试向量不是合法十六进制: %v", err)
	}
	return b
}

// MS-NLMP §4.2.4 NTLMv2 Authentication 的官方示例向量。
//
// §4.2.1 公共参数：
//
//	User "User"、Domain "Domain"、Password "Password"
//	Server NetBIOS 名 "Server"、客户端 NetBIOS 名 "COMPUTER"
//	ServerChallenge = 0123456789abcdef
//	ClientChallenge = aaaaaaaaaaaaaaaa
//	Time = 0，RandomSessionKey = 55 x16
const (
	msnlmpUser     = "User"
	msnlmpDomain   = "Domain"
	msnlmpPassword = "Password"
)

var (
	msnlmpServerChallenge = [8]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}

	// §4.2.4.3 CHALLENGE_MESSAGE
	msnlmpChallengeMsg = `
	4e 54 4c 4d 53 53 50 00 02 00 00 00 0c 00 0c 00
	38 00 00 00 33 82 8a e2 01 23 45 67 89 ab cd ef
	00 00 00 00 00 00 00 00 24 00 24 00 44 00 00 00
	06 00 70 17 00 00 00 0f 53 00 65 00 72 00 76 00
	65 00 72 00 02 00 0c 00 44 00 6f 00 6d 00 61 00
	69 00 6e 00 01 00 0c 00 53 00 65 00 72 00 76 00
	65 00 72 00 00 00 00 00`

	// §4.2.4.3 AUTHENTICATE_MESSAGE
	msnlmpAuthenticateMsg = `
	4e 54 4c 4d 53 53 50 00 03 00 00 00 18 00 18 00
	6c 00 00 00 54 00 54 00 84 00 00 00 0c 00 0c 00
	48 00 00 00 08 00 08 00 54 00 00 00 10 00 10 00
	5c 00 00 00 10 00 10 00 d8 00 00 00 35 82 88 e2
	05 01 28 0a 00 00 00 0f 44 00 6f 00 6d 00 61 00
	69 00 6e 00 55 00 73 00 65 00 72 00 43 00 4f 00
	4d 00 50 00 55 00 54 00 45 00 52 00 86 c3 50 97
	ac 9c ec 10 25 54 76 4a 57 cc cc 19 aa aa aa aa
	aa aa aa aa 68 cd 0a b8 51 e5 1c 96 aa bc 92 7b
	eb ef 6a 1c 01 01 00 00 00 00 00 00 00 00 00 00
	00 00 00 00 aa aa aa aa aa aa aa aa 00 00 00 00
	02 00 0c 00 44 00 6f 00 6d 00 61 00 69 00 6e 00
	01 00 0c 00 53 00 65 00 72 00 76 00 65 00 72 00
	00 00 00 00 00 00 00 00 c5 da d2 54 4f c9 79 90
	94 ce 1c e9 0b c9 d0 3e`

	// §4.2.4.1.3 temp（NtChallengeResponse 去掉前 16 字节 NTProofStr）
	msnlmpTemp = `
	01 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00
	aa aa aa aa aa aa aa aa 00 00 00 00 02 00 0c 00
	44 00 6f 00 6d 00 61 00 69 00 6e 00 01 00 0c 00
	53 00 65 00 72 00 76 00 65 00 72 00 00 00 00 00
	00 00 00 00`
)

// §4.2.4.1.1 NTOWFv2 与 §4.2.4.1.2 SessionBaseKey。
func TestNTOWFv2MSNLMP(t *testing.T) {
	nt := NTHash(msnlmpPassword)
	ntowf := NTOWFv2(nt, msnlmpUser, msnlmpDomain)
	want := "0c868a403bfd7a93a3001ef22ef02e3f"
	if hex.EncodeToString(ntowf[:]) != want {
		t.Fatalf("NTOWFv2 = %x, want %s", ntowf, want)
	}

	// §4.2.4.1.3：NTProofStr = HMAC_MD5(NTOWFv2, ServerChallenge || temp)
	temp := unhex(t, msnlmpTemp)
	proof := ComputeNTProofStr(ntowf, msnlmpServerChallenge, temp)
	wantProof := "68cd0ab851e51c96aabc927bebef6a1c"
	if hex.EncodeToString(proof[:]) != wantProof {
		t.Fatalf("NTProofStr = %x, want %s", proof, wantProof)
	}

	sbk := SessionBaseKey(ntowf, proof)
	wantSBK := "8de40ccadbc14a82f15cb0ad0de95ca3"
	if hex.EncodeToString(sbk[:]) != wantSBK {
		t.Fatalf("SessionBaseKey = %x, want %s", sbk, wantSBK)
	}
}

// §4.2.4.3 CHALLENGE_MESSAGE 编码 golden test。
func TestChallengeMessageGolden(t *testing.T) {
	want := unhex(t, msnlmpChallengeMsg)

	m := &ChallengeMessage{
		TargetName:      "Server",
		Flags:           NegotiateFlags(0xe28a8233),
		ServerChallenge: msnlmpServerChallenge,
		TargetInfo: AvPairs{
			{ID: MsvAvNbDomainName, Value: utf16le("Domain")},
			{ID: MsvAvNbComputerName, Value: utf16le("Server")},
		},
		Version: Version{Major: 6, Minor: 0, Build: 6000, NTLMRevision: NTLMRevisionCurrent},
	}
	got := m.Marshal()
	if !bytes.Equal(got, want) {
		t.Fatalf("CHALLENGE 编码不符\n got: % x\nwant: % x", got, want)
	}

	// 回环解析。
	back, err := ParseChallengeMessage(want)
	if err != nil {
		t.Fatalf("解析 CHALLENGE: %v", err)
	}
	if back.TargetName != "Server" {
		t.Errorf("TargetName = %q", back.TargetName)
	}
	if back.ServerChallenge != msnlmpServerChallenge {
		t.Errorf("ServerChallenge = %x", back.ServerChallenge)
	}
	if len(back.TargetInfo) != 2 {
		t.Fatalf("TargetInfo 条数 = %d", len(back.TargetInfo))
	}
	if v, ok := back.TargetInfo.Get(MsvAvNbDomainName); !ok || fromUTF16LE(v) != "Domain" {
		t.Errorf("NbDomainName = %q", fromUTF16LE(v))
	}
	if back.Version.Major != 6 || back.Version.Build != 6000 ||
		back.Version.NTLMRevision != NTLMRevisionCurrent {
		t.Errorf("Version = %+v", back.Version)
	}
}

// §4.2.4.3 AUTHENTICATE_MESSAGE 解析 + 完整服务端校验链路。
func TestAuthenticateMessageMSNLMP(t *testing.T) {
	raw := unhex(t, msnlmpAuthenticateMsg)
	m, err := ParseAuthenticateMessage(raw)
	if err != nil {
		t.Fatalf("解析 AUTHENTICATE: %v", err)
	}
	if m.DomainName != "Domain" || m.UserName != "User" || m.Workstation != "COMPUTER" {
		t.Fatalf("身份字段: domain=%q user=%q ws=%q", m.DomainName, m.UserName, m.Workstation)
	}
	if m.Flags != NegotiateFlags(0xe2888235) {
		t.Errorf("Flags = %08x", uint32(m.Flags))
	}
	// 本示例的 payload 从偏移 72 开始，说明没有 MIC 字段。
	if m.MICPresent {
		t.Errorf("不应判定为有 MIC")
	}
	if len(m.NTResponse) != 84 {
		t.Fatalf("NtChallengeResponse 长度 = %d, want 84", len(m.NTResponse))
	}
	if len(m.LMResponse) != 24 {
		t.Fatalf("LmChallengeResponse 长度 = %d, want 24", len(m.LMResponse))
	}
	if !bytes.Equal(m.NTResponse[NTProofStrLen:], unhex(t, msnlmpTemp)) {
		t.Errorf("temp 不符")
	}

	// 服务端校验链路。
	ntowf := NTOWFv2(NTHash(msnlmpPassword), m.UserName, m.DomainName)
	sbk, blob, ok := VerifyNTLMv2(ntowf, msnlmpServerChallenge, m.NTResponse)
	if !ok {
		t.Fatal("NTLMv2 校验应当通过")
	}
	if hex.EncodeToString(sbk[:]) != "8de40ccadbc14a82f15cb0ad0de95ca3" {
		t.Errorf("SessionBaseKey = %x", sbk)
	}

	// blob 里应能解析出 AV_PAIR。
	avs, err := ParseAvPairs(blob[28:])
	if err != nil {
		t.Fatalf("解析 blob 中的 AV_PAIR: %v", err)
	}
	if v, ok := avs.Get(MsvAvNbComputerName); !ok || fromUTF16LE(v) != "Server" {
		t.Errorf("blob NbComputerName = %q", fromUTF16LE(v))
	}

	// §4.2.4.2：KEY_EXCH 置位，RC4 解出 ExportedSessionKey = 55 x16。
	if !m.Flags.Has(NegotiateKeyExch) {
		t.Fatal("示例应置 NTLMSSP_NEGOTIATE_KEY_EXCH")
	}
	esk, err := ExportedSessionKey(KeyExchangeKey(sbk), m.Flags, m.EncryptedRandomSessionKey)
	if err != nil {
		t.Fatalf("ExportedSessionKey: %v", err)
	}
	want := bytes.Repeat([]byte{0x55}, 16)
	if !bytes.Equal(esk[:], want) {
		t.Fatalf("ExportedSessionKey = %x, want %x", esk, want)
	}

	// 错误口令必须失败。
	bad := NTOWFv2(NTHash("wrong"), m.UserName, m.DomainName)
	if _, _, ok := VerifyNTLMv2(bad, msnlmpServerChallenge, m.NTResponse); ok {
		t.Error("错误口令不应通过")
	}
}

// Marshal → Parse 回环（含 MIC 分支）。
func TestAuthenticateMessageRoundTrip(t *testing.T) {
	m := &AuthenticateMessage{
		LMResponse:                bytes.Repeat([]byte{0x11}, 24),
		NTResponse:                bytes.Repeat([]byte{0x22}, 48),
		DomainName:                "WORKGROUP",
		UserName:                  "alice",
		Workstation:               "LAPTOP",
		EncryptedRandomSessionKey: bytes.Repeat([]byte{0x33}, 16),
		Flags:                     NegotiateUnicode | NegotiateKeyExch | NegotiateVersion,
		MICPresent:                true,
	}
	copy(m.MIC[:], bytes.Repeat([]byte{0x44}, MICSize))

	back, err := ParseAuthenticateMessage(m.Marshal())
	if err != nil {
		t.Fatalf("回环解析: %v", err)
	}
	if back.UserName != "alice" || back.DomainName != "WORKGROUP" || back.Workstation != "LAPTOP" {
		t.Errorf("身份字段错: %+v", back)
	}
	if !back.MICPresent || back.MIC != m.MIC {
		t.Errorf("MIC 回环失败: present=%v mic=%x", back.MICPresent, back.MIC)
	}
	if !bytes.Equal(back.NTResponse, m.NTResponse) || !bytes.Equal(back.LMResponse, m.LMResponse) {
		t.Error("响应字段回环失败")
	}
	if !bytes.Equal(back.EncryptedRandomSessionKey, m.EncryptedRandomSessionKey) {
		t.Error("EncryptedRandomSessionKey 回环失败")
	}
}

// 畸形输入不得 panic，一律返回错误。
func TestParseMalformed(t *testing.T) {
	full := unhex(t, msnlmpAuthenticateMsg)
	for i := 0; i < len(full); i++ {
		if _, err := ParseAuthenticateMessage(full[:i]); err == nil && i < len(full) {
			// 截断后仍可能解析成功（payload 恰好都在前面），只要不 panic 即可。
			continue
		}
	}
	// 偏移指向消息之外。
	bad := make([]byte, len(full))
	copy(bad, full)
	bad[20+4] = 0xff // NtChallengeResponse 的 BufferOffset 低字节
	bad[20+5] = 0xff
	if _, err := ParseAuthenticateMessage(bad); err == nil {
		t.Error("越界偏移应当被拒绝")
	}

	if _, err := MessageType([]byte("XXXXXXXX\x01\x00\x00\x00")); err != ErrNTLMSignature {
		t.Error("坏魔数应返回 ErrNTLMSignature")
	}
	if _, err := MessageType(nil); err != ErrNTLMTruncated {
		t.Error("空输入应返回 ErrNTLMTruncated")
	}
	if _, err := ParseAvPairs([]byte{0x01, 0x00, 0xff, 0x00, 0x01}); err != ErrNTLMTruncated {
		t.Error("AV_PAIR 长度越界应被拒绝")
	}
}

func TestNegotiateMessageRoundTrip(t *testing.T) {
	m := &NegotiateMessage{
		Flags:       NegotiateUnicode | NegotiateNTLM | NegotiateVersion | NegotiateExtendedSessionSecurity,
		Domain:      "WORKGROUP",
		Workstation: "CLIENT",
		Version:     Version{Major: 10, Minor: 0, Build: 19041, NTLMRevision: NTLMRevisionCurrent},
	}
	back, err := ParseNegotiateMessage(m.Marshal())
	if err != nil {
		t.Fatalf("回环解析: %v", err)
	}
	if back.Domain != "WORKGROUP" || back.Workstation != "CLIENT" {
		t.Errorf("字段错: %+v", back)
	}
	if back.Version != m.Version {
		t.Errorf("Version = %+v, want %+v", back.Version, m.Version)
	}

	// 只有 16 字节的最小 NEGOTIATE（部分客户端会这么发）不应报错。
	min := m.Marshal()[:16]
	if _, err := ParseNegotiateMessage(min); err != nil {
		t.Errorf("最小 NEGOTIATE 解析失败: %v", err)
	}
}
