package server

import (
	"context"
	"encoding/binary"
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

// 复合请求（compound request）相关常量。
const (
	// MaxCompoundMessages 是单帧允许包含的消息条数上限。
	//
	// MS-SMB2 没有规定上限，但不设限会让一个 1 MiB 的帧塞进上万条 64 字节
	// 空头，放大处理开销（AGENTS.md §8 资源限制）。真实客户端的链长个位数。
	MaxCompoundMessages = 64

	// compoundAlign 是复合链中每条消息的对齐要求（MS-SMB2 §3.3.5.2.7：
	// NextCommand 必须是 8 的倍数）。
	compoundAlign = 8

	// headerNextCommandOffset 是 SMB2 头中 NextCommand 字段的偏移
	// （MS-SMB2 §2.2.1.2，小端 4 字节）。
	//
	// 复合响应里一条消息的 NextCommand 只有在**下一条**处理完之后才知道，
	// 因此必须回填 —— 这是唯一需要在 server 层直接触碰报文字节的地方。
	headerNextCommandOffset = 0x14

	// defaultResponseBufSize 是响应缓冲的初始容量，够装下绝大多数控制类响应。
	defaultResponseBufSize = 4096
)

// transformProtocolID 是 SMB3 TRANSFORM_HEADER 的魔数 0xFD 'S' 'M' 'B'
// （MS-SMB2 §2.2.41）。收到它说明客户端发来了加密报文。
var transformProtocolID = [4]byte{0xFD, 'S', 'M', 'B'}

// 连接级致命错误：出现即断开连接。
var (
	// errNotSMB 表示帧既不是 SMB1 也不是 SMB2/SMB3。
	errNotSMB = errors.New("smb conn: 未知的报文魔数")
	// errSMB1Disabled 表示收到 SMB1 报文但未启用 SMB1 协商入口。
	errSMB1Disabled = errors.New("smb conn: 收到 SMB1 报文但 SMB1 协商入口未启用")
	// errEncryptedNotSupported 表示收到加密报文但本连接尚不支持解密。
	errEncryptedNotSupported = errors.New("smb conn: 收到 TRANSFORM 加密报文，尚未支持")
	// errHandlerPanic 表示 handler 崩溃，连接状态不再可信。
	errHandlerPanic = errors.New("smb conn: 请求处理 panic")
	// errCompoundTooLong 表示复合链条数超限。
	errCompoundTooLong = errors.New("smb conn: 复合请求条数超限")
)

// Connection 是一条客户端 TCP 连接（MS-SMB2 §3.3.1.5 Connection 的宿主）。
//
// 职责边界：本类型只做**传输与编排** —— 收帧、拆复合链、把每条消息交给
// internal/smb/command 分发、拼装并写回。所有 SMB 语义都在 command 包里。
type Connection struct {
	srv *Server
	nc  net.Conn
	t   *Transport
	log *slog.Logger

	// conn 是本连接的 SMB2 协议状态（会话表、协商结果……）。
	conn *command.Conn

	// credits 是本连接的 credit 池。
	credits *Credits

	// wmu 串行化写出。目前只有读循环这一个写者，但异步命令
	// （SMB2_FLAGS_ASYNC_COMMAND）会从别的 goroutine 写，先把锁准备好。
	wmu sync.Mutex

	closeOnce sync.Once
}

// newConnection 为一条已接受的 TCP 连接创建 Connection。
func newConnection(s *Server, nc net.Conn) *Connection {
	t := NewTransport(nc, s.opts.MaxFrameSize)
	t.SetTimeouts(s.opts.idleTimeout(), s.opts.writeTimeout())

	remote := nc.RemoteAddr().String()
	local := nc.LocalAddr().String()

	return &Connection{
		srv:     s,
		nc:      nc,
		t:       t,
		log:     s.log.With("remote", remote),
		conn:    command.NewConn(s.opts.Settings, remote, local),
		credits: NewCredits(0),
	}
}

// RemoteAddr 返回对端地址，用于日志与测试。
func (c *Connection) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }

// SMBConn 返回本连接的协议状态，用于测试。
func (c *Connection) SMBConn() *command.Conn { return c.conn }

// Close 关闭连接并释放其上的全部会话资源。可重复调用。
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		_ = c.t.Close()
		c.conn.Close()
	})
}

