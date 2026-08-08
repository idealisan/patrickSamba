package auth

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/config"
)

func testProvider(t *testing.T, users []config.User, allowGuest, allowAnon bool) *NTLMProvider {
	t.Helper()
	store, err := NewStaticStore(config.Auth{AllowGuest: allowGuest, Users: users}, "WORKGROUP")
	if err != nil {
		t.Fatalf("构造账户表: %v", err)
	}
	return NewNTLMProvider(Options{
		Store:          store,
		ServerName:     "STUPIDSAMBA",
		DomainName:     "WORKGROUP",
		AllowAnonymous: allowAnon,
	})
}

// clientNTLMv2 用给定口令生成一条 NTLMv2 的 NtChallengeResponse。
//
// 这是**客户端侧**的计算，只在测试里出现，用来端到端验证服务端校验逻辑。
func clientNTLMv2(password, user, domain string, serverChallenge [8]byte,
	targetInfo AvPairs, clientChallenge [8]byte) (ntResponse []byte, ntowf [16]byte) {

	ntowf = NTOWFv2(NTHash(password), user, domain)

	var temp []byte
	temp = append(temp, 0x01, 0x01, 0, 0, 0, 0, 0, 0) // RespType/HiRespType/Reserved
	temp = append(temp, encodeFiletime(0)...)         // Timestamp
	temp = append(temp, clientChallenge[:]...)
	temp = append(temp, 0, 0, 0, 0) // Reserved3
	temp = append(temp, targetInfo.Encode()...)
	temp = append(temp, 0, 0, 0, 0) // 结尾 Z(4)

	proof := ComputeNTProofStr(ntowf, serverChallenge, temp)
	ntResponse = append(append([]byte(nil), proof[:]...), temp...)
	return ntResponse, ntowf
}

// unwrap 从服务端返回的 token 里取出 NTLM 报文。
func unwrap(t *testing.T, out []byte) []byte {
	t.Helper()
	if IsNTLMSSP(out) {
		return out
	}
	tok, err := ParseSPNEGO(out)
	if err != nil {
		t.Fatalf("解析服务端 token: %v", err)
	}
	return tok.Token
}

