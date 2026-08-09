package server

import (
	"testing"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// testSigningKey 是单测用的固定签名密钥（长度必须是 AES-128 的 16 字节）。
var testSigningKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F,
}

// establishSession 造一个已认证、要求签名的会话，跳过真正的 NTLM 握手
// （认证本身由 auth agent 的测试覆盖，这里只关心签名强制）。
func establishSession(t *testing.T, c *Connection, signingRequired bool) *command.Session {
	t.Helper()

	c.state.Dialect = dialect.SMB300
	c.state.NegotiateDone = true

	sess, st := c.state.NewSession()
	if st != status.Success {
		t.Fatalf("NewSession 失败: %s", st)
	}
	sess.Establish(&auth.Identity{User: "alice"})
	sess.SetKeys(command.Keys{SessionKey: testSigningKey, SigningKey: testSigningKey})
	sess.SetSigningRequired(signingRequired)
	return sess
}

// signedMsg 构造一条已正确签名的请求。
func signedMsg(t *testing.T, alg crypto.SigningAlgorithm, key []byte,
	cmd wire.Command, msgID, sessionID uint64, body []byte) []byte {
	t.Helper()

	msg := append(buildSessionMsg(cmd, msgID, sessionID, wire.FlagSigned), body...)
	if err := crypto.SignWith(alg, key, msg); err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	return msg
}

// buildSessionMsg 拼一条带 SessionId 的请求头。
func buildSessionMsg(cmd wire.Command, msgID, sessionID uint64, flags wire.Flags) []byte {
	h := wire.Header{
		Command:   cmd,
		MessageID: msgID,
		SessionID: sessionID,
		Credits:   1,
		Flags:     flags,
	}
	return h.Append(nil)
}

func respStatus(t *testing.T, resp []byte) status.Status {
	t.Helper()
	h, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析响应头失败: %v", err)
	}
	return status.Status(h.Status)
}

// TestNegotiateSignedRejected：MS-SMB2 §3.3.5.2.4 第一句 ——
// NEGOTIATE 请求置了 SMB2_FLAGS_SIGNED 必须回 STATUS_INVALID_PARAMETER
// （协商阶段根本还没有密钥）。
func TestNegotiateSignedRejected(t *testing.T) {
	c := newTestConn()

	// NEGOTIATE 报文体解析失败也会回 INVALID_PARAMETER，为了确认是签名
	// 检查生效，这里只断言"没有被当成正常协商处理"。
	resp, err := c.handleSMB2Chain(
		buildMsg(wire.CommandNegotiate, 0, wire.FlagSigned, nil))
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.InvalidParameter {
		t.Fatalf("带 SIGNED 的 NEGOTIATE 期望 STATUS_INVALID_PARAMETER，实际 %s", got)
	}
	if c.state.NegotiateDone {
		t.Fatal("被拒绝的 NEGOTIATE 不应把连接标记为已协商")
	}
}

// TestUnsignedRejectedWhenSigningRequired：会话要求签名后，
// 未签名的请求必须回 STATUS_ACCESS_DENIED 且**不得继续处理**。
func TestUnsignedRejectedWhenSigningRequired(t *testing.T) {
	c := newTestConn()
	sess := establishSession(t, c, true)

	msg := append(buildSessionMsg(wire.CommandEcho, 1, sess.ID, 0), echoBody()...)
	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.AccessDenied {
		t.Fatalf("未签名请求期望 STATUS_ACCESS_DENIED，实际 %s", got)
	}
	// ERROR Response = 64 头 + 9 字节 ErrorResponse，不能带 ECHO 响应体。
	if len(resp) != wire.HeaderSize+9 {
		t.Fatalf("应当回 ERROR Response（%d 字节），实际 %d 字节",
			wire.HeaderSize+9, len(resp))
	}
}

// TestBadSignatureRejected：签名错误回 STATUS_ACCESS_DENIED。
func TestBadSignatureRejected(t *testing.T) {
	c := newTestConn()
	sess := establishSession(t, c, true)

	msg := signedMsg(t, c.state.SigningAlg(), testSigningKey,
		wire.CommandEcho, 1, sess.ID, echoBody())
	// 篡改签名字段（头偏移 0x30 起 16 字节）。
	msg[0x30] ^= 0xFF

	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.AccessDenied {
		t.Fatalf("签名错误期望 STATUS_ACCESS_DENIED，实际 %s", got)
	}
}

// TestTamperedBodyRejected：报文体被篡改（签名覆盖整条消息）同样拒绝。
func TestTamperedBodyRejected(t *testing.T) {
	c := newTestConn()
	sess := establishSession(t, c, true)

	msg := signedMsg(t, c.state.SigningAlg(), testSigningKey,
		wire.CommandEcho, 1, sess.ID, echoBody())
	msg[wire.HeaderSize+2] ^= 0xFF // 改 ECHO 体的 Reserved

	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.AccessDenied {
		t.Fatalf("报文体被篡改期望 STATUS_ACCESS_DENIED，实际 %s", got)
	}
}