// serve 是连接的主循环：读帧 → 处理 → 写帧，直到出错或对端关闭。
func (c *Connection) serve(ctx context.Context) {
	defer c.Close()

	// ctx 取消时主动关闭套接字，让阻塞中的 ReadFrame 立刻返回。
	stop := context.AfterFunc(ctx, c.Close)
	defer stop()

	c.log.Debug("SMB 连接建立")

	for {
		frame, err := c.t.ReadFrame()
		if err != nil {
			c.logDisconnect(err)
			return
		}

		resp, err := c.handleFrame(frame)
		if err != nil {
			// 到这一步说明帧本身不可解析或状态已不可信 —— SMB2 在传输层
			// 没有"协议错误"的表达方式，唯一正确的做法是断开。
			c.log.Warn("请求无法处理，断开连接", "err", err)
			return
		}
		if len(resp) == 0 {
			// CANCEL 之类的命令没有响应（MS-SMB2 §2.2.26）。
			continue
		}
		if err := c.writeFrame(resp); err != nil {
			c.log.Debug("写响应失败", "err", err)
			return
		}
	}
}

// writeFrame 写出一个 Direct TCP 帧，串行化以支持将来的异步响应。
func (c *Connection) writeFrame(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.t.WriteFrame(payload)
}

// logDisconnect 把读循环的退出原因记成合适的级别。
func (c *Connection) logDisconnect(err error) {
	switch {
	case errors.Is(err, io.EOF):
		c.log.Debug("对端关闭连接")
	case errors.Is(err, net.ErrClosed):
		c.log.Debug("连接已关闭")
	case errors.Is(err, io.ErrUnexpectedEOF):
		c.log.Debug("对端在传输中断开")
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			c.log.Debug("连接空闲超时")
			return
		}
		c.log.Warn("读帧失败", "err", err)
	}
}

// handleFrame 处理一个完整的 Direct TCP 帧，返回要写回的响应帧（可能为空）。
//
// handler 里的 panic 在这里兜底：外部输入绝不允许打崩服务（AGENTS.md §5），
// 但崩过之后这条连接的协议状态已经不可信，因此只保连接不保会话 —— 记日志、
// 断开、让客户端重连。
func (c *Connection) handleFrame(frame []byte) (resp []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("请求处理 panic", "panic", r, "stack", string(debug.Stack()))
			resp, err = nil, errHandlerPanic
		}
	}()

	switch {
	case wire.IsSMB2(frame):
		return c.handleSMB2Frame(frame)

	case isTransform(frame):
		// TODO(M5): SMB3 TRANSFORM_HEADER 解密（MS-SMB2 §2.2.41 / §3.3.5.2.14）。
		return nil, errEncryptedNotSupported

	case wire.IsSMB1(frame):
		// SMB1 多协议协商入口：客户端先发 SMB1 SMB_COM_NEGOTIATE，
		// 方言串里带 "SMB 2.???" 表示愿意升级到 SMB2（MS-SMB2 §3.3.5.3.1）。
		if !c.conn.Settings.AllowSMB1Negotiate {
			return nil, errSMB1Disabled
		}
		// TODO(server): 实现 SMB1 多协议协商入口，回一条 DialectRevision
		// 为 0x02FF（wildcard）的 SMB2 NEGOTIATE Response。
		// 现代 smbclient/Windows 默认直接发 SMB2，M1 不依赖这条路径。
		return nil, errSMB1Disabled

	default:
		return nil, fmt.Errorf("%w: %02X %02X %02X %02X", errNotSMB,
			frame[0], frame[1], frame[2], frame[3])
	}
}

// isTransform 报告 b 是否为 SMB3 TRANSFORM_HEADER（加密报文）。
func isTransform(b []byte) bool {
	return len(b) >= 4 && b[0] == transformProtocolID[0] && b[1] == transformProtocolID[1] &&
		b[2] == transformProtocolID[2] && b[3] == transformProtocolID[3]
}

// respMsg 记录复合响应中一条消息的位置与签名需求。
type respMsg struct {
	// start 是该条响应头在响应缓冲中的下标。
	start int
	// signKey 非 nil 时表示该条响应要签名。
	signKey []byte
}