// 端到端：SPNEGO + NTLMv2 成功认证，并核对 ExportedSessionKey。
func TestProviderNTLMv2Success(t *testing.T) {
	const pw = "s3cret"
	p := testProvider(t, []config.User{{Name: "alice", Password: pw, UID: 1000, GID: 1000}}, false, false)
	ctx := p.NewContext()

	// 第一轮：客户端 NEGOTIATE，包在 SPNEGO negTokenInit 里。
	neg := (&NegotiateMessage{
		Flags: NegotiateUnicode | NegotiateRequestTarget | NegotiateNTLM |
			NegotiateExtendedSessionSecurity | NegotiateAlwaysSign | NegotiateKeyExch |
			NegotiateSign | Negotiate128 | Negotiate56,
		Workstation: "CLIENT",
	}).Marshal()

	mechTypes := derTLV(tagContext0, derTLV(tagSequence, derTLV(tagOID, oidNTLMSSP)))
	mechToken := derTLV(tagContext2, derTLV(tagOctetString, neg))
	initTok := derTLV(tagApplication0, append(derTLV(tagOID, oidSPNEGO),
		derTLV(tagContext0, derTLV(tagSequence, append(mechTypes, mechToken...)))...))

	out, done, err := ctx.Step(initTok)
	if err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	if done {
		t.Fatal("第一轮不应完成")
	}
	if ctx.Identity() != nil {
		t.Error("未完成时 Identity 应为 nil")
	}

	ch, err := ParseChallengeMessage(unwrap(t, out))
	if err != nil {
		t.Fatalf("解析 CHALLENGE: %v", err)
	}
	if ch.TargetName != "STUPIDSAMBA" {
		t.Errorf("TargetName = %q", ch.TargetName)
	}
	if !ch.Flags.Has(NegotiateExtendedSessionSecurity) || !ch.Flags.Has(NegotiateTargetInfo) {
		t.Errorf("CHALLENGE Flags = %08x", uint32(ch.Flags))
	}
	if !ch.Flags.Has(NegotiateKeyExch) {
		t.Error("客户端请求了 KEY_EXCH，服务端必须镜像该位")
	}
	// TargetInfo 必须带齐 4 个名字项 + Timestamp。
	for _, id := range []AvID{MsvAvNbDomainName, MsvAvNbComputerName,
		MsvAvDnsDomainName, MsvAvDnsComputerName, MsvAvTimestamp} {
		if _, ok := ch.TargetInfo.Get(id); !ok {
			t.Errorf("TargetInfo 缺少 AvId=0x%04x", uint16(id))
		}
	}

	// 第二轮：客户端 AUTHENTICATE。
	clientChallenge := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	ntResp, ntowf := clientNTLMv2(pw, "alice", "WORKGROUP", ch.ServerChallenge,
		ch.TargetInfo, clientChallenge)

	var proof [16]byte
	copy(proof[:], ntResp[:16])
	sbk := SessionBaseKey(ntowf, proof)
	randomSessionKey := bytes.Repeat([]byte{0x77}, 16)
	encKey, err := ExportedSessionKey(sbk, NegotiateKeyExch, randomSessionKey)
	if err != nil {
		t.Fatalf("RC4: %v", err)
	}

	auth := &AuthenticateMessage{
		NTResponse:                ntResp,
		DomainName:                "WORKGROUP",
		UserName:                  "alice",
		Workstation:               "CLIENT",
		EncryptedRandomSessionKey: encKey[:],
		Flags:                     ch.Flags,
	}
	respTok := derTLV(tagContext1, derTLV(tagSequence,
		derTLV(tagContext2, derTLV(tagOctetString, auth.Marshal()))))

	out, done, err = ctx.Step(respTok)
	if err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	if !done {
		t.Fatal("第二轮应完成")
	}
	// 最终 token 应为 accept-completed。
	final, err := ParseSPNEGO(out)
	if err != nil || final.State != NegAcceptCompleted {
		t.Errorf("最终 negState = %v (err=%v)", final.State, err)
	}

	id := ctx.Identity()
	if id == nil || id.User != "alice" || id.UID != 1000 || id.GID != 1000 {
		t.Fatalf("Identity = %+v", id)
	}
	if id.Guest || id.Anonymous {
		t.Error("不应是 guest/匿名会话")
	}
	// ExportedSessionKey 必须等于客户端的 RandomSessionKey。
	if !bytes.Equal(ctx.SessionKey(), randomSessionKey) {
		t.Errorf("SessionKey = %x, want %x", ctx.SessionKey(), randomSessionKey)
	}
}

// 口令错误：不开 guest 时必须返回 ErrLogonFailure。
func TestProviderWrongPassword(t *testing.T) {
	p := testProvider(t, []config.User{{Name: "alice", Password: "right"}}, false, false)
	ch, ctx := handshakeToChallenge(t, p)

	ntResp, _ := clientNTLMv2("wrong", "alice", "WORKGROUP", ch.ServerChallenge,
		ch.TargetInfo, [8]byte{9, 9, 9, 9, 9, 9, 9, 9})
	auth := &AuthenticateMessage{
		NTResponse: ntResp, UserName: "alice", DomainName: "WORKGROUP",
		Flags: NegotiateUnicode,
	}
	if _, _, err := ctx.Step(rawToken(auth.Marshal())); err != ErrLogonFailure {
		t.Fatalf("err = %v, want ErrLogonFailure", err)
	}
	if ctx.Identity() != nil {
		t.Error("失败后 Identity 必须为 nil")
	}
}

// 用户不存在同样是 ErrLogonFailure（不泄露用户是否存在）。
func TestProviderNoSuchUser(t *testing.T) {
	p := testProvider(t, []config.User{{Name: "alice", Password: "right"}}, false, false)
	ch, ctx := handshakeToChallenge(t, p)

	ntResp, _ := clientNTLMv2("whatever", "bob", "WORKGROUP", ch.ServerChallenge,
		ch.TargetInfo, [8]byte{})
	auth := &AuthenticateMessage{
		NTResponse: ntResp, UserName: "bob", DomainName: "WORKGROUP", Flags: NegotiateUnicode,
	}
	if _, _, err := ctx.Step(rawToken(auth.Marshal())); err != ErrLogonFailure {
		t.Fatalf("err = %v, want ErrLogonFailure", err)
	}
}

