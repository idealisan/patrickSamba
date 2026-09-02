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
	"sync/atomic"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
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

	// writeMu 串行化写出。读循环的响应与主动推送的 oplock/lease break
	// （见 oplock_send.go）都必须持有它 —— Transport 复用同一份写头缓冲，
	// 绕过这把锁会造成 data race 与报文交错。
	writeMu sync.Mutex

	// handshakeDone 表示本连接已经有过认证成功的会话，握手期限已解除。
	// 只由读 goroutine 读写。
	handshakeDone bool

	// vniDropPending 表示处理中出现了 FSCTL_VALIDATE_NEGOTIATE_INFO
	// 复核失败（bh5-F6）：按 MS-SMB2 §3.3.5.15.12，服务端在回完
	// STATUS_ACCESS_DENIED 响应后 MUST terminate the transport connection。
	// serve() 在响应帧写出成功后据此断开。
	//
	// 用 atomic：**复合链续跑**（见 resumeChain）会在另一个 goroutine 里
	// 调 processMessage，那里也会写这个标志，于是它不再是"只由读 goroutine
	// 读写"。读取一律用 Swap(false)（读后清零），保证只断连一次。
	vniDropPending atomic.Bool

	closeOnce sync.Once
}

func newConnection(s *Server, nc net.Conn) *Connection {
	remote := nc.RemoteAddr().String()
	local := nc.LocalAddr().String()

	log := s.log.With("remote", remote)
	tr := NewTransport(nc, s.opts.MaxFrameSize)
	tr.SetTimeouts(s.opts.idleTimeout(), s.opts.writeTimeout())

	c := &Connection{
		srv:     s,
		nc:      nc,
		tr:      tr,
		log:     log,
		state:   command.NewConn(s.opts.Settings, remote, local),
		credits: NewCredits(DefaultMaxCredits),
	}

	// 给协议层两条"从别的 goroutine 往这条连接写东西"的通路。
	// 都必须读循环启动**之前**注入。
	c.state.SetBreakSender(breakSender{c: c})
	// 异步响应补发通路：CHANGE_NOTIFY 与阻塞 LOCK 靠它把挂起的请求
	// 稍后补上最终响应（见 command/async.go）。
	c.state.SetAsyncSink(asyncSender{c: c})

	// 认证完成前施加一个短得多的**绝对**期限：未认证连接同样占着
	// MaxConnections 槽位，若也享受 15 分钟的空闲超时，几百条一言不发的
	// TCP 连接就能在不出示任何凭据的情况下让服务对外不可用（slowloris）。
	if ht := s.opts.handshakeTimeout(); ht > 0 {
		tr.SetHardReadDeadline(time.Now().Add(ht))
	} else {
		c.handshakeDone = true
	}
	return c
}

// onSessionEstablished 在本连接第一次出现认证成功的会话时解除握手期限，
// 让连接回落到常规的空闲超时。
//
// 只由读 goroutine 调用。
func (c *Connection) onSessionEstablished() {
	if c.handshakeDone {
		return
	}
	c.handshakeDone = true
	c.tr.SetHardReadDeadline(time.Time{})
	handshakeDeadlineClears.Add(1) // 观测点
	c.log.Debug("认证完成，解除握手期限")
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
			c.countReadExit(err) // 观测点：先按期限来源分类计数，再记日志
			c.logReadError(err)
			return
		}

		resp, err := c.handleFrame(frame)
		if err != nil {
			malformedFrameDrops.Add(1) // 观测点
			c.log.Warn("处理帧失败，断开连接", "err", err)
			return
		}
		if !c.handshakeDone && c.state.HasEstablishedSession() {
			c.onSessionEstablished()
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

		if c.vniDropPending.Swap(false) {
			// bh5-F6：VALIDATE_NEGOTIATE_INFO 复核失败。错误响应已经送达，
			// 现在按 MS-SMB2 §3.3.5.15.12 终止传输连接（defer c.Close()
			// 会拆掉会话/树/句柄）。不断连的话，被篡改的客户端收到
			// ACCESS_DENIED 后仍可继续用这条连接发命令。
			//
			// Swap 而不是 Load：续跑路径也会置这个标志，读后清零保证
			// 读循环与续跑两边只断一次。
			c.log.Warn("VALIDATE_NEGOTIATE_INFO 复核失败，按规范断开连接")
			return
		}
	}
}

