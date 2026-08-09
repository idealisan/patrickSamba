//go:build integration

// Package integration 包含需要真实运行 stupidsamba 服务器的端到端测试。
//
// 这些测试用纯 Go 写的「裸 SMB2 客户端」（rawClient）直连 harness 起的
// 进程内服务器，专门覆盖第三方客户端（smbclient / go-smb2 / impacket）难以
// 构造、且正被并行改动的边界路径：
//
//   - 复合请求链（related / unrelated，含事后抑制回滚与 credit 豁免）
//   - 签名强制（篡改签名被拒，连接被断）
//   - CANCEL 不产生响应（连接仍可复用、credit 不被破坏）
//   - ADS 读写（file:stream:$DATA）
//   - SMB3 加密会话下的完整文件操作
//   - 多方言协商（2.0.2 / 2.1 / 3.0 / 3.0.2 / 3.1.1 各自跑通基本文件操作）
//
// rawClient 复用项目自身的 wire / auth / crypto 包：报文编解码走 wire，
// NTLMv2 服务端校验原语（NTHash / NTOWFv2 / ComputeNTProofStr / SessionBaseKey
// 等）也被客户端侧复用，SPNEGO 外壳则刻意省略——直接发裸 NTLMSSP，服务端
// 的 ParseSPNEGO 接受裸 NTLMSSP 且裸模式下跳过 mechListMIC 校验。
package integration

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// rawClient 是一个最小但正确的 SMB2/3 客户端，仅用于测试。
type rawClient struct {
	t       *testing.T
	conn    net.Conn
	addr    string
	host    string
	dialect wire.Dialect

	msgID   uint64
	sessID  uint64
	treeID  uint32
	user    string
	pass    string
	domain  string

	sessionKey []byte // 16 字节 ExportedSessionKey（无 KEY_EXCH 时即 SessionBaseKey）
	signingKey []byte
	appKey     []byte

	encrypted  bool
	cipher     crypto.Cipher
	encryptKey []byte // 服务端→客户端（解密响应），ServerOutKey
	decryptKey []byte // 客户端→服务端（加密请求），ServerInKey
	encNonce   *crypto.NonceCounter
	decNonce   *crypto.NonceCounter

	preauth  []byte // 3.1.1 滚动 preauth 哈希（连接级 + 会话级合一，按规范顺序累积）
	authDone bool
}

// newRawClient 创建未连接的裸客户端。
func newRawClient(t *testing.T, addr string) *rawClient {
	return &rawClient{t: t, addr: addr, host: "server", user: testUser, pass: testPass, domain: testDomain}
}

// dial 完成 NEGOTIATE + NTLMv2 SESSION_SETUP。
func (c *rawClient) dial(dialects []wire.Dialect, encrypt bool) error {
	conn, err := net.Dial("tcp", c.addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.addr, err)
	}
	c.conn = conn
	if err := c.negotiate(dialects, encrypt); err != nil {
		return err
	}
	return c.sessionSetup()
}

// close 关闭底层连接。
func (c *rawClient) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// ---------------------------------------------------------------- NEGOTIATE

