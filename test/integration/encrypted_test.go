//go:build integration

package integration

import (
	"encoding/binary"
	"io"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/crypto"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// readRawFrame 读回一条 Direct TCP 帧的**原始**字节（不做解密），用于断言
// SMB3 加密会话下线路上跑的是 TRANSFORM 报文（魔数 0xFD 'S' 'M' 'B'）。
func (c *rawClient) readRawFrame() ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr))
	if n <= 0 || n > 16*1024*1024 {
		return nil, io.ErrShortBuffer
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// TestSMB3EncryptedSession 验证在「强制加密」会话下，SMB 3.1.1 能完成
// 完整文件操作，且线路上实际传输的是加密 TRANSFORM 报文（而非明文 SMB2）。
func TestSMB3EncryptedSession(t *testing.T) {
	h := startServer(t, harnessOptions{EncryptionRequired: true})
	c := newRawClient(t, h.Addr)
	if err := c.dial([]wire.Dialect{wire.SMB311}, true); err != nil {
		t.Fatalf("dial (加密 3.1.1): %v", err)
	}
	defer c.close()

	if !c.encrypted {
		t.Fatalf("会话未被标记为加密（服务端未回 EncryptData？）")
	}

	if err := c.treeConnect(testShare); err != nil {
		t.Fatalf("treeConnect: %v", err)
	}
	name := "enc-file.txt"
	fid, err := c.create(name, wire.FileOpenIf, wire.FileNonDirectoryFile)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	data := []byte("secret-over-encrypted-transport")
	if err := c.write(fid, 0, data); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := c.read(fid, 0, uint32(len(data)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("加密会话下数据不匹配: got %q want %q", got, data)
	}
	if err := c.closeFile(fid); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 最后再发一条 ECHO，读取**原始**帧，断言线路上是 TRANSFORM（加密）报文。
	c.msgID++
	req := wire.Header{Command: wire.CommandEcho, Credits: 1, MessageID: c.msgID, SessionID: c.sessID}
	echo := req.Append(nil)
	echo = append(echo, (&wire.EchoRequest{}).Append(nil)...)
	nonce := c.decNonce.Next()
	enc, err := crypto.Encrypt(c.cipher, c.decryptKey, nonce[:], c.sessID, echo)
	if err != nil {
		t.Fatalf("加密 ECHO: %v", err)
	}
	if err := c.writeMessage(enc); err != nil {
		t.Fatalf("发送加密 ECHO: %v", err)
	}
	raw, err := c.readRawFrame()
	if err != nil {
		t.Fatalf("读原始帧: %v", err)
	}
	m := 4
	if len(raw) < m {
		m = len(raw)
	}
	if len(raw) < 4 || raw[0] != 0xFD || raw[1] != 'S' || raw[2] != 'M' || raw[3] != 'B' {
		t.Fatalf("线路上不是 TRANSFORM 报文（魔数 = % x），加密未生效", raw[:m])
	}
}
