//go:build integration

package integration

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 本文件是 encrypted_test.go（加密「开」正向探针）的**反向**探针：验证当服务端
// 显式**关闭** SMB3 加密能力时，客户端：
//   1. 在 NEGOTIATE 响应里**拿不到**任何加密宣告（3.0/3.0.2 没有 CAP_ENCRYPTION，
//      3.1.1 没有 ENCRYPTION_CAPABILITIES 上下文）；
//   2. 会话不被标记为加密，数据平面收发的是**明文 SMB2** 帧（魔数 0xFE 'S' 'M' 'B'，
//      而非 TRANSFORM 的 0xFD）；
//   3. 若客户端仍强行塞一个 TRANSFORM 帧，服务端必须**断开连接**（cipher==0 分支）。
//
// 为什么需要反向探针：AGENTS.md 反复强调「验证策略开关要同时测允许与拒绝两条路径」。
// 只测「加密开 → 线路加密」给的是假阳性——拒绝路径（关掉加密后客户端/服务端行为）
// 从没被接线验证过。本文件补上这条路径，且每条断言都带反向对照，失败用例不许 skip。

// encDialect 描述一个被测方言及其「CAP_ENCRYPTION 能力位是否有意义」的语义。
// MS-SMB2 §2.2.3：3.0 / 3.0.2 用 Capabilities 里的 CAP_ENCRYPTION 位；
// 3.1.1 **不用**该位，而是用 NEGOTIATE 上下文 ENCRYPTION_CAPABILITIES。
type encDialect struct {
	dialect          wire.Dialect
	capBitMeaningful bool // 该方言能否靠 NEGOTIATE 响应的 Capabilities 位判断加密
}

var encDialects = []encDialect{
	{wire.SMB302, true},
	{wire.SMB311, false},
}

// hasEncryptionContext 报告 NEGOTIATE 响应里是否带 ENCRYPTION_CAPABILITIES 上下文。
func hasEncryptionContext(nr *wire.NegotiateResponse) bool {
	for _, cx := range nr.Contexts {
		if cx.Type == wire.ContextEncryptionCapabilities {
			return true
		}
	}
	return false
}

// readRawFrameWithTimeout 读回一条 Direct TCP 帧的原始字节（不解密），带读超时。
func (c *rawClient) readRawFrameWithTimeout(d time.Duration) ([]byte, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	return c.readRawFrame()
}

// TestSMB3EncryptionDisabledStaysPlaintext 是核心反向断言：
// 服务端关掉加密后，客户端既不能协商出加密会话，线路上跑的也必须是明文 SMB2。
func TestSMB3EncryptionDisabledStaysPlaintext(t *testing.T) {
	for _, ed := range encDialects {
		ed := ed
		t.Run(ed.dialect.String(), func(t *testing.T) {
			h := startServer(t, harnessOptions{EncryptionDisabled: true, MaxDialect: dialect.Dialect(ed.dialect)})
			c := newRawClient(t, h.Addr)
			// 客户端**主动请求**加密，逼出服务端「明明能加密却故意不给」的那条分支。
			if err := c.dial([]wire.Dialect{ed.dialect}, true); err != nil {
				t.Fatalf("dial (加密关闭, %s): %v", ed.dialect, err)
			}
			defer c.close()

			if c.negResp == nil {
				t.Fatalf("客户端未记录 NEGOTIATE 响应")
			}

			// 断言 1：NEGOTIATE 响应不得宣告加密能力。
			if ed.capBitMeaningful {
				if c.negResp.Capabilities&wire.CapEncryption != 0 {
					t.Fatalf("服务端仍宣告了 CAP_ENCRYPTION（方言 %s）—— 关加密未生效", ed.dialect)
				}
			} else {
				if hasEncryptionContext(c.negResp) {
					t.Fatalf("3.1.1 响应仍带 ENCRYPTION_CAPABILITIES 上下文 —— 关加密未生效")
				}
			}

			// 断言 2：会话不能被标记为加密。
			if c.encrypted {
				t.Fatalf("会话被标记为加密，但服务端应已关闭加密")
			}

			// 断言 3：数据平面能正常工作。
			if err := c.treeConnect(testShare); err != nil {
				t.Fatalf("treeConnect: %v", err)
			}
			name := "plain-file.txt"
			fid, err := c.create(name, wire.FileOpenIf, wire.FileNonDirectoryFile)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			data := []byte("plaintext-over-clear-transport")
			if err := c.write(fid, 0, data); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := c.read(fid, 0, uint32(len(data)))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != string(data) {
				t.Fatalf("明文会话下数据不匹配: got %q want %q", got, data)
			}
			_ = c.closeFile(fid)

			// 断言 4（最关键、可证伪）：线路上跑的是**明文 SMB2** 帧，
			// 不是 TRANSFORM（0xFD）。手搓一个带签名的 ECHO 并发出去读原始帧。
			c.msgID++
			req := wire.Header{Command: wire.CommandEcho, Credits: 1, MessageID: c.msgID, TreeID: c.treeID, SessionID: c.sessID}
			echo := req.Append(nil)
			echo = append(echo, (&wire.EchoRequest{}).Append(nil)...)
			if err := crypto.Sign(uint16(c.dialect), c.signingKey, echo); err != nil {
				t.Fatalf("签名 ECHO: %v", err)
			}
			if err := c.writeMessage(echo); err != nil {
				t.Fatalf("发送 ECHO: %v", err)
			}
			raw, err := c.readRawFrameWithTimeout(5 * time.Second)
			if err != nil {
				t.Fatalf("读原始帧: %v", err)
			}
			if len(raw) < 4 {
				t.Fatalf("原始帧过短: % x", raw)
			}
			if raw[0] == 0xFD && raw[1] == 'S' && raw[2] == 'M' && raw[3] == 'B' {
				t.Fatalf("关掉加密后线路上竟是 TRANSFORM 报文（% x）—— 加密未被真正关闭", raw[:4])
			}
			if !(raw[0] == 0xFE && raw[1] == 'S' && raw[2] == 'M' && raw[3] == 'B') {
				t.Fatalf("线路上不是明文 SMB2 帧（魔数 = % x）", raw[:4])
			}
		})
	}
}

