package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"

	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 复合链相关错误（都是致命的协议错误，一律断连）。
var (
	errBadCompound  = errors.New("smb: 复合请求链格式错误")
	errUnknownFrame = errors.New("smb: 无法识别的帧类型")
	errSMB1Refused  = errors.New("smb: 拒绝 SMB1 请求")
)

// transformProtocolID 是 SMB2 TRANSFORM_HEADER 的魔数 0xFD 'S' 'M' 'B'
// （MS-SMB2 §2.2.41）。加密消息以它开头。
//
// TODO(wire): 等 wire 包实现 TRANSFORM_HEADER 后改用 wire.IsTransform。
var transformProtocolID = [4]byte{0xFD, 'S', 'M', 'B'}

func isTransform(b []byte) bool {
	return len(b) >= 4 && b[0] == transformProtocolID[0] && b[1] == transformProtocolID[1] &&
		b[2] == transformProtocolID[2] && b[3] == transformProtocolID[3]
}

// Connection 是一条客户端连接：传输层 + SMB2 协议状态 + 读循环。
type Connection struct {
	srv *Server
	nc  net.Conn
	tr  *Transport
	log *slog.Logger

	// state 是 SMB2 协议状态（会话/树/句柄表）。
	state *command.Conn

	// credits 是本连接的 credit 池。
	credits *Credits

	// writeMu 串行化写出。当前只有读循环会写，但异步响应（M4+）会用到。
	writeMu sync.Mutex

	closeOnce sync.Once
}

func newConnection(s *Server, nc net.Conn) *Connection {
	remote := nc.RemoteAddr().String()
	local := nc.LocalAddr().String()

	log := s.log.With("remote", remote)
	tr := NewTransport(nc, s.opts.MaxFrameSize)
	tr.SetTimeouts(s.opts.idleTimeout(), s.opts.writeTimeout())

	return &Connection{
		srv:     s,
		nc:      nc,
		tr:      tr,
		log:     log,
		state:   command.NewConn(s.opts.Settings, remote, local),
		credits: NewCredits(DefaultMaxCredits),
	}
}

// Close 关闭连接并释放其全部会话/树/句柄。可重复调用。
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		_ = c.tr.Close()
		c.state.Close()
	})
}

// serve 是连接的主循环：读帧 → 分发 → 写帧。
//
// 任何 panic 都在这里兜住：一条连接崩了不能拖垮整个服务（AGENTS.md §7）。
func (c *Connection) serve(ctx context.Context) {
	defer c.Close()
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("连接处理 panic，已断开该连接",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()

	// ctx 取消时立刻打断阻塞中的 Read/Write。
	stop := context.AfterFunc(ctx, func() { _ = c.nc.Close() })
	defer stop()

	c.log.Debug("连接建立")

	for {
		frame, err := c.tr.ReadFrame()
		if err != nil {
			c.logReadError(err)
			return
		}

		resp, err := c.handleFrame(frame)
		if err != nil {
			c.log.Warn("处理帧失败，断开连接", "err", err)
			return
		}
		if len(resp) == 0 {
			continue
		}

		c.writeMu.Lock()
		err = c.tr.WriteFrame(resp)
		c.writeMu.Unlock()
		if err != nil {
			c.log.Debug("写出响应失败", "err", err)
			return
		}
	}
}

func (c *Connection) logReadError(err error) {
	switch {
	case errors.Is(err, io.EOF):
		c.log.Debug("对端关闭连接")
	case errors.Is(err, net.ErrClosed):
		c.log.Debug("连接已关闭")
	case errors.Is(err, io.ErrUnexpectedEOF):
		c.log.Debug("对端在传输中途断开")
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			c.log.Debug("连接空闲超时，断开")
			return
		}
		c.log.Warn("读取帧失败", "err", err)
	}
}

// handleFrame 处理一个 Direct TCP 帧，返回要写回的响应帧（可能为空）。
//
// 返回 error 表示这是致命协议错误，调用方必须断开连接。
func (c *Connection) handleFrame(frame []byte) ([]byte, error) {
	switch {
	case wire.IsSMB2(frame):
		return c.handleSMB2Chain(frame)

	case wire.IsSMB1(frame):
		return c.handleSMB1(frame)

	case isTransform(frame):
		return c.handleEncrypted(frame)

	default:
		return nil, fmt.Errorf("%w: 首 4 字节 % x", errUnknownFrame, frame[:min(4, len(frame))])
	}
}

