package auth

import (
	"bytes"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/config"
)

// micHandshake 跑一遍完整的 SPNEGO + NTLMv2 握手，客户端侧按 RFC 4178 §5
// 在最终 negTokenResp 里携带 mechListMIC。
//
// mutate 用于在发出前篡改 MIC（nil 表示不篡改）；
// sendMIC 为 false 表示模拟不发 MIC 的老客户端。
//
// 返回服务端最终 token 的解析结果与错误。
func micHandshake(t *testing.T, sendMIC bool, mutate func([]byte)) (*SPNEGOToken, []byte, error) {
	t.Helper()

	const pw = "s3cret"
	store, err := NewStaticStore(config.Auth{
		Users: []config.User{{Name: "alice", Password: pw}},
	}, "WORKGROUP")
	if err != nil {
		t.Fatalf("构造账户表: %v", err)
	}
	p := NewNTLMProvider(Options{Store: store, ServerName: "STUPIDSAMBA", DomainName: "WORKGROUP"})
	ctx := p.NewContext()

	flags := NegotiateUnicode | NegotiateRequestTarget | NegotiateNTLM |
		NegotiateExtendedSessionSecurity | NegotiateAlwaysSign | NegotiateKeyExch |
		NegotiateSign | Negotiate128 | Negotiate56

	// ---- 第一轮：negTokenInit（mechTypes + NTLM NEGOTIATE）
	//
	// mechListDER 就是 mechTypes 字段 [0] 里那层 SEQUENCE 的完整 DER，
	// 客户端与服务端都用它来算 mechListMIC。
	mechListDER := derTLV(tagSequence, derTLV(tagOID, oidNTLMSSP))
	neg := (&NegotiateMessage{Flags: flags, Workstation: "CLIENT"}).Marshal()
	initTok := derTLV(tagApplication0, append(derTLV(tagOID, oidSPNEGO),
		derTLV(tagContext0, derTLV(tagSequence, append(
			derTLV(tagContext0, mechListDER),
			derTLV(tagContext2, derTLV(tagOctetString, neg))...)))...))

	out, done, err := ctx.Step(initTok)
	if err != nil || done {
		t.Fatalf("第一轮: err=%v done=%v", err, done)
	}
	ch, err := ParseChallengeMessage(unwrap(t, out))
	if err != nil {
		t.Fatalf("解析 CHALLENGE: %v", err)
	}

	// ---- 第二轮：客户端算 AUTHENTICATE 与 mechListMIC
	ntResp, ntowf := clientNTLMv2(pw, "alice", "WORKGROUP", ch.ServerChallenge,
		ch.TargetInfo, [8]byte{1, 2, 3, 4, 5, 6, 7, 8})
	var proof [16]byte
	copy(proof[:], ntResp[:16])

	esk := bytes.Repeat([]byte{0x77}, 16) // 客户端自选的 RandomSessionKey
	encKey, err := ExportedSessionKey(SessionBaseKey(ntowf, proof), NegotiateKeyExch, esk)
	if err != nil {
		t.Fatalf("RC4: %v", err)
	}
	authMsg := (&AuthenticateMessage{
		NTResponse:                ntResp,
		DomainName:                "WORKGROUP",
		UserName:                  "alice",
		Workstation:               "CLIENT",
		EncryptedRandomSessionKey: encKey[:],
		Flags:                     ch.Flags,
	}).Marshal()

	body := derTLV(tagContext2, derTLV(tagOctetString, authMsg))
	if sendMIC {
		cc, err := NewSigningContext(esk, ch.Flags, true) // 客户端方向
		if err != nil {
			t.Fatalf("客户端 SigningContext: %v", err)
		}
		mic := cc.MIC(mechListDER)
		if mutate != nil {
			mutate(mic[:])
		}
		body = append(body, derTLV(tagContext3, derTLV(tagOctetString, mic[:]))...)
	}

	out, _, err = ctx.Step(derTLV(tagContext1, derTLV(tagSequence, body)))
	if err != nil {
		return nil, esk, err
	}
	final, perr := ParseSPNEGO(out)
	if perr != nil {
		t.Fatalf("解析最终 token: %v", perr)
	}
	return final, esk, nil
}

