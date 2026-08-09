package command

import (
	"log/slog"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// Chain 是一条复合请求链（compound request）在处理过程中的共享状态。
//
// MS-SMB2 §3.3.5.2.7 / protocol-notes §2：
//   - 置了 SMB2_FLAGS_RELATED_OPERATIONS 的消息复用前一条的
//     SessionId / TreeId / FileId；
//   - 客户端把 FileId 写成全 0xFF 表示"用上一个 CREATE 返回的句柄"；
//   - 链中前序失败后，后续 related 消息必须直接失败，不得执行。
//
// 非复合请求也会有一个只用一次的 Chain，逻辑统一。
type Chain struct {
	// Session / Tree 是链中最近一次成功解析出的会话与树。
	Session *Session
	Tree    *Tree

	// LastOpen 是链中最近一次 CREATE 建立的句柄。
	LastOpen *Open

	// Failed 表示链中已经有消息失败。
	Failed bool
	// FailStatus 是首个失败的状态码，供后续 related 消息复用。
	FailStatus status.Status
}

// Context 是处理一条 SMB2 请求的上下文。
//
// 由 internal/server 填充请求侧字段并预置好响应头占位，
// handler 只负责往 Out 追加**报文体**并设置 Status。
type Context struct {
	// Conn 是连接级协议状态。
	Conn *Conn
	// Chain 是所属复合链的共享状态。
	Chain *Chain

	// Header 是已解析的请求头。
	Header wire.Header
	// Msg 是本条请求的**完整字节**（含 64 字节头，含尾部对齐填充）。
	//
	// wire 包的 Parse* 约定入参就是完整消息（偏移字段相对消息起点），
	// 因此 handler 直接把它传给 wire.ParseXxx 即可。
	Msg []byte

	// Session / Tree 由分发器解析（含 related 继承），可能为 nil。
	Session *Session
	Tree    *Tree

	// RespHeader 是响应头。handler 可以修改（例如 SESSION_SETUP 要写入
	// 新分配的 SessionId）。分发器在 handler 返回后统一回填到 Out。
	RespHeader wire.Header

	// Out 是响应缓冲区。进入 handler 时它已经以本条响应的 64 字节头结尾。
	Out []byte

	// msgStart 是本条响应头在 Out 中的下标（复合链里不为 0）。
	msgStart int

	// Status 是最终写入响应头的 NTSTATUS。
	Status status.Status

	// SignKey 非 nil 时表示本条响应必须签名，由 internal/server 在
	// 复合链拼装完成后统一执行（签名范围含尾部对齐填充）。
	SignKey []byte

	// HashResponseConn 为 true 时，本条响应字节要滚进**连接级** preauth hash
	// （只有 3.1.1 的 NEGOTIATE Response 需要）。
	HashResponseConn bool
	// HashResponseSession 非 nil 时，本条响应字节要滚进该会话的 preauth hash
	// （3.1.1 中间轮次的 SESSION_SETUP Response；最后一条成功的响应
	// **不参与**，见 protocol-notes §6）。
	HashResponseSession *Session

	// Log 是日志句柄。
	Log *slog.Logger

	// suppress 表示本条消息**不产生任何响应字节**（SMB2 CANCEL）。
	suppress bool
}

// Suppressed 报告本条消息是否不产生响应。internal/server 据此回滚响应缓冲。
func (c *Context) Suppressed() bool { return c.suppress }

// NewContext 构造一条请求的处理上下文。
//
// out 必须是响应缓冲区当前内容；本函数会把响应头占位追加进去。
func NewContext(conn *Conn, chain *Chain, hdr wire.Header, msg []byte, out []byte) *Context {
	c := &Context{
		Conn:       conn,
		Chain:      chain,
		Header:     hdr,
		Msg:        msg,
		RespHeader: hdr.Reply(),
		msgStart:   len(out),
		Status:     status.Success,
		Log:        conn.Settings.Log(),
	}
	c.Out = c.RespHeader.Append(out)
	return c
}

// Body 返回请求的报文体（跳过 64 字节头）。
func (c *Context) Body() []byte {
	if len(c.Msg) < wire.HeaderSize {
		return nil
	}
	return c.Msg[wire.HeaderSize:]
}

// MsgStart 返回本条响应头在 Out 中的下标。
func (c *Context) MsgStart() int { return c.msgStart }

// ResetBody 丢弃已经写入的响应体，只保留 64 字节响应头。
// 用于错误兜底：handler 写了一半才发现要失败。
func (c *Context) ResetBody() {
	c.Out = c.Out[:c.msgStart+wire.HeaderSize]
}

// discard 连响应头一起丢弃，使本条消息在缓冲里不留任何痕迹。
func (c *Context) discard() {
	c.Out = c.Out[:c.msgStart]
	c.SignKey = nil
	c.HashResponseConn = false
	c.HashResponseSession = nil
}

// finishHeader 把 Status / Credits 写入 RespHeader 并原地回填到 Out。
//
// 这里利用了 Header.Append 的实现细节：对 c.Out[:msgStart] 追加 64 字节时，
// 底层数组容量足够，会直接原地覆写 [msgStart, msgStart+64)。
func (c *Context) finishHeader() {
	c.RespHeader.Status = uint32(c.Status)
	if len(c.SignKey) > 0 {
		// 签名值本身由 internal/server 在复合链拼装完成后填入，
		// 但 SMB2_FLAGS_SIGNED 必须先置位 —— 它在签名覆盖范围内。
		c.RespHeader.Flags |= wire.FlagSigned
	}
	_ = c.RespHeader.Append(c.Out[:c.msgStart])
}

// SetCredits 设置本条响应授予的 credit 数。
//
// **恒 >= 1**：credit 归零会让客户端停止发送并挂死（protocol-notes §12）。
func (c *Context) SetCredits(n uint16) {
	if n < 1 {
		n = 1
	}
	c.RespHeader.Credits = n
}

// RequireWritable 在树只读时返回 STATUS_MEDIA_WRITE_PROTECTED。
//
// 授权完全由配置决定（AGENTS.md §1.1），不看宿主文件系统 ACL。
func (c *Context) RequireWritable() error {
	if c.Tree == nil {
		return status.NetworkNameDeleted
	}
	if !c.Tree.Writable() {
		return status.MediaWriteProtected
	}
	return nil
}
