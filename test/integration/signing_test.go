//go:build integration

package integration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// sendTampered 构造一条正常签名、随后被翻掉一个签名字节的请求，发送并读回
// 原始响应（**不**验签：服务端拒签请求时不会给响应签名，用 c.request 会先
// 死在客户端侧的验签上，看不到真正的 NTSTATUS）。
//
// 返回 (响应报文, 本条请求的 MessageID)。
func (c *rawClient) sendTampered(t *testing.T, cmd wire.Command, body []byte) ([]byte, uint64) {
	t.Helper()
	c.msgID++
	id := c.msgID
	h := wire.Header{
		Command:   cmd,
		Credits:   1,
		MessageID: id,
		TreeID:    c.treeID,
		SessionID: c.sessID,
	}
	msg := h.Append(nil)
	msg = append(msg, body...)
	if err := crypto.Sign(uint16(c.dialect), c.signingKey, msg); err != nil {
		t.Fatalf("签名: %v", err)
	}
	// 翻掉 Signature 字段（SMB2 头偏移 0x30 起 16 字节）的一个 bit。
	msg[0x30] ^= 0xFF
	if err := c.writeMessage(msg); err != nil {
		t.Fatalf("发送篡改请求: %v", err)
	}
	resp, err := c.readMessageTimeout(5 * time.Second)
	if err != nil {
		// 服务端选择了断连（MS-SMB2 §3.3.5.2.4 允许的 MAY 分支）。
		return nil, id
	}
	return resp, id
}

// TestSigningEnforced 验证「强制签名」会话下签名校验的规范行为。
//
// MS-SMB2 §3.3.5.2.4 Verifying the Signature 原文：
//
//	"If the signature verification fails, the server MUST fail the request
//	 with the error code STATUS_ACCESS_DENIED. The server MAY also disconnect
//	 the connection as specified in section 3.3.7.1."
//
// 即：回 STATUS_ACCESS_DENIED 是 **MUST**，断开连接只是 **MAY**。
// 本用例此前把 MAY（断连）当成 MUST 来断言，且反过来把 MUST（回错误码）
// 判成「签名校验未生效」，属于**假故障**——服务端行为一直是合规的。
// 现改为断言规范真正要求的那一条。
func TestSigningEnforced(t *testing.T) {
	h := startServer(t, harnessOptions{SigningRequired: true})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()

	// (1) 正常签名的 ECHO 应成功——证明裸客户端的签名确实被服务端接受，
	//     后面那条被拒绝的请求才能归因到「签名被篡改」而非「客户端签错了」。
	if err := c.echo(); err != nil {
		t.Fatalf("合法签名 ECHO 失败: %v", err)
	}

	// (2) 篡改签名的 ECHO：MUST 得到 STATUS_ACCESS_DENIED。
	resp, id := c.sendTampered(t, wire.CommandEcho, (&wire.EchoRequest{}).Append(nil))
	if resp == nil {
		// MAY 分支：服务端直接断连。合规，但没法校验错误码。
		t.Log("服务端对篡改签名选择了断开连接（MS-SMB2 §3.3.5.2.4 的 MAY 分支），跳过错误码断言")
		return
	}
	hdr, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析响应头: %v", err)
	}
	if got := status.Status(hdr.Status); got != status.AccessDenied {
		t.Fatalf("篡改签名的请求得到 status=%#x (%s)，期望 STATUS_ACCESS_DENIED (%#x)",
			hdr.Status, got, uint32(status.AccessDenied))
	}
	if hdr.MessageID != id {
		t.Fatalf("错误响应的 MessageID=%d，期望 %d（响应张冠李戴）", hdr.MessageID, id)
	}
}

// TestSigningEnforcedRejectsSideEffects 证明「拒绝」是**真的拒绝**，
// 而不只是回了个错误码。
//
// 这是本用例存在的理由：单看 STATUS_ACCESS_DENIED 无法排除「服务端先把
// 活干了、再回一个错误码」这种旁路。必须观察共享目录的真实状态。
//
// 用例自带**反向对照**：同一条 CREATE，一次篡改签名、一次合法签名。
//   - 篡改的那次：文件**不得**出现在磁盘上；
//   - 合法的那次：文件**必须**出现。
//
// 没有反向对照的话，「文件不存在」也可能只是因为 CREATE 这条路本身就是坏的，
// 那样这个探针即使永远亮绿也毫无意义。
func TestSigningEnforcedRejectsSideEffects(t *testing.T) {
	h := startServer(t, harnessOptions{SigningRequired: true})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	if err := c.treeConnect(testShare); err != nil {
		t.Fatalf("tree connect: %v", err)
	}

	newCreateBody := func(t *testing.T, name string) []byte {
		t.Helper()
		req := &wire.CreateRequest{
			ImpersonationLevel: wire.ImpersonationImpersonation,
			DesiredAccess:      wire.GenericRead | wire.GenericWrite,
			FileAttributes:     wire.FileAttributeNormal,
			ShareAccess:        wire.ShareRead | wire.ShareWrite | wire.ShareDelete,
			CreateDisposition:  wire.FileCreate,
			Name:               name,
		}
		body, err := req.Append(nil)
		if err != nil {
			t.Fatalf("编码 CREATE: %v", err)
		}
		return body
	}
	exists := func(name string) bool {
		_, err := os.Lstat(filepath.Join(h.Root, name))
		return !errors.Is(err, os.ErrNotExist)
	}

	// —— 负向：签名被篡改的 CREATE 必须既被拒、又不留痕 ——
	const tampered = "tampered-must-not-exist.txt"
	resp, _ := c.sendTampered(t, wire.CommandCreate, newCreateBody(t, tampered))
	if resp != nil {
		hdr, err := wire.ParseHeader(resp)
		if err != nil {
			t.Fatalf("解析响应头: %v", err)
		}
		if got := status.Status(hdr.Status); got != status.AccessDenied {
			t.Fatalf("篡改签名的 CREATE 得到 status=%#x (%s)，期望 STATUS_ACCESS_DENIED", hdr.Status, got)
		}
	}
	if exists(tampered) {
		t.Fatalf("签名校验失守：篡改签名的 CREATE 竟然在磁盘上建出了 %q", tampered)
	}

	// —— 反向对照：同一条 CREATE 走合法签名，必须成功且落盘 ——
	// 若这一步失败，说明上面的「文件不存在」根本不能归因于签名校验，
	// 整个探针失效——所以这里用 Fatalf 而不是跳过。
	const control = "control-must-exist.txt"
	if _, err := c.create(control, wire.FileCreate, 0); err != nil {
		t.Fatalf("反向对照失败：合法签名的 CREATE 也没成功（%v）——"+
			"上面的负向断言因此不可信", err)
	}
	if !exists(control) {
		t.Fatalf("反向对照失败：合法签名的 CREATE 返回成功但磁盘上没有 %q", control)
	}
}