func (c *rawClient) negotiate(dialects []wire.Dialect, encrypt bool) error {
	nr := &wire.NegotiateRequest{
		SecurityMode: wire.NegotiateSigningEnabled,
		ClientGUID:   newGUID(),
		Dialects:     dialects,
	}
	if encrypt {
		nr.Capabilities |= wire.CapEncryption
	}
	has311 := false
	for _, d := range dialects {
		if d == wire.SMB311 {
			has311 = true
		}
	}
	if has311 {
		salt := make([]byte, 32)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		pa, err := (&wire.PreauthIntegrityCapabilities{
			HashAlgorithms: []uint16{wire.HashAlgorithmSHA512},
			Salt:           salt,
		}).Encode()
		if err != nil {
			return err
		}
		ctxs := []wire.NegotiateContext{{Type: wire.ContextPreauthIntegrityCapabilities, Data: pa}}
		if encrypt {
			enc, err := (&wire.EncryptionCapabilities{Ciphers: []uint16{uint16(wire.CipherAES128GCM)}}).Encode()
			if err != nil {
				return err
			}
			ctxs = append(ctxs, wire.NegotiateContext{Type: wire.ContextEncryptionCapabilities, Data: enc})
		}
		nr.Contexts = ctxs
	}
	body, err := nr.Append(nil)
	if err != nil {
		return err
	}
	h := wire.Header{Command: wire.CommandNegotiate, Credits: 1, MessageID: 0}
	msg := h.Append(nil)
	msg = append(msg, body...)
	if has311 {
		c.updatePreauth(msg)
	}
	if err := c.writeMessage(msg); err != nil {
		return err
	}
	resp, err := c.readMessage()
	if err != nil {
		return err
	}
	if has311 {
		c.updatePreauth(resp)
	}
	nr2, err := wire.ParseNegotiateResponse(resp)
	if err != nil {
		return err
	}
	c.dialect = nr2.DialectRevision
	if c.dialect >= wire.SMB300 {
		c.cipher = crypto.CipherAES128CCM // 3.0 / 3.0.2 固定 AES-128-CCM
		if c.dialect == wire.SMB311 {
			for _, cx := range nr2.Contexts {
				if cx.Type == wire.ContextEncryptionCapabilities && len(cx.Data) >= 2 {
					c.cipher = crypto.Cipher(binary.LittleEndian.Uint16(cx.Data))
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- SESSION_SETUP (NTLMv2)

func (c *rawClient) sessionSetup() error {
	// —— 轮次 1：NTLMSSP NEGOTIATE（Type1） ——
	neg := (&auth.NegotiateMessage{Flags: c.ntlmFlags()}).Marshal()
	req := &wire.SessionSetupRequest{SecurityBuffer: neg, SecurityMode: wire.NegotiateSigningEnabled}
	body, err := req.Append(nil)
	if err != nil {
		return err
	}
	h := wire.Header{Command: wire.CommandSessionSetup, Credits: 1, MessageID: c.nextMsgID()}
	reqMsg := h.Append(nil)
	reqMsg = append(reqMsg, body...)
	resp1, err := c.sendRaw(reqMsg, true)
	if err != nil {
		return err
	}
	c.updatePreauth(resp1) // 中间轮次响应也进 preauth（协议注释 §6）

	hdr1, err := wire.ParseHeader(resp1)
	if err != nil {
		return err
	}
	if hdr1.Status != 0 { // 期望 STATUS_MORE_PROCESSING_REQUIRED
		return fmt.Errorf("session setup round1 status = %#x", hdr1.Status)
	}
	ssr1, err := wire.ParseSessionSetupResponse(resp1)
	if err != nil {
		return err
	}
	ch, err := auth.ParseChallengeMessage(ssr1.SecurityBuffer)
	if err != nil {
		return fmt.Errorf("parse CHALLENGE: %w", err)
	}
	c.sessID = hdr1.SessionID

	// —— 轮次 2：NTLMSSP AUTHENTICATE（Type3） ——
	authMsg := c.buildAuthenticate(ch)
	req2 := &wire.SessionSetupRequest{SecurityBuffer: authMsg, SecurityMode: wire.NegotiateSigningEnabled}
	body2, err := req2.Append(nil)
	if err != nil {
		return err
	}
	h2 := wire.Header{Command: wire.CommandSessionSetup, Credits: 1, MessageID: c.nextMsgID(), SessionID: c.sessID}
	reqMsg2 := h2.Append(nil)
	reqMsg2 = append(reqMsg2, body2...)
	resp2, err := c.sendRaw(reqMsg2, true)
	if err != nil {
		return err
	}
	// 最终成功响应**不**进 preauth 哈希（密钥派生用当前哈希）。

	hdr2, err := wire.ParseHeader(resp2)
	if err != nil {
		return err
	}
	if hdr2.Status != 0 {
		return fmt.Errorf("session setup round2 status = %#x", hdr2.Status)
	}
	ssr2, err := wire.ParseSessionSetupResponse(resp2)
	if err != nil {
		return err
	}
	encrypt := ssr2.SessionFlags&wire.SessionFlagEncryptData != 0
	c.deriveKeys(encrypt)
	// 最终响应由服务器用派生出的 SigningKey 签名，客户端校验。
	if err := crypto.Verify(uint16(c.dialect), c.signingKey, resp2); err != nil {
		return fmt.Errorf("session setup 最终响应签名校验失败: %w", err)
	}
	if encrypt {
		c.encrypted = true
	}
	c.authDone = true
	return nil
}

// buildAuthenticate 构造 NTLMv2 AUTHENTICATE 报文并就地记录会话密钥。
func (c *rawClient) buildAuthenticate(ch *auth.ChallengeMessage) []byte {
	ntHash := auth.NTHash(c.pass)
	ntowf := auth.NTOWFv2(ntHash, c.user, c.domain)

	var blob bytes.Buffer
	blob.WriteByte(0x01)              // RespType
	blob.WriteByte(0x01)              // HiRespType
	blob.Write([]byte{0, 0})          // Reserved (2)
	blob.Write([]byte{0, 0, 0, 0})    // Reserved (4)
	blob.Write(filetimeNow())        // Timestamp (8)
	cc := make([]byte, 8)             // ClientChallenge (8)
	_, _ = rand.Read(cc)
	blob.Write(cc)
	blob.Write([]byte{0, 0, 0, 0}) // Reserved (4)
	blob.Write(ch.TargetInfo.Encode())
	b := blob.Bytes()

	proof := auth.ComputeNTProofStr(ntowf, ch.ServerChallenge, b)
	var ntResp []byte
	ntResp = append(ntResp, proof[:]...)
	ntResp = append(ntResp, b...)

	sbk := auth.SessionBaseKey(ntowf, proof)
	// 未协商 NEGOTIATE_KEY_EXCH：ExportedSessionKey == KeyExchangeKey == SessionBaseKey。
	c.sessionKey = sbk[:]

	msg := &auth.AuthenticateMessage{
		LMResponse:                cc, // NTLMv2 的 LM 响应即 8 字节 client challenge
		NTResponse:                ntResp,
		DomainName:                c.domain,
		UserName:                  c.user,
		Workstation:               "QA",
		EncryptedRandomSessionKey: nil,
		Flags:                     c.ntlmFlags() | auth.NegotiateTargetTypeServer,
		Version:                   auth.Version{Major: 6, Minor: 1, Build: 7601, NTLMRevision: auth.NTLMRevisionCurrent},
	}
	return msg.Marshal()
}

// ntlmFlags 是客户端 NTLM 协商标志（Unicode / NTLMv2 / 始终签名 / 目标信息）。
func (c *rawClient) ntlmFlags() auth.NegotiateFlags {
	return auth.NegotiateUnicode |
		auth.NegotiateRequestTarget |
		auth.NegotiateNTLM |
		auth.NegotiateExtendedSessionSecurity |
		auth.NegotiateTargetInfo |
		auth.NegotiateAlwaysSign |
		auth.Negotiate128 |
		auth.NegotiateVersion |
		auth.NegotiateSign
}

// deriveKeys 根据方言与（可能的）加密标志派生签名 / 应用 / 加解密密钥。
func (c *rawClient) deriveKeys(encrypt bool) {
	d := uint16(c.dialect)
	if c.dialect >= wire.SMB300 {
		keyLen := 16
		if c.cipher == crypto.CipherAES256CCM || c.cipher == crypto.CipherAES256GCM {
			keyLen = 32
		}
		c.signingKey = crypto.SigningKey(d, c.sessionKey, c.preauth)
		c.appKey = crypto.ApplicationKey(d, c.sessionKey, c.preauth)
		if encrypt {
			c.encryptKey = crypto.ServerOutKey(d, c.sessionKey, c.preauth, keyLen)
			c.decryptKey = crypto.ServerInKey(d, c.sessionKey, c.preauth, keyLen)
			c.encNonce = &crypto.NonceCounter{}
			c.decNonce = &crypto.NonceCounter{}
		}
	} else {
		// 2.x：SigningKey / ApplicationKey 直接使用 SessionKey。
		c.signingKey = append([]byte(nil), c.sessionKey...)
		c.appKey = append([]byte(nil), c.sessionKey...)
	}
}

// ---------------------------------------------------------------- 通用收发

// request 发送一条已认证会话下的请求并读回单条响应（自动签名 / 加密 / 验签）。
func (c *rawClient) request(cmd wire.Command, body []byte) ([]byte, error) {
	if !c.authDone {
		return nil, fmt.Errorf("request: 会话尚未建立")
	}
	c.msgID++
	h := wire.Header{
		Command:    cmd,
		Credits:    1,
		MessageID:  c.msgID,
		TreeID:     c.treeID,
		SessionID:  c.sessID,
	}
	msg := h.Append(nil)
	msg = append(msg, body...)

	if !c.encrypted && c.signingKey != nil {
		if err := crypto.Sign(uint16(c.dialect), c.signingKey, msg); err != nil {
			return nil, err
		}
	}
	if c.encrypted && c.decryptKey != nil {
		nonce := c.decNonce.Next()
		enc, err := crypto.Encrypt(c.cipher, c.decryptKey, nonce[:], c.sessID, msg)
		if err != nil {
			return nil, err
		}
		msg = enc
	}
	if err := c.writeMessage(msg); err != nil {
		return nil, err
	}
	resp, err := c.readMessage()
	if err != nil {
		return nil, err
	}
	if !c.encrypted && c.signingKey != nil {
		if err := crypto.Verify(uint16(c.dialect), c.signingKey, resp); err != nil {
			return nil, fmt.Errorf("响应签名校验失败: %w", err)
		}
	}
	return resp, nil
}

// sendRaw 发送一条裸消息（用于协商 / 认证 / CANCEL 等无密钥场景）。
// hashReq 表示这条请求是否进 3.1.1 preauth 哈希（调用方负责给响应哈希）。
func (c *rawClient) sendRaw(msg []byte, hashReq bool) ([]byte, error) {
	if hashReq && c.dialect == wire.SMB311 {
		c.updatePreauth(msg)
	}
	if err := c.writeMessage(msg); err != nil {
		return nil, err
	}
	return c.readMessage()
}

// writeMessage 按 Direct TCP 帧（0x00 + 3 字节大端长度）写出一条消息。
func (c *rawClient) writeMessage(msg []byte) error {
	if c.conn == nil {
		return fmt.Errorf("writeMessage: 连接未建立")
	}
	buf := make([]byte, 4+len(msg))
	buf[0] = 0x00
	binary.BigEndian.PutUint32(buf, uint32(len(msg))) // 高字节恒为 0（len < 16MiB）
	copy(buf[4:], msg)
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(buf); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// readMessage 读回一条消息（自动解密 SMB3 TRANSFORM）。
func (c *rawClient) readMessage() ([]byte, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("readMessage: 连接未建立")
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return nil, fmt.Errorf("read hdr: %w", err)
	}
	n := int(binary.BigEndian.Uint32(hdr))
	if n <= 0 || n > 16*1024*1024 {
		return nil, fmt.Errorf("read: 非法长度 %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if crypto.IsTransform(buf) {
		plain, err := crypto.Decrypt(c.cipher, c.encryptKey, buf)
		if err != nil {
			return nil, fmt.Errorf("decrypt: %w", err)
		}
		return plain, nil
	}
	return buf, nil
}

// readMessageTimeout 带自定义超时的读（用于 CANCEL 不产生响应断言）。
func (c *rawClient) readMessageTimeout(d time.Duration) ([]byte, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("readMessageTimeout: 连接未建立")
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr))
	if n <= 0 || n > 16*1024*1024 {
		return nil, fmt.Errorf("read: 非法长度 %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// updatePreauth 把一条消息滚进 3.1.1 preauth 哈希（SHA-512 链接）。
func (c *rawClient) updatePreauth(msg []byte) {
	h := sha512.New()
	if len(c.preauth) > 0 {
		h.Write(c.preauth)
	}
	h.Write(msg)
	c.preauth = h.Sum(nil)
}

func (c *rawClient) nextMsgID() uint64 {
	c.msgID++
	return c.msgID
}

// ---------------------------------------------------------------- 高层文件操作

// treeConnect 连接共享，成功后写入 c.treeID。
func (c *rawClient) treeConnect(share string) error {
	req := &wire.TreeConnectRequest{Path: `\\` + c.host + `\` + share}
	body, err := req.Append(nil)
	if err != nil {
		return err
	}
	resp, err := c.request(wire.CommandTreeConnect, body)
	if err != nil {
		return err
	}
	hdr, err := wire.ParseHeader(resp)
	if err != nil {
		return err
	}
	if hdr.Status != 0 {
		return fmt.Errorf("TREE_CONNECT %s 失败: status=%#x", share, hdr.Status)
	}
	c.treeID = hdr.TreeID
	return nil
}

// create 打开/创建文件，返回 FileID。name 为共享内相对路径（ADS 用 "f:s:$DATA"）。
func (c *rawClient) create(name string, disp wire.CreateDisposition, opts wire.CreateOptions) (wire.FileID, error) {
	req := &wire.CreateRequest{
		ImpersonationLevel: wire.ImpersonationImpersonation,
		DesiredAccess:      wire.GenericRead | wire.GenericWrite,
		FileAttributes:     wire.FileAttributeNormal,
		ShareAccess:        wire.ShareRead | wire.ShareWrite | wire.ShareDelete,
		CreateDisposition:  disp,
		CreateOptions:      opts,
		Name:               name,
	}
	body, err := req.Append(nil)
	if err != nil {
		return wire.FileID{}, err
	}
	resp, err := c.request(wire.CommandCreate, body)
	if err != nil {
		return wire.FileID{}, err
	}
	hdr, err := wire.ParseHeader(resp)
	if err != nil {
		return wire.FileID{}, err
	}
	if hdr.Status != 0 {
		return wire.FileID{}, fmt.Errorf("CREATE %q 失败: status=%#x", name, hdr.Status)
	}
	cr, err := wire.ParseCreateResponse(resp)
	if err != nil {
		return wire.FileID{}, err
	}
	return cr.FileID, nil
}

// read 从文件读 len 字节（offset 起）。
func (c *rawClient) read(fid wire.FileID, off uint64, length uint32) ([]byte, error) {
	req := &wire.ReadRequest{
		Length:       length,
		Offset:       off,
		FileID:       fid,
		MinimumCount: 0,
	}
	body, err := req.Append(nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.request(wire.CommandRead, body)
	if err != nil {
		return nil, err
	}
	hdr, err := wire.ParseHeader(resp)
	if err != nil {
		return nil, err
	}
	if hdr.Status != 0 {
		return nil, fmt.Errorf("READ 失败: status=%#x", hdr.Status)
	}
	rr, err := wire.ParseReadResponse(resp)
	if err != nil {
		return nil, err
	}
	return rr.Data, nil
}

// write 向文件写 data（offset 起）。
func (c *rawClient) write(fid wire.FileID, off uint64, data []byte) error {
	req := &wire.WriteRequest{
		Offset: off,
		FileID: fid,
		Data:   data,
	}
	body, err := req.Append(nil)
	if err != nil {
		return err
	}
	resp, err := c.request(wire.CommandWrite, body)
	if err != nil {
		return err
	}
	hdr, err := wire.ParseHeader(resp)
	if err != nil {
		return err
	}
	if hdr.Status != 0 {
		return fmt.Errorf("WRITE 失败: status=%#x", hdr.Status)
	}
	wr, err := wire.ParseWriteResponse(resp)
	if err != nil {
		return err
	}
	if wr.Count != uint32(len(data)) {
		return fmt.Errorf("WRITE 只写了 %d/%d 字节", wr.Count, len(data))
	}
	return nil
}

// close 关闭句柄。
func (c *rawClient) closeFile(fid wire.FileID) error {
	req := &wire.CloseRequest{FileID: fid}
	body := req.Append(nil)
	resp, err := c.request(wire.CommandClose, body)
	if err != nil {
		return err
	}
	hdr, err := wire.ParseHeader(resp)
	if err != nil {
		return err
	}
	if hdr.Status != 0 {
		return fmt.Errorf("CLOSE 失败: status=%#x", hdr.Status)
	}
	return nil
}

// echo 发一条 ECHO（用于验证连接可用性，例如 CANCEL 之后）。
func (c *rawClient) echo() error {
	req := &wire.EchoRequest{}
	body := req.Append(nil)
	resp, err := c.request(wire.CommandEcho, body)
	if err != nil {
		return err
	}
	hdr, err := wire.ParseHeader(resp)
	if err != nil {
		return err
	}
	if hdr.Status != 0 {
		return fmt.Errorf("ECHO 失败: status=%#x", hdr.Status)
	}
	return nil
}

// sendCancel 发送一条 CANCEL（不签名、不产生响应）。
func (c *rawClient) sendCancel(msgID uint64) error {
	req := &wire.CancelRequest{}
	body := req.Append(nil)
	h := wire.Header{Command: wire.CommandCancel, Credits: 1, MessageID: msgID, SessionID: c.sessID}
	msg := h.Append(nil)
	msg = append(msg, body...)
	// CANCEL 既不被签名也不被加密，且服务端不产生响应。
	return c.writeMessage(msg)
}

// ---------------------------------------------------------------- 编码辅助

// utf16le 把字符串编码为 UTF-16LE 字节串。
func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(out[i*2:], v)
	}
	return out
}

// filetimeNow 返回当前时间的 FILETIME（1601-01-01 起的 100ns 计数）。
func filetimeNow() []byte {
	const epochDelta = uint64(116444736000000000)
	v := uint64(time.Now().UTC().UnixNano()/100) + epochDelta
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

// newGUID 生成 16 字节随机 GUID。
func newGUID() [16]byte {
	var g [16]byte
	_, _ = rand.Read(g[:])
	return g
}
