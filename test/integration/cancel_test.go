//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// TestCancelNoResponse 验证 SMB2 CANCEL（命令 0xC）的特殊语义：
//   - CANCEL 本身不产生任何响应（MS-SMB2 §3.3.5.16）；
//   - 发送 CANCEL 后连接仍可用（credit 不被破坏、不被误关），后续 ECHO 正常。
func TestCancelNoResponse(t *testing.T) {
	h := startServer(t, harnessOptions{})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.close()
	if err := c.treeConnect(testShare); err != nil {
		t.Fatalf("treeConnect: %v", err)
	}
	// 建一个句柄，确保会话/树处于活跃态。
	fid, err := c.create("cancel-base.txt", wire.FileOpenIf, wire.FileNonDirectoryFile)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer c.closeFile(fid)

	// 发送 CANCEL（引用一个不存在的 MessageId 也无妨，服务端只做静默丢弃）。
	if err := c.sendCancel(c.msgID + 100); err != nil {
		t.Fatalf("sendCancel: %v", err)
	}

	// 期望：在短超时内读不到任何响应。
	if _, err := c.readMessageTimeout(2 * time.Second); err == nil {
		t.Fatalf("CANCEL 竟产生了响应（违反 §3.3.5.16）")
	}

	// 连接仍健康：后续 ECHO 应正常返回。
	if err := c.echo(); err != nil {
		t.Fatalf("CANCEL 后连接不可用（credit/连接被破坏？）: %v", err)
	}
}