// handleSMB1 处理 SMB1 多协议协商入口（protocol-notes §4）。
func (c *Connection) handleSMB1(frame []byte) ([]byte, error) {
	if c.state.NegotiateDone {
		// 协商完成后再收到 SMB1 是异常，直接断连。
		return nil, errSMB1Refused
	}
	if !c.srv.opts.Settings.AllowSMB1Negotiate {
		return nil, fmt.Errorf("%w: SMB1 协商入口已禁用", errSMB1Refused)
	}

	dialects, err := command.ParseSMB1NegotiateDialects(frame)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSMB1Refused, err)
	}

	out, err := command.AppendSMB1NegotiateReply(c.state, dialects, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSMB1Refused, err)
	}
	c.log.Debug("以 SMB2 应答 SMB1 多协议协商", "dialects", dialects)
	return out, nil
}

// handleEncrypted 处理一条 SMB3 加密帧（外层是 TRANSFORM_HEADER）。
//
// MS-SMB2 §3.1.4.3：先按本会话的 C2S 密钥解密，得到内部明文 SMB2 消息
// （可能是复合请求链），走普通处理；响应再用本会话的 S2C 密钥重新加密后返回。
// SMB3 加密是**逐会话**的，nonce 取自会话级单调递增计数器（同密钥下绝不重复，
// 否则 CCM/GCM 会直接泄露明文异或值）。
func (c *Connection) handleEncrypted(frame []byte) ([]byte, error) {
	h, err := crypto.ParseTransformHeader(frame)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnknownFrame, err)
	}
	sess := c.state.Session(h.SessionID)
	if sess == nil {
		return nil, fmt.Errorf("%w: 加密帧引用了未知会话 %#x", errUnknownFrame, h.SessionID)
	}
	cipher := crypto.Cipher(c.state.Cipher)
	if cipher == 0 {
		return nil, fmt.Errorf("%w: 会话未协商加密算法", errUnknownFrame)
	}

	plain, err := crypto.Decrypt(cipher, sess.DecryptKey(), frame)
	if err != nil {
		c.log.Warn("SMB3 解密失败", "session", h.SessionID, "err", err)
		return nil, fmt.Errorf("%w: %v", errUnknownFrame, err)
	}
	if !wire.IsSMB2(plain) {
		return nil, fmt.Errorf("%w: 解密结果不是合法 SMB2 消息", errUnknownFrame)
	}

	resp, err := c.handleSMB2Chain(plain)
	if err != nil {
		return nil, err
	}
	if len(resp) == 0 {
		return nil, nil
	}

	enc, err := crypto.Encrypt(cipher, sess.EncryptKey(),
		sess.NextEncryptNonce()[:], h.SessionID, resp)
	if err != nil {
		c.log.Error("SMB3 加密响应失败", "session", h.SessionID, "err", err)
		return nil, fmt.Errorf("%w: %v", errUnknownFrame, err)
	}
	return enc, nil
}

// respMsg 记录复合响应链中一条消息的位置与后处理需求。
type respMsg struct {
	start int
	// signKey 非 nil 时要对本条响应签名（范围含尾部对齐填充）。
	signKey []byte
	// hashConn / hashSession 是 3.1.1 preauth integrity hash 的更新目标。
	hashConn    bool
	hashSession *command.Session
}

// handleSMB2Chain 处理一个 SMB2 帧（可能是复合请求链）。
//
// MS-SMB2 §3.3.5.2.7 / protocol-notes §2：
//   - NextCommand != 0 时本条消息长度即 NextCommand，== 0 时延伸到帧尾；
//   - 每段起点 8 字节对齐；
//   - 响应也必须拼成复合链一次性提交给传输层。
func (c *Connection) handleSMB2Chain(frame []byte) ([]byte, error) {
	chain := &command.Chain{}
	var out []byte
	var msgs []respMsg

	pos := 0
	for {
		seg := frame[pos:]
		hdr, err := wire.ParseHeader(seg)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errBadCompound, err)
		}

		segLen := len(seg)
		if hdr.NextCommand != 0 {
			segLen = int(hdr.NextCommand)
			// NextCommand 必须至少覆盖一个头，且不能越出本帧。
			if segLen < wire.HeaderSize || segLen > len(seg) {
				return nil, fmt.Errorf("%w: NextCommand=%d 超界（剩余 %d）",
					errBadCompound, hdr.NextCommand, len(seg))
			}
		}
		msg := seg[:segLen]

		// rollback 是拼接本条响应**之前**的缓冲长度。CANCEL 这类不产生
		// 响应的消息要连同下面补的对齐字节一起回滚（MS-SMB2 §3.3.5.16）。
		rollback := len(out)

		// 拼接上一条响应：先补 8 字节对齐，再回填它的 NextCommand。
		if len(msgs) > 0 {
			out = pad8(out)
			prev := msgs[len(msgs)-1].start
			setNextCommand(out, prev, uint32(len(out)-prev))
		}

		ctx := c.processMessage(hdr, msg, chain, out)
		if ctx.Suppressed() {
			// 本条消息不产生任何响应字节。除了丢弃它自己的响应头
			// （Context.discard 已做），还要把刚补的对齐填充和上一条的
			// NextCommand 回滚 —— 否则上一条会声称"后面还有消息"，
			// 客户端解析到帧尾之外。
			out = out[:rollback]
			if len(msgs) > 0 {
				setNextCommand(out, msgs[len(msgs)-1].start, 0)
			}
		} else {
			out = ctx.Out
			msgs = append(msgs, respMsg{
				start:       ctx.MsgStart(),
				signKey:     ctx.SignKey,
				hashConn:    ctx.HashResponseConn,
				hashSession: ctx.HashResponseSession,
			})
		}

		if hdr.NextCommand == 0 {
			break
		}
		pos += segLen
		if pos >= len(frame) {
			// NextCommand 指到了帧尾之外。
			return nil, fmt.Errorf("%w: 链在偏移 %d 处越界", errBadCompound, pos)
		}
	}

	c.finishChain(out, msgs)
	return out, nil
}

