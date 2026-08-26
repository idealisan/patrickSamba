package server

import (
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// bh5-F6：VALIDATE_NEGOTIATE_INFO 复核失败后，服务端不能只回 ACCESS_DENIED
// 就完事 —— MS-SMB2 §3.3.5.15.12 原文 "the server MUST terminate the
// transport connection"；Samba 的 fsctl_validate_neg_info 每处失配都置
// *disconnect = true。不断连的话，被篡改的客户端收到错误后仍能继续用这条
// 连接发命令，防降级复核的威慑减半。
//
// 实现方式：connection.go 在分发完一条消息后检查「VNI 请求 + 最终状态为
// STATUS_ACCESS_DENIED」，置 vniDropPending；响应帧写出后立即断开。

// buildVNIBody 编码一个 FSCTL_VALIDATE_NEGOTIATE_INFO 请求体。
func buildVNIBody(t *testing.T, guid [16]byte, caps wire.Capabilities,
	mode wire.SecurityMode, dialects []wire.Dialect) []byte {
	t.Helper()
	in := &wire.ValidateNegotiateInfoRequest{
		ClientGUID:   guid,
		Capabilities: caps,
		SecurityMode: mode,
		Dialects:     dialects,
	}
	body, err := in.Encode()
	if err != nil {
		t.Fatalf("编码 VNI 请求体: %v", err)
	}
	req := &wire.IoctlRequest{
		CtlCode:           wire.FSCTLValidateNegotiateInfo,
		Flags:             wire.IoctlIsFSCTL,
		MaxOutputResponse: 4096,
		Input:             body,
	}
	out, err := req.Append(nil)
	if err != nil {
		t.Fatalf("编码 IOCTL 请求: %v", err)
	}
	return out // Append(nil) 产出完整报文体（固定部分 + Input）
}

// vniTestState 填好连接上 VNI 复核要用的协商存档字段。
func vniTestState(t *testing.T, c *Connection) [16]byte {
	t.Helper()
	var guid [16]byte
	copy(guid[:], []byte("0123456789abcdef"))
	establishSession(t, c, false)
	c.state.ClientGUID = guid
	c.state.ClientCapabilities = wire.Capabilities(0x00000001)
	c.state.ClientSecurityMode = wire.NegotiateSigningEnabled
	c.state.ServerCapabilities = wire.Capabilities(0x00000001)
	c.state.ServerSecurityMode = wire.NegotiateSigningEnabled
	return guid
}

// TestVNIRejectMarksDisconnect：复核失败（GUID 被篡改）→ 响应回
// STATUS_ACCESS_DENIED，且连接被标记为「响应后必须终止」。
func TestVNIRejectMarksDisconnect(t *testing.T) {
	c := newTestConn()
	guid := vniTestState(t, c)

	tampered := guid
	tampered[0] ^= 0xFF
	frame := buildSessionMsg(wire.CommandIoctl, 1, 1, 0)
	msg := append(frame, buildVNIBody(t, tampered,
		c.state.ClientCapabilities, c.state.ClientSecurityMode,
		[]wire.Dialect{0x0202, 0x0210, 0x0300})...)

	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.AccessDenied {
		t.Fatalf("篡改的 VNI 期望 STATUS_ACCESS_DENIED，实际 %s", got)
	}
	if !c.vniDropPending {
		t.Fatal("VNI 复核失败后必须标记断连（MS-SMB2 §3.3.5.15.12 MUST terminate）")
	}
}

// TestVNISuccessDoesNotMarkDisconnect：忠实重放的 VNI 正常回显，不断连。
func TestVNISuccessDoesNotMarkDisconnect(t *testing.T) {
	c := newTestConn()
	guid := vniTestState(t, c)

	frame := buildSessionMsg(wire.CommandIoctl, 1, 1, 0)
	msg := append(frame, buildVNIBody(t, guid,
		c.state.ClientCapabilities, c.state.ClientSecurityMode,
		[]wire.Dialect{0x0202, 0x0210, 0x0300})...)

	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.Success {
		t.Fatalf("忠实的 VNI 应成功，实际 %s", got)
	}
	if c.vniDropPending {
		t.Fatal("校验通过的 VNI 不应触发断连")
	}
}

// TestOtherAccessDeniedDoesNotMarkDisconnect：断连标记只属于 VNI 复核失败，
// 其他来源的 ACCESS_DENIED（这里是未签名请求撞上强制签名）不得误伤。
func TestOtherAccessDeniedDoesNotMarkDisconnect(t *testing.T) {
	c := newTestConn()
	sess := establishSession(t, c, true) // 强制签名

	msg := buildSessionMsg(wire.CommandEcho, 1, sess.ID, 0) // 未签名
	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if got := respStatus(t, resp); got != status.AccessDenied {
		t.Fatalf("未签名请求期望 STATUS_ACCESS_DENIED，实际 %s", got)
	}
	if c.vniDropPending {
		t.Fatal("普通 ACCESS_DENIED 不应触发断连标记")
	}
}

// TestVNIRejectHelperClassifies：分类函数只认「IOCTL + FSCTL_VALIDATE_NEGOTIATE_INFO」。
func TestVNIRejectHelperClassifies(t *testing.T) {
	vni := append(buildSessionMsg(wire.CommandIoctl, 1, 0, 0),
		buildVNIBody(t, [16]byte{}, 0, 0, []wire.Dialect{0x0202})...)
	hdr, err := wire.ParseHeader(vni)
	if err != nil {
		t.Fatalf("解析头: %v", err)
	}
	if !isValidateNegotiate(hdr, vni) {
		t.Fatal("VNI 请求应被识别")
	}

	echo := buildSessionMsg(wire.CommandEcho, 2, 0, 0)
	hdrEcho, _ := wire.ParseHeader(echo)
	if isValidateNegotiate(hdrEcho, echo) {
		t.Fatal("ECHO 不是 VNI")
	}

	// IOCTL 但控制码不同（SRV_ENUMERATE_SNAPSHOTS 的最小合法输入难以构造，
	// 这里直接用一个畸形 body —— 分类函数对解析失败必须按「不是」处理）。
	bad := buildSessionMsg(wire.CommandIoctl, 3, 0, 0)
	hdrBad, _ := wire.ParseHeader(bad)
	if isValidateNegotiate(hdrBad, append(bad, []byte{0x01}...)) {
		t.Fatal("畸形 IOCTL 不应被识别为 VNI")
	}
}
