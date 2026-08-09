package server

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// smb1NegotiateFrame 拼一条 SMB1 SMB_COM_NEGOTIATE（MS-CIFS §2.2.4.52）。
//
// 布局：32 字节 SMB1 头 + WordCount(1)=0 + ByteCount(2, 小端) +
// 若干个 `0x02 + NUL 结尾 ASCII 方言串`。
func smb1NegotiateFrame(dialects []string) []byte {
	var body bytes.Buffer
	for _, d := range dialects {
		body.WriteByte(0x02)
		body.WriteString(d)
		body.WriteByte(0x00)
	}

	frame := make([]byte, 32)
	copy(frame, []byte{0xFF, 'S', 'M', 'B'})
	frame[4] = 0x72 // SMB_COM_NEGOTIATE

	frame = append(frame, 0x00) // WordCount = 0
	frame = binary.LittleEndian.AppendUint16(frame, uint16(body.Len()))
	return append(frame, body.Bytes()...)
}

// TestSMB1NegotiateOnlySMB2002RejectedWhenEncryptionRequired 覆盖**第二个协商
// 出口**上的加密绕过。
//
// handleNegotiate 里的 fail closed 只管 SMB2 NEGOTIATE。SMB1 多协议协商里
// 客户端若只报 "SMB 2.002"（不带 "SMB 2.???" 通配），AppendSMB1NegotiateReply
// 会就地把方言定型为 2.0.2 并置 NegotiateDone —— **永远不会进 handleNegotiate**，
// 于是 `EncryptionRequired && Cipher == 0` 一次都不执行，客户端换个入口照样
// 拿到 2.0.2 明文会话。
func TestSMB1NegotiateOnlySMB2002RejectedWhenEncryptionRequired(t *testing.T) {
	c := newEncTestConn(t, true)

	_, err := c.handleSMB1(smb1NegotiateFrame([]string{"NT LM 0.12", "SMB 2.002"}))
	if err == nil {
		t.Fatal("要求加密时只报 SMB 2.002 必须被拒绝（2.0.2 没有任何加密能力）")
	}
	if !errors.Is(err, errSMB1Refused) {
		t.Fatalf("应当以 errSMB1Refused 关连接，实际: %v", err)
	}
	if c.state.NegotiateDone {
		t.Fatal("被拒的协商不应把连接标记为已协商")
	}
	if c.state.Dialect != 0 {
		t.Fatalf("被拒的协商不应定型方言，实际 %s", c.state.Dialect)
	}
}