// 开了 guest 时口令错误降级为 guest 会话，session key 全零。
func TestProviderGuestFallback(t *testing.T) {
	p := testProvider(t, []config.User{{Name: "alice", Password: "right"}}, true, false)
	ch, ctx := handshakeToChallenge(t, p)

	ntResp, _ := clientNTLMv2("wrong", "alice", "WORKGROUP", ch.ServerChallenge,
		ch.TargetInfo, [8]byte{})
	auth := &AuthenticateMessage{
		NTResponse: ntResp, UserName: "alice", DomainName: "WORKGROUP", Flags: NegotiateUnicode,
	}
	_, done, err := ctx.Step(rawToken(auth.Marshal()))
	if err != nil || !done {
		t.Fatalf("guest 降级失败: err=%v done=%v", err, done)
	}
	id := ctx.Identity()
	if id == nil || !id.Guest {
		t.Fatalf("Identity = %+v, 期望 Guest", id)
	}
	if !bytes.Equal(ctx.SessionKey(), make([]byte, SessionKeyLen)) {
		t.Error("guest 会话的 session key 必须全零")
	}
}

// 匿名会话。
func TestProviderAnonymous(t *testing.T) {
	p := testProvider(t, nil, false, true)
	_, ctx := handshakeToChallenge(t, p)

	auth := &AuthenticateMessage{Flags: NegotiateUnicode | NegotiateAnonymous}
	_, done, err := ctx.Step(rawToken(auth.Marshal()))
	if err != nil || !done {
		t.Fatalf("匿名认证失败: err=%v done=%v", err, done)
	}
	if id := ctx.Identity(); id == nil || !id.Anonymous {
		t.Fatalf("Identity = %+v, 期望 Anonymous", id)
	}

	// 不允许匿名时必须拒绝。
	p2 := testProvider(t, nil, false, false)
	_, ctx2 := handshakeToChallenge(t, p2)
	if _, _, err := ctx2.Step(rawToken(auth.Marshal())); err != ErrLogonFailure {
		t.Errorf("err = %v, want ErrLogonFailure", err)
	}
}

// MIC：客户端在 MsvAvFlags 里声明带 MIC 时服务端必须校验。
func TestProviderMIC(t *testing.T) {
	const pw = "s3cret"
	p := testProvider(t, []config.User{{Name: "alice", Password: pw}}, false, false)

	build := func(t *testing.T, tamper bool) error {
		t.Helper()
		ctx := p.NewContext().(*ntlmContext)
		neg := (&NegotiateMessage{Flags: NegotiateUnicode | NegotiateNTLM}).Marshal()
		out, _, err := ctx.Step(neg)
		if err != nil {
			t.Fatalf("NEGOTIATE: %v", err)
		}
		ch, err := ParseChallengeMessage(out)
		if err != nil {
			t.Fatalf("CHALLENGE: %v", err)
		}

		// 客户端在 TargetInfo 里追加 MsvAvFlags(MIC present)。
		info := append(AvPairs(nil), ch.TargetInfo...)
		var fb [4]byte
		binary.LittleEndian.PutUint32(fb[:], AvFlagMICPresent)
		info = append(info, AvPair{ID: MsvAvFlags, Value: fb[:]})

		ntResp, ntowf := clientNTLMv2(pw, "alice", "WORKGROUP", ch.ServerChallenge,
			info, [8]byte{7, 7, 7, 7, 7, 7, 7, 7})
		var proof [16]byte
		copy(proof[:], ntResp[:16])
		esk := SessionBaseKey(ntowf, proof) // 未置 KEY_EXCH，ESK = SessionBaseKey

		auth := &AuthenticateMessage{
			NTResponse: ntResp, UserName: "alice", DomainName: "WORKGROUP",
			Flags: NegotiateUnicode, MICPresent: true,
		}
		// MIC 计算时 MIC 字段必须为零。
		zeroed := auth.Marshal()
		mic := ComputeMIC(esk, neg, out, zeroed)
		if tamper {
			mic[0] ^= 0xFF
		}
		auth.MIC = mic
		_, _, err = ctx.Step(auth.Marshal())
		return err
	}

	if err := build(t, false); err != nil {
		t.Fatalf("正确的 MIC 应通过: %v", err)
	}
	if err := build(t, true); err != ErrLogonFailure {
		t.Fatalf("被篡改的 MIC 应拒绝, got %v", err)
	}
}