// countReadExit 按读循环退出原因更新观测计数器（AGENTS.md §3 确定性事件计数）。
//
// 只由读 goroutine 调用。超时类退出按 lastDeadlineSrc 区分是哪个期限到期：
// 两者都是 net.Error 超时，仅凭错误值无法区分。
func (c *Connection) countReadExit(err error) {
	switch {
	case errors.Is(err, ErrFrameTooLarge):
		frameTooLargeDrops.Add(1)
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			if c.tr.lastDeadlineSrc == deadlineHard {
				handshakeTimeoutCloses.Add(1)
			} else {
				idleTimeoutCloses.Add(1)
			}
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
		return c.handleSMB2Frame(frame, false)

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
		if errors.Is(err, command.ErrSMB1EncryptionRequired) {
			// 措辞与 negotiate.go 的 fail-closed 保持一致，方便运维一把搜出
			// 所有因 encryption_required 被拒的连接。
			c.log.Warn("配置要求加密但本连接协商不出加密算法，拒绝协商",
				"dialect", "2.0.2",
				"dialect_supports_encryption", false,
				"entry", "smb1_negotiate",
				"client_dialects", dialects)
		}
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

	// encrypted=true 会让 command 层豁免验签（MS-SMB2 §3.3.5.2.4：
	// 加密消息不验签），并满足会话级"必须加密"的要求。
	resp, err := c.handleSMB2Frame(plain, true)
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

// handleSMB2Chain 处理一个**明文** SMB2 帧。
//
// 保留这个签名是为了单元测试与调用方便；加密路径走 handleSMB2Frame。
func (c *Connection) handleSMB2Chain(frame []byte) ([]byte, error) {
	return c.handleSMB2Frame(frame, false)
}

// chainPause 记录一条因**中途挂起**而中断的复合链的续跑状态。
//
// 背景：某条消息（例如需要等 oplock break 确认的 CREATE）可以被挂起，
// 让读循环继续收帧。若它是复合链的**末条**，异步响应单独补发即可；
// 若它在链的中间，后面还有消息没处理，就必须把"剩下的字节 + 链状态"存下来，
// 等那条异步请求完成后再接着跑 —— 这就是本结构。
//
// 为什么必须保留 chain：后面的消息可能用 FileId 全 0xFF 引用链中最近一次
// CREATE 建立的句柄（MS-SMB2 §3.3.5.2.7）。链状态丢了，续跑时那些消息
// 就找不到句柄。
type chainPause struct {
	// rest 是**被挂起那条之后**尚未处理的字节。
	//
	// 刻意不含被挂起的那条本身：它的响应走异步路径单独补发
	// （AsyncRequest.Complete → sendUnsolicited），不属于续跑的这一段。
	rest []byte
	// chain 是链级共享状态（Session / Tree / LastOpen / Failed）。
	chain *command.Chain

	encrypted bool

	// async 是被挂起的那个请求；它完成后触发续跑。
	async *command.AsyncRequest
}

// handleSMB2Frame 处理一个 SMB2 帧（可能是复合请求链）。
//
// encrypted 表示本帧来自 SMB3 TRANSFORM_HEADER 解密结果，
// 会透传给每条消息的 Context（影响验签与加密强制）。
//
// MS-SMB2 §3.3.5.2.7 / protocol-notes §2：
//   - NextCommand != 0 时本条消息长度即 NextCommand，== 0 时延伸到帧尾；
//   - 每段起点 8 字节对齐；
//   - 响应也必须拼成复合链一次性提交给传输层。
//
// 中途挂起时的处置：本帧先回**已处理完的那些**消息，剩下的交给续跑
// （见 chainPause 与 resumeChain）。
func (c *Connection) handleSMB2Frame(frame []byte, encrypted bool) ([]byte, error) {
	out, msgs, pause, err := c.runChain(frame, &command.Chain{}, encrypted)
	if err != nil {
		return nil, err
	}
	if pause != nil {
		// 登记续跑。**必须**在 finishChain 之前？不必，但必须在返回之前：
		// 一旦本帧被写出，客户端就可能发来 break 确认，异步请求随之完成；
		// 晚于这个时刻登记会漏掉续跑（SetResume 内部处理了"已完成"的竞态，
		// 所以顺序上先登记更稳妥）。
		pause.async.SetResume(func() { c.resumeChain(pause) })
	}
	c.finishChain(out, msgs)
	return out, nil
}

// runChain 处理一段字节里的复合链，返回响应缓冲、各消息的后处理需求，
// 以及（若中途挂起）续跑所需的状态。
//
// 拆成独立方法是为了让**读循环**与**续跑**共用同一套链路处理逻辑 ——
// 两条路径若各写一份，复合链的边界处理迟早会分叉（那类分叉的表现是
// "末条挂起正常、中间挂起就把响应拼错"）。
func (c *Connection) runChain(frame []byte, chain *command.Chain, encrypted bool) (
	out []byte, msgs []respMsg, pause *chainPause, err error) {

	pos := 0
	for {
		seg := frame[pos:]
		hdr, herr := wire.ParseHeader(seg)
		if herr != nil {
			return nil, nil, nil, fmt.Errorf("%w: %v", errBadCompound, herr)
		}

		segLen := len(seg)
		if hdr.NextCommand != 0 {
			segLen = int(hdr.NextCommand)
			// NextCommand 必须至少覆盖一个头，且不能越出本帧。
			if segLen < wire.HeaderSize || segLen > len(seg) {
				return nil, nil, nil, fmt.Errorf("%w: NextCommand=%d 超界（剩余 %d）",
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

		ctx := c.processMessage(hdr, msg, chain, out, encrypted)
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

		// 中途挂起：把剩下的字节交出去，等那条异步请求完成后再续跑。
		//
		// 末条消息挂起时不用这么做 —— 它后面没有消息，异步响应单独补发
		// 就够了（这也正是 Context.Defer 只允许末条时才有意义的由来；
		// 中间挂起由这里兜住）。
		if ctx.Async() != nil && hdr.NextCommand != 0 {
			return out, msgs, &chainPause{
				rest:      frame[pos+segLen:],
				chain:     chain,
				encrypted: encrypted,
				async:     ctx.Async(),
			}, nil
		}

		if hdr.NextCommand == 0 {
			break
		}
		pos += segLen
		if pos >= len(frame) {
			// NextCommand 指到了帧尾之外。
			return nil, nil, nil, fmt.Errorf("%w: 链在偏移 %d 处越界", errBadCompound, pos)
		}
	}
	return out, msgs, nil, nil
}

// resumeChain 续跑一条被挂起打断的复合链，并把响应写成新的一帧。
//
// 它在**异步请求的 goroutine** 上运行，与读循环并发 —— 因此这里碰到的
// 每一点连接状态都必须能承受并发（vniDropPending 已改成 atomic 就是为了它）。
//
// ⚠️ 调用方保证：本函数在 AsyncRequest 补发完响应之后才被调用
// （见 AsyncRequest.Complete），否则会与补发抢同一把写锁。
func (c *Connection) resumeChain(p *chainPause) {
	out, msgs, next, err := c.runChain(p.rest, p.chain, p.encrypted)
	if err != nil {
		// 续跑阶段遇到致命协议错误（复合链格式坏了）。这里**不在**读循环里，
		// 没有"返回 error 让 serve() 断连"这条路，只能记日志后主动关连接。
		c.log.Warn("复合链续跑失败，断开连接", "err", err)
		c.Close()
		return
	}
	if next != nil {
		// 剩下的消息里又出现一次挂起：递归登记，别把后面那段丢了。
		next.async.SetResume(func() { c.resumeChain(next) })
	}
	if len(out) == 0 {
		return
	}
	c.finishChain(out, msgs)

	c.writeMu.Lock()
	werr := c.tr.WriteFrame(out)
	c.writeMu.Unlock()
	if werr != nil {
		c.log.Debug("写出续跑响应失败", "err", werr)
		return
	}
	if c.vniDropPending.Swap(false) {
		c.log.Warn("VALIDATE_NEGOTIATE_INFO 复核失败，按规范断开连接")
		c.Close()
	}
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
	chain *command.Chain, out []byte, encrypted bool) *command.Context {

	ctx := command.NewContext(c.state, chain, hdr, msg, out)
	ctx.Encrypted = encrypted

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

	// bh5-F6：VNI 复核失败必须断连（MS-SMB2 §3.3.5.15.12 MUST terminate
	// the transport connection）。command 层的 handler 错误会转成 NTSTATUS
	// 响应、不会向上传播，所以在这里按「请求是 VNI + 最终状态是
	// ACCESS_DENIED」识别复核失败 —— 这正是 ioctlValidateNegotiate 所有
	// 校验失败分支（GUID/SecurityMode/Capabilities/方言/输入畸形）的统一出口；
	// 校验通过（SUCCESS）与 3.1.1 的 FILE_CLOSED 分支都不命中。
	if ctx.Status == status.AccessDenied && isValidateNegotiate(hdr, msg) {
		c.vniDropPending.Store(true)
	}
	return ctx
}

// isValidateNegotiate 报告这条请求是否为 FSCTL_VALIDATE_NEGOTIATE_INFO。
// 报文解析失败按「不是」处理 —— 那种请求走不到 VNI handler。
func isValidateNegotiate(hdr wire.Header, msg []byte) bool {
	if hdr.Command != wire.CommandIoctl {
		return false
	}
	req, err := wire.ParseIoctlRequest(msg)
	return err == nil && req.IsFSCTL() && req.CtlCode == wire.FSCTLValidateNegotiateInfo
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