// finishChain 在复合链拼装完成后执行 preauth hash 更新与签名。
//
// 顺序很重要：
//  1. 此时 NextCommand 与对齐填充都已就位，字节内容不会再变；
//  2. preauth hash 覆盖的是**未签名**的响应（3.1.1 中间轮次的 SESSION_SETUP
//     响应不签名，最终成功的响应签名但不参与 hash，两者不冲突）；
//  3. 签名范围**含尾部对齐填充字节**（protocol-notes §2，极易踩坑）。
func (c *Connection) finishChain(out []byte, msgs []respMsg) {
	for i, m := range msgs {
		end := len(out)
		if i+1 < len(msgs) {
			end = msgs[i+1].start
		}
		seg := out[m.start:end]

		if m.hashConn {
			c.state.UpdatePreauthHash(seg)
		}
		if m.hashSession != nil {
			m.hashSession.UpdatePreauthHash(seg)
		}
		if len(m.signKey) > 0 {
			if err := crypto.SignWith(c.state.SigningAlg(), m.signKey, seg); err != nil {
				c.log.Error("响应签名失败", "err", err)
			}
		}
	}
}

// processMessage 处理复合链中的一条消息，把响应追加到 out。
func (c *Connection) processMessage(hdr wire.Header, msg []byte,
	chain *command.Chain, out []byte) *command.Context {

	ctx := command.NewContext(c.state, chain, hdr, msg, out)

	// —— credit 记账（protocol-notes §12）——
	// 任何响应都至少授予 1 个 credit，否则客户端会停止发送并挂死。
	//
	// 不产生响应的命令（CANCEL）**必须跳过记账**：credit 是靠响应头的
	// CreditGranted 还给客户端的，收不到响应就拿不回 credit。在这里扣减
	// 会让服务端与客户端的水位永久错位，最终把客户端饿死。
	// MS-SMB2 §3.3.5.16 也明确 CANCEL 不消耗 credit。
	if !command.NoResponse(hdr.Command) {
		c.credits.SetMultiCredit(c.state.SupportsMultiCredit())
		charge := c.credits.Charge(hdr.CreditCharge)
		ctx.SetCredits(c.credits.Grant(charge, hdr.Credits))
	}

	// 会话/树定位与签名校验都在 command.Dispatch 内完成
	// （它需要在同一处决定响应是否签名）。
	command.Dispatch(ctx)
	return ctx
}

// pad8 把 dst 补齐到 8 字节对齐（复合响应链要求，MS-SMB2 §3.3.5.2.7）。
func pad8(dst []byte) []byte {
	if n := (8 - len(dst)%8) % 8; n > 0 {
		dst = append(dst, make([]byte, n)...)
	}
	return dst
}

// setNextCommand 回填 buf 中位于 start 处的 SMB2 头的 NextCommand 字段
// （偏移 0x14，4 字节**小端**）。
func setNextCommand(buf []byte, start int, v uint32) {
	off := start + 0x14
	if off+4 > len(buf) {
		return
	}
	buf[off+0] = byte(v)
	buf[off+1] = byte(v >> 8)
	buf[off+2] = byte(v >> 16)
	buf[off+3] = byte(v >> 24)
}