// 状态机：跳过 CHALLENGE 直接发 AUTHENTICATE 必须被拒。
func TestProviderStateMachine(t *testing.T) {
	p := testProvider(t, []config.User{{Name: "alice", Password: "x"}}, false, false)
	ctx := p.NewContext()

	auth := &AuthenticateMessage{UserName: "alice", NTResponse: bytes.Repeat([]byte{1}, 40)}
	if _, _, err := ctx.Step(auth.Marshal()); err != ErrInvalidToken {
		t.Errorf("跳步应返回 ErrInvalidToken, got %v", err)
	}

	// 空 token。
	if _, _, err := p.NewContext().Step(nil); err != ErrInvalidToken {
		t.Errorf("空 token 应返回 ErrInvalidToken, got %v", err)
	}
}

// 客户端只列 Kerberos：应回 ErrMechUnsupported。
func TestProviderMechUnsupported(t *testing.T) {
	p := testProvider(t, nil, false, false)
	mechTypes := derTLV(tagContext0, derTLV(tagSequence, derTLV(tagOID, oidKerberos5)))
	in := derTLV(tagApplication0, append(derTLV(tagOID, oidSPNEGO),
		derTLV(tagContext0, derTLV(tagSequence, mechTypes))...))
	if _, _, err := p.NewContext().Step(in); err != ErrMechUnsupported {
		t.Errorf("err = %v, want ErrMechUnsupported", err)
	}
}

// 客户端同时列 Kerberos 与 NTLM 且先发 Kerberos token：
// 服务端应回 accept-incomplete + supportedMech=NTLM，让它改用 NTLM。
func TestProviderMechFallback(t *testing.T) {
	p := testProvider(t, nil, false, false)
	mechList := append(derTLV(tagOID, oidKerberos5), derTLV(tagOID, oidNTLMSSP)...)
	mechTypes := derTLV(tagContext0, derTLV(tagSequence, mechList))
	krbToken := derTLV(tagContext2, derTLV(tagOctetString, []byte{0x60, 0x01, 0x00}))
	in := derTLV(tagApplication0, append(derTLV(tagOID, oidSPNEGO),
		derTLV(tagContext0, derTLV(tagSequence, append(mechTypes, krbToken...)))...))

	out, done, err := p.NewContext().Step(in)
	if err != nil || done {
		t.Fatalf("err=%v done=%v", err, done)
	}
	tok, err := ParseSPNEGO(out)
	if err != nil {
		t.Fatalf("解析回应: %v", err)
	}
	if tok.State != NegAcceptIncomplete {
		t.Errorf("negState = %v", tok.State)
	}
	if len(tok.MechTypes) != 1 || !tok.HasMech(oidNTLMSSP) {
		t.Errorf("supportedMech = %x", tok.MechTypes)
	}
}

// InitialToken 每次返回独立副本，调用方改动不影响 Provider。
func TestInitialTokenIsCopy(t *testing.T) {
	p := testProvider(t, nil, false, false)
	a := p.InitialToken()
	a[0] = 0x00
	b := p.InitialToken()
	if b[0] != tagApplication0 {
		t.Error("InitialToken 返回的不是副本")
	}
}

// ---------------------------------------------------------------- 测试辅助

// handshakeToChallenge 跑完第一轮，返回 CHALLENGE 与握手上下文。
func handshakeToChallenge(t *testing.T, p *NTLMProvider) (*ChallengeMessage, Context) {
	t.Helper()
	ctx := p.NewContext()
	neg := (&NegotiateMessage{Flags: NegotiateUnicode | NegotiateNTLM}).Marshal()
	out, done, err := ctx.Step(neg)
	if err != nil || done {
		t.Fatalf("NEGOTIATE 轮次失败: err=%v done=%v", err, done)
	}
	ch, err := ParseChallengeMessage(out)
	if err != nil {
		t.Fatalf("解析 CHALLENGE: %v", err)
	}
	return ch, ctx
}

// rawToken 直接返回裸 NTLM 报文（对应客户端不用 SPNEGO 外壳的情形）。
func rawToken(b []byte) []byte { return b }