// TestValidSignatureAcceptedAndResponseSigned：验签通过则正常处理，
// 且响应也必须签名（MS-SMB2 §3.3.4.1.1）。
func TestValidSignatureAcceptedAndResponseSigned(t *testing.T) {
	c := newTestConn()
	sess := establishSession(t, c, true)

	msg := signedMsg(t, c.state.SigningAlg(), testSigningKey,
		wire.CommandEcho, 1, sess.ID, echoBody())

	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.Success {
		t.Fatalf("正确签名的 ECHO 期望成功，实际 %s", got)
	}

	h, _ := wire.ParseHeader(resp)
	if !h.IsSigned() {
		t.Fatal("对已验签请求的响应必须置 SMB2_FLAGS_SIGNED")
	}
	if err := crypto.VerifyWith(c.state.SigningAlg(), testSigningKey, resp); err != nil {
		t.Fatalf("响应签名校验失败: %v", err)
	}
}

// TestEncryptedRequestExemptFromSigning：加密消息豁免签名校验
// （MS-SMB2 §3.3.5.2.4 "and the message is not encrypted"）。
//
// 这是曾经的真 bug：开启 SMB3 加密后客户端不会在信封内再签名，
// 若仍按 SigningRequired 拒绝，所有请求都会被打成 ACCESS_DENIED。
func TestEncryptedRequestExemptFromSigning(t *testing.T) {
	c := newTestConn()
	sess := establishSession(t, c, true)

	msg := append(buildSessionMsg(wire.CommandEcho, 1, sess.ID, 0), echoBody()...)
	resp, err := c.handleSMB2Frame(msg, true)
	if err != nil {
		t.Fatalf("handleSMB2Frame 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.Success {
		t.Fatalf("加密消息应豁免签名校验，期望成功，实际 %s", got)
	}
}

// TestSigningRequiredCoversAllCommands：除 NEGOTIATE / SESSION_SETUP 外
// 所有命令都要走验签。这里逐个命令跑一遍，确保没有命令绕过检查。
func TestSigningRequiredCoversAllCommands(t *testing.T) {
	cmds := []wire.Command{
		wire.CommandLogoff, wire.CommandTreeConnect, wire.CommandTreeDisconnect,
		wire.CommandCreate, wire.CommandClose, wire.CommandFlush,
		wire.CommandRead, wire.CommandWrite, wire.CommandLock,
		wire.CommandIoctl, wire.CommandEcho, wire.CommandQueryDirectory,
		wire.CommandChangeNotify, wire.CommandQueryInfo, wire.CommandSetInfo,
		wire.CommandOplockBreak,
	}
	for _, cmd := range cmds {
		c := newTestConn()
		sess := establishSession(t, c, true)

		msg := buildSessionMsg(cmd, 1, sess.ID, 0)
		resp, err := c.handleSMB2Chain(msg)
		if err != nil {
			t.Fatalf("%s: handleSMB2Chain 返回错误: %v", cmd, err)
		}
		if got := respStatus(t, resp); got != status.AccessDenied {
			t.Errorf("%s: 未签名请求期望 STATUS_ACCESS_DENIED，实际 %s", cmd, got)
		}
	}
}

// TestMissingSigningKeyNotSupported：客户端声称已签名但会话没有签名密钥时，
// 规范要求 STATUS_NOT_SUPPORTED（不是 ACCESS_DENIED）。
func TestMissingSigningKeyNotSupported(t *testing.T) {
	c := newTestConn()
	c.state.Dialect = dialect.SMB300
	c.state.NegotiateDone = true

	sess, st := c.state.NewSession()
	if st != status.Success {
		t.Fatalf("NewSession 失败: %s", st)
	}
	sess.Establish(&auth.Identity{User: "guest", Guest: true})
	// 刻意不 SetKeys：guest 会话没有可用的签名密钥。

	msg := append(buildSessionMsg(wire.CommandEcho, 1, sess.ID, wire.FlagSigned),
		echoBody()...)
	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.NotSupported {
		t.Fatalf("无签名密钥期望 STATUS_NOT_SUPPORTED，实际 %s", got)
	}
}

// TestEncryptDataRequiresEncryptedRequest：会话协商了强制加密后，
// 明文请求必须回 STATUS_ACCESS_DENIED（MS-SMB2 §3.3.5.2.9）。
func TestEncryptDataRequiresEncryptedRequest(t *testing.T) {
	c := newTestConn()
	sess := establishSession(t, c, false)
	sess.SetEncryptData(true)

	msg := append(buildSessionMsg(wire.CommandEcho, 1, sess.ID, 0), echoBody()...)
	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.AccessDenied {
		t.Fatalf("强制加密会话的明文请求期望 STATUS_ACCESS_DENIED，实际 %s", got)
	}

	// 同一条请求走加密路径必须放行。
	resp, err = c.handleSMB2Frame(msg, true)
	if err != nil {
		t.Fatalf("handleSMB2Frame 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.Success {
		t.Fatalf("加密请求期望成功，实际 %s", got)
	}
}
