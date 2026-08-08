package command

import (
	"fmt"

	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// Handler 处理一条 SMB2 请求。
//
// 约定：
//   - 成功时把**报文体**追加到 ctx.Out（响应头已由分发器预置），返回 nil；
//   - 失败时返回 error。返回 status.Status 可以精确指定 NTSTATUS，
//     返回其它错误由 status.FromVFSError 统一映射（AGENTS.md §5 P5，
//     禁止在 handler 里裸写魔数）。
//   - 需要返回带响应体的非 SUCCESS 状态（如 SESSION_SETUP 的
//     STATUS_MORE_PROCESSING_REQUIRED、QUERY_INFO 的 STATUS_BUFFER_OVERFLOW）时，
//     直接设置 ctx.Status 并返回 nil。
type Handler func(ctx *Context) error

// handlerSpec 描述一个命令的处理器及其前置条件。
type handlerSpec struct {
	fn Handler
	// needSession 表示必须有一个**已认证**的会话。
	needSession bool
	// needTree 表示必须有一个有效的树连接。
	needTree bool
}

// handlers 是命令分发表（AGENTS.md §5 P2 策略模式）。
//
// 新增一个命令只需要在自己的文件里 init() 调 register()，
// **不需要改动本文件**。未注册的命令统一走 defaultHandler。
var handlers = map[wire.Command]handlerSpec{}

// register 登记一个命令处理器。重复注册会 panic —— 这是编码错误，
// 发生在进程启动阶段（init），不会影响运行中的服务。
func register(cmd wire.Command, needSession, needTree bool, fn Handler) {
	if _, dup := handlers[cmd]; dup {
		panic(fmt.Sprintf("command: %s 重复注册", cmd))
	}
	handlers[cmd] = handlerSpec{fn: fn, needSession: needSession, needTree: needTree}
}

// Registered 报告某个命令是否已经实现，用于日志与测试。
func Registered(cmd wire.Command) bool {
	_, ok := handlers[cmd]
	return ok
}

// defaultHandler 处理所有未实现的命令（AGENTS.md §5 P2）。
func defaultHandler(ctx *Context) error {
	ctx.Log.Debug("未实现的 SMB2 命令", "command", ctx.Header.Command.String(),
		"remote", ctx.Conn.RemoteAddr)
	return status.NotSupported
}

// Dispatch 处理一条请求并把响应写入 ctx.Out。
//
// 无论成功失败都会产生一条格式正确的响应 —— SMB2 不允许对请求静默不答，
// 客户端会一直等到超时。
func Dispatch(ctx *Context) {
	if err := dispatch(ctx); err != nil {
		ctx.fail(status.FromVFSError(err))
	}
	ctx.finishHeader()

	// 把本条解析/新建出的会话与树记入链，供后续 related 消息继承。
	if ctx.Session != nil {
		ctx.Chain.Session = ctx.Session
	}
	if ctx.Tree != nil {
		ctx.Chain.Tree = ctx.Tree
	}

	if ctx.Status.IsError() {
		ctx.Chain.Failed = true
		if ctx.Chain.FailStatus == 0 {
			ctx.Chain.FailStatus = ctx.Status
		}
	}
}

// dispatch 执行前置检查与 handler。
func dispatch(ctx *Context) error {
	// 复合链中前序已失败：后续 related 消息必须直接失败，不得执行
	// （MS-SMB2 §3.3.5.2.7）。
	if ctx.Header.IsRelated() && ctx.Chain.Failed {
		st := ctx.Chain.FailStatus
		if st == 0 {
			st = status.Unsuccessful
		}
		return st
	}

	spec, ok := handlers[ctx.Header.Command]
	if !ok {
		spec = handlerSpec{fn: defaultHandler}
	}

	// 先定位会话与树（含 related 继承），再校验签名 —— 验签需要会话密钥。
	if err := ctx.resolve(); err != nil {
		return err
	}
	if err := ctx.checkSignature(); err != nil {
		return err
	}

	if spec.needSession {
		if ctx.Session == nil {
			return status.UserSessionDeleted
		}
		if !ctx.Session.Established() {
			return status.AccessDenied
		}
	}
	if spec.needTree && ctx.Tree == nil {
		return status.NetworkNameDeleted
	}

	return spec.fn(ctx)
}

// resolve 根据请求头定位会话与树，并处理复合链里的继承语义。
//
// MS-SMB2 §3.3.5.2.7：置了 SMB2_FLAGS_RELATED_OPERATIONS 的消息复用
// 前一条的 SessionId / TreeId，头里的对应字段应当被忽略。
func (c *Context) resolve() error {
	if c.Header.IsRelated() {
		c.Session = c.Chain.Session
		c.Tree = c.Chain.Tree
		// 响应头要回填真实的 Id，客户端据此对账。
		if c.Session != nil {
			c.RespHeader.SessionID = c.Session.ID
		}
		if c.Tree != nil {
			c.RespHeader.TreeID = c.Tree.ID
		}
		return nil
	}

	if id := c.Header.SessionID; id != 0 {
		s := c.Conn.Session(id)
		if s == nil {
			// MS-SMB2 §3.3.5.2.9：找不到会话回 STATUS_USER_SESSION_DELETED。
			return status.UserSessionDeleted
		}
		c.Session = s
		c.Chain.Session = s
	}

	if id := c.Header.TreeID; id != 0 {
		if c.Session == nil {
			return status.UserSessionDeleted
		}
		t := c.Session.Tree(id)
		if t == nil {
			// MS-SMB2 §3.3.5.2.11：找不到树回 STATUS_NETWORK_NAME_DELETED。
			return status.NetworkNameDeleted
		}
		c.Tree = t
		c.Chain.Tree = t
	}
	return nil
}

// checkSignature 校验请求签名，并决定响应是否需要签名。
//
// MS-SMB2 §3.3.5.2.4：
//   - 请求置了 SMB2_FLAGS_SIGNED 且会话已建立 → 必须验签，失败回
//     STATUS_ACCESS_DENIED；
//   - 会话要求签名而请求没签 → STATUS_ACCESS_DENIED；
//   - 验签通过的请求，其响应也必须签名。
//
// SESSION_SETUP 是例外：认证完成前还没有会话密钥，签名由该命令的 handler
// 在密钥派生之后自行处理（§3.3.5.5.3）。
func (c *Context) checkSignature() error {
	if c.Header.Command == wire.CommandSessionSetup {
		return nil
	}

	s := c.Session
	if s == nil || !s.Established() {
		return nil
	}

	key := s.SigningKey()
	signed := c.Header.IsSigned()

	if !signed {
		if s.SigningRequired() {
			c.Log.Warn("会话要求签名但请求未签名",
				"command", c.Header.Command.String(), "session", s.ID)
			return status.AccessDenied
		}
		return nil
	}

	if len(key) == 0 {
		// 客户端声称签了名，但我们没有密钥可校验 —— 只能拒绝。
		return status.AccessDenied
	}
	if err := crypto.Verify(uint16(c.Conn.Dialect), key, c.Msg); err != nil {
		c.Log.Warn("请求签名校验失败",
			"command", c.Header.Command.String(), "session", s.ID, "err", err)
		return status.AccessDenied
	}

	// 验签通过：响应也要签名。实际计算由 internal/server 在复合链拼装完成
	// 之后统一执行（签名范围含尾部对齐填充）。
	c.SignKey = key
	return nil
}

// fail 丢弃已写入的响应体，改写成一条 SMB2 ERROR Response。
func (c *Context) fail(st status.Status) {
	if st == status.Success {
		st = status.Unsuccessful
	}
	c.ResetBody()
	c.Status = st

	// MS-SMB2 §2.2.2：ByteCount 为 0 时 ErrorData 必须补 1 字节占位，
	// 这由 wire.ErrorResponse.Append 负责。
	out, err := (&wire.ErrorResponse{}).Append(c.Out)
	if err != nil {
		// ErrorResponse 是定长结构，编码不可能失败；真出错也不能让连接静默，
		// 保底写一条最小 ERROR body。
		c.Log.Error("编码 ERROR Response 失败", "err", err)
		c.ResetBody()
		c.Out = append(c.Out, 0x09, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
		return
	}
	c.Out = out
}