// 客户端带正确的 mechListMIC：服务端必须校验通过，并回自己方向的 MIC。
func TestMechListMICRoundTrip(t *testing.T) {
	final, esk, err := micHandshake(t, true, nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	if final.State != NegAcceptCompleted {
		t.Fatalf("negState = %v, want accept-completed", final.State)
	}
	if len(final.MechListMIC) != SignatureLen {
		t.Fatalf("服务端 mechListMIC 长度 = %d, want %d", len(final.MechListMIC), SignatureLen)
	}

	// 客户端侧独立复算：服务端方向、序号 0、输入是 MechTypeList 的完整 DER。
	flags := NegotiateUnicode | NegotiateRequestTarget | NegotiateNTLM |
		NegotiateExtendedSessionSecurity | NegotiateAlwaysSign | NegotiateKeyExch |
		NegotiateSign | NegotiateTargetTypeServer | NegotiateTargetInfo |
		NegotiateVersion | Negotiate128 | Negotiate56
	sc, err := NewSigningContext(esk, flags, false)
	if err != nil {
		t.Fatalf("SigningContext: %v", err)
	}
	want := sc.MIC(derTLV(tagSequence, derTLV(tagOID, oidNTLMSSP)))
	if !bytes.Equal(final.MechListMIC, want[:]) {
		t.Errorf("服务端 mechListMIC = % x, want % x", final.MechListMIC, want[:])
	}
	// 服务端方向的 MIC 不能等于客户端方向的（否则密钥方向搞反了）。
	cc, _ := NewSigningContext(esk, flags, true)
	clientMIC := cc.MIC(derTLV(tagSequence, derTLV(tagOID, oidNTLMSSP)))
	if bytes.Equal(final.MechListMIC, clientMIC[:]) {
		t.Error("服务端用了客户端方向的 SigningKey")
	}
}

// 被篡改的 mechListMIC 必须导致认证失败（防机制降级中间人）。
func TestMechListMICTampered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"翻转 checksum", func(b []byte) { b[4] ^= 0xFF }},
		{"改 seqnum", func(b []byte) { b[12] = 0x01 }},
		{"改 version", func(b []byte) { b[0] = 0x02 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := micHandshake(t, true, tc.mutate); err != ErrLogonFailure {
				t.Fatalf("err = %v, want ErrLogonFailure", err)
			}
		})
	}
}

// 老客户端不发 mechListMIC 时不能强制要求，服务端也不主动回一个。
func TestMechListMICAbsent(t *testing.T) {
	final, _, err := micHandshake(t, false, nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	if final.State != NegAcceptCompleted {
		t.Fatalf("negState = %v", final.State)
	}
	if len(final.MechListMIC) != 0 {
		t.Errorf("客户端没带 MIC，服务端不应回 MIC，got % x", final.MechListMIC)
	}
}

// MechTypesDER 必须是客户端原样发来的字节，而不是我们重新编码的结果 ——
// 长度域写法不同就会算出不同的 MIC。
func TestMechTypesDERPreservesClientBytes(t *testing.T) {
	// 用长型长度（0x81 0x0c）编码本可用短型表示的 SEQUENCE。
	inner := derTLV(tagOID, oidNTLMSSP)
	seq := append([]byte{tagSequence, 0x81, byte(len(inner))}, inner...)
	in := derTLV(tagApplication0, append(derTLV(tagOID, oidSPNEGO),
		derTLV(tagContext0, derTLV(tagSequence, derTLV(tagContext0, seq)))...))

	tok, err := ParseSPNEGO(in)
	if err != nil {
		t.Fatalf("ParseSPNEGO: %v", err)
	}
	if !bytes.Equal(tok.MechTypesDER, seq) {
		t.Fatalf("MechTypesDER = % x, want 客户端原始字节 % x", tok.MechTypesDER, seq)
	}
}