// TestSMB1NegotiateOnlySMB2002AllowedWithoutEncryptionRequired：不要求加密时
// 这条路径必须照常工作，别把 impacket 这类默认走 SMB1 协商的客户端误伤了。
func TestSMB1NegotiateOnlySMB2002AllowedWithoutEncryptionRequired(t *testing.T) {
	c := newEncTestConn(t, false)

	out, err := c.handleSMB1(smb1NegotiateFrame([]string{"NT LM 0.12", "SMB 2.002"}))
	if err != nil {
		t.Fatalf("不要求加密时只报 SMB 2.002 应当正常协商，实际: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("应当回一条 SMB2 NEGOTIATE 响应")
	}
	if !c.state.NegotiateDone || c.state.Dialect != dialect.SMB202 {
		t.Fatalf("应就地完成 2.0.2 协商，实际 done=%v dialect=%s",
			c.state.NegotiateDone, c.state.Dialect)
	}
}

// TestSMB1NegotiateWildcardDeferredToSMB2Negotiate：带 "SMB 2.???" 通配的那条
// 分支**刻意放过**——它只回一个 0x02FF 占位应答，不定型方言、不置 NegotiateDone，
// 客户端随后必然再发一个真正的 SMB2 NEGOTIATE，由 handleNegotiate 的 fail
// closed 拦住。本用例把这两步真的走一遍，证明"放过"不等于"漏了"。
func TestSMB1NegotiateWildcardDeferredToSMB2Negotiate(t *testing.T) {
	c := newEncTestConn(t, true)

	out, err := c.handleSMB1(smb1NegotiateFrame([]string{"NT LM 0.12", "SMB 2.002", "SMB 2.???"}))
	if err != nil {
		t.Fatalf("通配分支应当放过（等下一发真 NEGOTIATE），实际: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("通配分支应当回 0x02FF 占位应答")
	}
	if c.state.NegotiateDone {
		t.Fatal("通配分支不应把连接标记为已协商，否则后续真 NEGOTIATE 会被当成重协商拒掉")
	}

	// 第二步：客户端发真正的 SMB2 NEGOTIATE，压到 2.1 —— 必须被拦。
	resp, err := c.handleSMB2Chain(negotiateFrame(t,
		[]wire.Dialect{wire.Dialect(dialect.SMB210)}, 0, false))
	if err != nil {
		t.Fatalf("处理 NEGOTIATE 出错: %v", err)
	}
	if got := respStatus(t, resp); got != status.AccessDenied {
		t.Fatalf("通配升级后的低方言协商仍须 fail closed，实际 %s", got)
	}
}

// TestSessionSetupBackstopRejectsMissingCipher 覆盖 session_setup 的**兜底**。
//
// 这里手工制造"协商层漏了"的状态（已协商、EncryptionRequired、但 Cipher==0），
// 断言会话建立阶段直接 ACCESS_DENIED，而不是像历史实现那样
// （`EncryptionRequired && Cipher != 0`）静默跳过加密、以明文继续。
func TestSessionSetupBackstopRejectsMissingCipher(t *testing.T) {
	c := newEncTestConn(t, true)

	// 绕过协商层，直接把连接置于"已协商 3.0 但没有 Cipher"的状态。
	c.state.Dialect = dialect.SMB300
	c.state.NegotiateDone = true
	c.state.Cipher = 0

	cl := newNTLMTestClient("alice", "secret", "WORKGROUP")
	resp, st := cl.round1(t, c)
	if st != status.MoreProcessingRequired {
		t.Fatalf("轮次 1 应回 MORE_PROCESSING_REQUIRED，实际 %s", st)
	}
	_, st = cl.round2(t, c, resp)
	if st != status.AccessDenied {
		t.Fatalf("要求加密却没有 Cipher 时必须兜底拒绝，实际 %s", st)
	}
}

// TestEncryptionRequiredSMB30SetsEncryptDataFlag：3.0 + CAP_ENCRYPTION +
// encryption_required 走完整的 NEGOTIATE + NTLMv2 SESSION_SETUP，
// 断言最终响应的 SessionFlags 带 SMB2_SESSION_FLAG_ENCRYPT_DATA。
//
// 这是"加密真的生效了"的唯一硬证据：Cipher 协商出来只说明算法选定，
// 只有这个标志位才让客户端把后续流量塞进 TRANSFORM_HEADER。
func TestEncryptionRequiredSMB30SetsEncryptDataFlag(t *testing.T) {
	c := newEncTestConn(t, true)

	resp, err := c.handleSMB2Chain(negotiateFrame(t,
		[]wire.Dialect{wire.Dialect(dialect.SMB300)}, dialect.CapEncryption, false))
	if err != nil {
		t.Fatalf("处理 NEGOTIATE 出错: %v", err)
	}
	if got := respStatus(t, resp); got != status.Success {
		t.Fatalf("3.0 宣告 CAP_ENCRYPTION 应协商成功，实际 %s", got)
	}
	if c.state.Cipher != wire.CipherAES128CCM {
		t.Fatalf("3.0 的算法应为 AES-128-CCM，实际 %#x", c.state.Cipher)
	}

	cl := newNTLMTestClient("alice", "secret", "WORKGROUP")
	r1, st := cl.round1(t, c)
	if st != status.MoreProcessingRequired {
		t.Fatalf("轮次 1 应回 MORE_PROCESSING_REQUIRED，实际 %s", st)
	}
	r2, st := cl.round2(t, c, r1)
	if st != status.Success {
		t.Fatalf("会话建立应成功，实际 %s", st)
	}

	ssr, err := wire.ParseSessionSetupResponse(r2)
	if err != nil {
		t.Fatalf("解析 SESSION_SETUP 响应失败: %v", err)
	}
	if ssr.SessionFlags&wire.SessionFlagEncryptData == 0 {
		t.Fatalf("要求加密且协商出算法时，SessionFlags 必须带 EncryptData，实际 %#x",
			ssr.SessionFlags)
	}
}

// ---------------------------------------------------------------- NTLM 测试客户端
//
// 服务端的 ParseSPNEGO 接受**裸 NTLMSSP**（裸模式下跳过 mechListMIC 校验），
// 所以这里省掉 SPNEGO 外壳，直接发 Type1/Type3。
// 只用于加密标志位的断言，NTLM 本身的正确性由 internal/auth 的测试覆盖。

type ntlmTestClient struct {
	user, pass, domain string
	sessID             uint64
	msgID              uint64
}

func newNTLMTestClient(user, pass, domain string) *ntlmTestClient {
	return &ntlmTestClient{user: user, pass: pass, domain: domain}
}

func (cl *ntlmTestClient) nextMsgID() uint64 {
	id := cl.msgID
	cl.msgID++
	return id
}

// round1 发 NTLMSSP NEGOTIATE（Type1），返回服务端响应与状态。
func (cl *ntlmTestClient) round1(t *testing.T, c *Connection) ([]byte, status.Status) {
	t.Helper()

	neg := (&auth.NegotiateMessage{Flags: ntlmTestFlags()}).Marshal()
	resp := cl.send(t, c, neg, 0)
	h, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析响应头失败: %v", err)
	}
	cl.sessID = h.SessionID
	return resp, status.Status(h.Status)
}

// round2 根据服务端 CHALLENGE 构造 NTLMSSP AUTHENTICATE（Type3）并发出。
func (cl *ntlmTestClient) round2(t *testing.T, c *Connection, resp1 []byte) ([]byte, status.Status) {
	t.Helper()

	ssr1, err := wire.ParseSessionSetupResponse(resp1)
	if err != nil {
		t.Fatalf("解析轮次 1 响应失败: %v", err)
	}
	ch, err := auth.ParseChallengeMessage(ssr1.SecurityBuffer)
	if err != nil {
		t.Fatalf("解析 CHALLENGE 失败: %v", err)
	}

	resp := cl.send(t, c, cl.authenticate(ch), cl.sessID)
	h, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析响应头失败: %v", err)
	}
	return resp, status.Status(h.Status)
}