// handleSMB2Frame 拆解复合请求链，逐条分发，并拼装成一个复合响应帧。
//
// 复合链规则（MS-SMB2 §3.3.5.2.7 / protocol-notes §2）：
//   - 请求侧：每条消息头的 NextCommand 是**到下一条消息头的字节偏移**
//     （相对本条头起点），0 表示本条是最后一条；
//   - 响应侧：同样用 NextCommand 串起来，每条响应必须 8 字节对齐；
//   - 置了 RELATED_OPERATIONS 的消息复用前一条的 SessionId/TreeId/FileId，
//     且前序失败后必须直接失败（由 command.Dispatch 通过 Chain 实现）。
func (c *Connection) handleSMB2Frame(frame []byte) ([]byte, error) {
	chain := &command.Chain{}
	out := make([]byte, 0, defaultResponseBufSize)
	msgs := make([]respMsg, 0, 4)

	off := 0
	for n := 0; ; n++ {
		if n >= MaxCompoundMessages {
			return nil, fmt.Errorf("%w: > %d", errCompoundTooLong, MaxCompoundMessages)
		}

		msg, next, err := splitMessage(frame, off)
		if err != nil {
			return nil, err
		}

		hdr, err := wire.ParseHeader(msg)
		if err != nil {
			return nil, err
		}

		if hdr.Command == wire.CommandCancel {
			// CANCEL 没有响应（MS-SMB2 §2.2.26）。异步请求尚未实现，
			// 因此这里除了记日志无事可做。
			// TODO(M4): 有异步命令后按 MessageId/AsyncId 取消对应操作。
			c.log.Debug("收到 CANCEL，当前无可取消的异步请求", "message_id", hdr.MessageID)
			if next == 0 {
				break
			}
			off = next
			continue
		}

		// 从第二条起：先把缓冲补齐到 8 字节边界，再回填**上一条**响应的
		// NextCommand（= 本条响应头起点 - 上一条响应头起点）。
		if len(msgs) > 0 {
			out = padTo(out, compoundAlign)
			prev := msgs[len(msgs)-1].start
			binary.LittleEndian.PutUint32(out[prev+headerNextCommandOffset:], uint32(len(out)-prev))
		}
		start := len(out)

		ctx := command.NewContext(c.conn, chain, hdr, msg, out)

		// Credit 记账：先归一化 CreditCharge，再决定本条响应授予多少。
		// Grant 恒返回 >= 1，这是防客户端挂死的底线（protocol-notes §12）。
		charge := c.credits.Charge(hdr.CreditCharge)
		ctx.SetCredits(c.credits.Grant(charge, hdr.Credits))

		command.Dispatch(ctx)
		out = ctx.Out

		msgs = append(msgs, respMsg{start: start, signKey: ctx.SignKey})

		// 方言在 NEGOTIATE 处理完才定型，之后才能启用多信用。
		c.credits.SetMultiCredit(c.conn.SupportsMultiCredit())

		if next == 0 {
			break
		}
		off = next
	}

	if len(msgs) == 0 {
		// 整帧都是 CANCEL：不回任何东西。
		return nil, nil
	}

	if err := c.signResponses(out, msgs); err != nil {
		return nil, err
	}
	return out, nil
}

// splitMessage 从 frame 的 off 处切出一条消息。
//
// 返回的 msg **包含**复合链里的尾部对齐填充 —— 签名计算覆盖填充
// （MS-SMB2 §3.1.4.1），切短了会验签失败。
func splitMessage(frame []byte, off int) (msg []byte, next int, err error) {
	if off < 0 || off >= len(frame) {
		return nil, 0, fmt.Errorf("复合请求偏移越界: %d (帧长 %d)", off, len(frame))
	}
	rest := frame[off:]
	if len(rest) < wire.HeaderSize {
		return nil, 0, fmt.Errorf("复合请求剩余 %d 字节，不足一个 SMB2 头", len(rest))
	}

	nc := binary.LittleEndian.Uint32(rest[headerNextCommandOffset:])
	if nc == 0 {
		return rest, 0, nil
	}
	// NextCommand 必须至少跨过一个头，且不能越出本帧。
	if nc < wire.HeaderSize || uint64(nc) > uint64(len(rest)) {
		return nil, 0, fmt.Errorf("非法 NextCommand: %d (剩余 %d)", nc, len(rest))
	}
	if nc%compoundAlign != 0 {
		return nil, 0, fmt.Errorf("NextCommand %d 未按 %d 字节对齐", nc, compoundAlign)
	}
	return rest[:nc], off + int(nc), nil
}

// signResponses 给需要签名的响应逐条计算签名。
//
// 必须在整个复合链拼装完成之后做：签名范围是**本条消息的完整字节**，
// 含尾部对齐填充，而填充只有在下一条开始写入时才确定。
func (c *Connection) signResponses(out []byte, msgs []respMsg) error {
	for i, m := range msgs {
		if len(m.signKey) == 0 {
			continue
		}
		end := len(out)
		if i+1 < len(msgs) {
			end = msgs[i+1].start
		}
		if err := crypto.Sign(uint16(c.conn.Dialect), m.signKey, out[m.start:end]); err != nil {
			return fmt.Errorf("响应签名失败: %w", err)
		}
	}
	return nil
}

// padTo 把 b 补零到 align 的整数倍。
func padTo(b []byte, align int) []byte {
	if n := len(b) % align; n != 0 {
		b = append(b, make([]byte, align-n)...)
	}
	return b
}