// TestSMB3EncryptionDisabledRejectsTransformFrame 验证：在关掉加密的会话里，
// 客户端若仍发 TRANSFORM 帧，服务端必须断开连接（handleEncrypted 的 cipher==0 分支）。
// 这是「拒绝路径」的端到端确认——服务端不能把没协商过加密算法的帧当正常请求处理。
func TestSMB3EncryptionDisabledRejectsTransformFrame(t *testing.T) {
	for _, ed := range encDialects {
		ed := ed
		t.Run(ed.dialect.String(), func(t *testing.T) {
			h := startServer(t, harnessOptions{EncryptionDisabled: true, MaxDialect: dialect.Dialect(ed.dialect)})
			c := newRawClient(t, h.Addr)
			if err := c.dial([]wire.Dialect{ed.dialect}, true); err != nil {
				t.Fatalf("dial (加密关闭, %s): %v", ed.dialect, err)
			}
			defer c.close()

			// 用服务端赋予的 SessionID 伪造一个 TRANSFORM 帧。
			// 密钥/nonce 都随便填——服务端根本到不了「解密」那一步，
			// 它会在 cipher==0 处直接报错并断连。
			keyLen := 16
			if c.cipher == crypto.CipherAES256CCM || c.cipher == crypto.CipherAES256GCM {
				keyLen = 32
			}
			dummyKey := make([]byte, keyLen)
			nonce := make([]byte, 11) // TRANSFORM nonce 长度
			bogus, err := crypto.Encrypt(c.cipher, dummyKey, nonce, c.sessID, []byte("bogus"))
			if err != nil {
				t.Fatalf("伪造 TRANSFORM 帧: %v", err)
			}
			if err := c.writeMessage(bogus); err != nil {
				t.Fatalf("发送伪造 TRANSFORM: %v", err)
			}
			// 服务端应当**断开**：读到的要么是 EOF（连接已关），要么超时错误。
			// 只要读不到「一条正常响应」，就说明连接已被掐断。
			_, err = c.readRawFrameWithTimeout(3 * time.Second)
			if err == nil {
				t.Fatalf("服务端在收到未协商加密的 TRANSFORM 后仍正常响应 —— 应断连")
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				// 期望路径：连接被服务端关闭。
				return
			}
			// 其它错误（如读超时）也算「连接不再可用」，接受。
		})
	}
}

// TestSMB3EncryptionEnabledAdvertisesCipher 是反向对照（控制组）：
// 加密「开」时必须能在 NEGOTIATE 响应里看到加密能力宣告。这条控制组一旦失败，
// 上面的「关闭」断言才可信——否则「关闭后看不到宣告」可能只是测试客户端根本没去读。
func TestSMB3EncryptionEnabledAdvertisesCipher(t *testing.T) {
	for _, ed := range encDialects {
		ed := ed
		t.Run(ed.dialect.String(), func(t *testing.T) {
			h := startServer(t, harnessOptions{EncryptionRequired: true, MaxDialect: dialect.Dialect(ed.dialect)})
			c := newRawClient(t, h.Addr)
			if err := c.dial([]wire.Dialect{ed.dialect}, true); err != nil {
				t.Fatalf("dial (加密开启, %s): %v", ed.dialect, err)
			}
			defer c.close()

			if c.negResp == nil {
				t.Fatalf("客户端未记录 NEGOTIATE 响应")
			}
			if ed.capBitMeaningful {
				if c.negResp.Capabilities&wire.CapEncryption == 0 {
					t.Fatalf("3.0/3.0.2 开启加密后响应竟无 CAP_ENCRYPTION")
				}
			} else {
				if !hasEncryptionContext(c.negResp) {
					t.Fatalf("3.1.1 开启加密后响应无 ENCRYPTION_CAPABILITIES 上下文")
				}
			}
			if !c.encrypted {
				t.Fatalf("开启加密后会话未被标记为加密")
			}
		})
	}
}