func (cl *ntlmTestClient) send(t *testing.T, c *Connection, blob []byte, sessID uint64) []byte {
	t.Helper()

	req := &wire.SessionSetupRequest{
		SecurityBuffer: blob,
		SecurityMode:   wire.NegotiateSigningEnabled,
	}
	body, err := req.Append(nil)
	if err != nil {
		t.Fatalf("编码 SESSION_SETUP 请求失败: %v", err)
	}
	h := wire.Header{
		Command:   wire.CommandSessionSetup,
		Credits:   1,
		MessageID: cl.nextMsgID(),
		SessionID: sessID,
	}
	msg := append(h.Append(nil), body...)

	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("处理 SESSION_SETUP 出错: %v", err)
	}
	return resp
}

// authenticate 构造 NTLMv2 AUTHENTICATE（MS-NLMP §3.3.2）。
func (cl *ntlmTestClient) authenticate(ch *auth.ChallengeMessage) []byte {
	ntowf := auth.NTOWFv2(auth.NTHash(cl.pass), cl.user, cl.domain)

	cc := make([]byte, 8)
	_, _ = rand.Read(cc)

	var blob bytes.Buffer
	blob.WriteByte(0x01)           // RespType
	blob.WriteByte(0x01)           // HiRespType
	blob.Write([]byte{0, 0})       // Reserved1
	blob.Write([]byte{0, 0, 0, 0}) // Reserved2
	blob.Write(binary.LittleEndian.AppendUint64(nil, vfs.TimeToFiletime(time.Now())))
	blob.Write(cc)
	blob.Write([]byte{0, 0, 0, 0}) // Reserved3
	blob.Write(ch.TargetInfo.Encode())
	b := blob.Bytes()

	proof := auth.ComputeNTProofStr(ntowf, ch.ServerChallenge, b)
	ntResp := append(append([]byte{}, proof[:]...), b...)

	return (&auth.AuthenticateMessage{
		LMResponse:  cc, // NTLMv2 的 LM 响应即 8 字节 client challenge
		NTResponse:  ntResp,
		DomainName:  cl.domain,
		UserName:    cl.user,
		Workstation: "AUDIT",
		Flags:       ntlmTestFlags() | auth.NegotiateTargetTypeServer,
	}).Marshal()
}

func ntlmTestFlags() auth.NegotiateFlags {
	return auth.NegotiateUnicode |
		auth.NegotiateRequestTarget |
		auth.NegotiateNTLM |
		auth.NegotiateExtendedSessionSecurity |
		auth.NegotiateTargetInfo |
		auth.NegotiateAlwaysSign |
		auth.Negotiate128 |
		auth.NegotiateSign
}
