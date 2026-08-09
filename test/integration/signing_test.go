//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// TestSigningEnforced 验证在「强制签名」会话下：
//  1. 正常签名的请求得到正常响应（证明裸客户端签名被服务端接受）；
//  2. 篡改签名的请求被服务端拒绝且连接被断（MS-SMB2 §3.3.5.2.3：
//     无效签名必须终止连接）。
func TestSigningEnforced(t *testing.T) {
	h := startServer(t, harnessOptions{SigningRequired: true})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()

	// (1) 正常签名的 ECHO 应成功。
	if err := c.echo(); err != nil {
		t.Fatalf("合法签名 ECHO 失败: %v", err)
	}

	// (2) 构造一条签名后被篡改的请求。
	c.msgID++
	h2 := wire.Header{
		Command:   wire.CommandEcho,
		Credits:   1,
		MessageID: c.msgID,
		TreeID:    c.treeID,
		SessionID: c.sessID,
	}
	msg := h2.Append(nil)
	msg = append(msg, (&wire.EchoRequest{}).Append(nil)...)
	if err := crypto.Sign(uint16(c.dialect), c.signingKey, msg); err != nil {
		t.Fatalf("签名: %v", err)
	}
	// 篡改 Signature 字段（偏移 0x30 起 16 字节）的一个字节。
	msg[0x30] ^= 0xFF
	if err := c.writeMessage(msg); err != nil {
		t.Fatalf("发送篡改请求: %v", err)
	}

	// 服务端应终止连接：读取应超时/报错，而非回一个响应。
	if _, err := c.readMessageTimeout(3 * time.Second); err == nil {
		t.Fatalf("篡改签名的请求竟得到了响应（签名校验未生效）")
	}
	// 连接已死：后续合法请求也应失败。
	if err := c.echo(); err == nil {
		t.Fatalf("篡改签名后连接仍可用（应已被服务端断开）")
	}
}
