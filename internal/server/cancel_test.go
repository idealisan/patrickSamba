package server

import (
	"io"
	"log/slog"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// newTestConn 构造一条只带协议状态、不带 socket 的连接。
//
// handleSMB2Chain 只用到 state / credits / log，不碰传输层，
// 因此可以脱离网络单测复合链的拼接与回滚逻辑。
func newTestConn() *Connection {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Connection{
		log:     log,
		state:   command.NewConn(&command.Settings{Logger: log}, "test", "test"),
		credits: NewCredits(0),
	}
}

// buildMsg 拼一条 SMB2 请求消息（64 字节头 + body）。
func buildMsg(cmd wire.Command, msgID uint64, flags wire.Flags, body []byte) []byte {
	h := wire.Header{
		Command:   cmd,
		MessageID: msgID,
		Credits:   1,
		Flags:     flags,
	}
	return append(h.Append(nil), body...)
}

// echoBody 是 SMB2 ECHO Request 的报文体（MS-SMB2 §2.2.28）：
// StructureSize(2)=4 + Reserved(2)。
func echoBody() []byte { return []byte{0x04, 0x00, 0x00, 0x00} }

// cancelBody 是 SMB2 CANCEL Request 的报文体（MS-SMB2 §2.2.30）：
// StructureSize(2)=4 + Reserved(2)。
func cancelBody() []byte { return []byte{0x04, 0x00, 0x00, 0x00} }

// buildChain 把多条消息拼成一个复合帧：每段 8 字节对齐，
// 前 n-1 段的 NextCommand 指向下一段（MS-SMB2 §3.3.5.2.7）。
func buildChain(msgs ...[]byte) []byte {
	var out []byte
	starts := make([]int, 0, len(msgs))
	for i, m := range msgs {
		if i > 0 {
			out = pad8(out)
			setNextCommand(out, starts[i-1], uint32(len(out)-starts[i-1]))
		}
		starts = append(starts, len(out))
		out = append(out, m...)
	}
	return out
}

// TestCancelSingleFrameNoResponse：单独一帧 CANCEL 必须一个字节都不回
// （MS-SMB2 §3.3.5.16），且不消耗 credit。
func TestCancelSingleFrameNoResponse(t *testing.T) {
	c := newTestConn()
	before := c.credits.Granted()

	resp, err := c.handleSMB2Chain(buildMsg(wire.CommandCancel, 7, 0, cancelBody()))
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if len(resp) != 0 {
		t.Fatalf("CANCEL 不允许产生任何响应字节，实际 %d 字节: % x", len(resp), resp)
	}
	if got := c.credits.Granted(); got != before {
		t.Fatalf("CANCEL 不应参与 credit 记账：before=%d after=%d", before, got)
	}
}

// TestCancelAsyncSingleFrameNoResponse：带 SMB2_FLAGS_ASYNC_COMMAND 的
// CANCEL（按 AsyncId 匹配）同样不回响应。
func TestCancelAsyncSingleFrameNoResponse(t *testing.T) {
	c := newTestConn()
	msg := buildMsg(wire.CommandCancel, 7, wire.FlagAsyncCommand, cancelBody())

	resp, err := c.handleSMB2Chain(msg)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if len(resp) != 0 {
		t.Fatalf("async CANCEL 不允许产生响应，实际 %d 字节", len(resp))
	}
}

// TestCancelAllSuppressedFrame：整帧全是 CANCEL 时，连空的 Direct TCP 帧
// 都不能发（发空帧会被客户端当成协议错误）。
func TestCancelAllSuppressedFrame(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandCancel, 1, 0, cancelBody()),
		buildMsg(wire.CommandCancel, 2, wire.FlagRelatedOps, cancelBody()),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if len(resp) != 0 {
		t.Fatalf("全 CANCEL 的复合帧不应产生任何字节，实际 %d 字节", len(resp))
	}
}

// TestCancelTailOfCompound：[ECHO, CANCEL] 只回一条 ECHO 响应，
// 且这条响应的 NextCommand 必须被回滚为 0 —— 否则客户端会顺着
// NextCommand 解析到帧尾之外。
func TestCancelTailOfCompound(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandEcho, 1, 0, echoBody()),
		buildMsg(wire.CommandCancel, 2, wire.FlagRelatedOps, cancelBody()),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}

	// ECHO Response = 64 字节头 + 4 字节体。
	if len(resp) != wire.HeaderSize+4 {
		t.Fatalf("期望只回一条 ECHO 响应（%d 字节），实际 %d 字节: % x",
			wire.HeaderSize+4, len(resp), resp)
	}
	h, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析响应头失败: %v", err)
	}
	if h.Command != wire.CommandEcho {
		t.Fatalf("响应命令期望 ECHO，实际 %s", h.Command)
	}
	if h.NextCommand != 0 {
		t.Fatalf("末条响应的 NextCommand 必须为 0，实际 %d", h.NextCommand)
	}
	if h.MessageID != 1 {
		t.Fatalf("响应 MessageId 期望 1，实际 %d", h.MessageID)
	}
}

// TestCancelHeadOfCompound：[CANCEL, ECHO] 只回一条 ECHO 响应，
// 且它必须从缓冲起点开始（前面不能残留 CANCEL 的对齐填充）。
func TestCancelHeadOfCompound(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandCancel, 1, 0, cancelBody()),
		buildMsg(wire.CommandEcho, 2, 0, echoBody()),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}
	if len(resp) != wire.HeaderSize+4 {
		t.Fatalf("期望只回一条 ECHO 响应（%d 字节），实际 %d 字节: % x",
			wire.HeaderSize+4, len(resp), resp)
	}
	h, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析响应头失败: %v", err)
	}
	if h.Command != wire.CommandEcho || h.MessageID != 2 {
		t.Fatalf("期望 ECHO/MessageId=2，实际 %s/%d", h.Command, h.MessageID)
	}
	if h.NextCommand != 0 {
		t.Fatalf("末条响应的 NextCommand 必须为 0，实际 %d", h.NextCommand)
	}
}

// TestCancelBetweenTwoEchos：[ECHO, CANCEL, ECHO] 回两条 ECHO 响应，
// 第一条的 NextCommand 必须精确指向第二条（不能把被抑制的 CANCEL 算进去）。
func TestCancelBetweenTwoEchos(t *testing.T) {
	c := newTestConn()
	frame := buildChain(
		buildMsg(wire.CommandEcho, 1, 0, echoBody()),
		buildMsg(wire.CommandCancel, 2, wire.FlagRelatedOps, cancelBody()),
		buildMsg(wire.CommandEcho, 3, 0, echoBody()),
	)

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain 返回错误: %v", err)
	}

	h1, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析第一条响应头失败: %v", err)
	}
	if h1.NextCommand == 0 {
		t.Fatalf("第一条响应应当有 NextCommand 指向第二条")
	}
	if int(h1.NextCommand) >= len(resp) {
		t.Fatalf("NextCommand=%d 越出响应帧（%d 字节）", h1.NextCommand, len(resp))
	}
	// 第一条响应 68 字节，8 字节对齐后为 72。
	if h1.NextCommand != 72 {
		t.Fatalf("NextCommand 期望 72（68 补齐到 8 的倍数），实际 %d", h1.NextCommand)
	}

	h2, err := wire.ParseHeader(resp[h1.NextCommand:])
	if err != nil {
		t.Fatalf("解析第二条响应头失败: %v", err)
	}
	if h2.Command != wire.CommandEcho || h2.MessageID != 3 {
		t.Fatalf("第二条期望 ECHO/MessageId=3，实际 %s/%d", h2.Command, h2.MessageID)
	}
	if h2.NextCommand != 0 {
		t.Fatalf("末条响应的 NextCommand 必须为 0，实际 %d", h2.NextCommand)
	}
}

// TestCancelIsRegisteredNoResponse 守住注册方式：CANCEL 必须登记为
// noResponse，否则会退回 defaultHandler 回 STATUS_NOT_SUPPORTED，
// 违反 MS-SMB2 §3.3.5.16。
func TestCancelIsRegisteredNoResponse(t *testing.T) {
	if !command.Registered(wire.CommandCancel) {
		t.Fatal("CANCEL 未注册处理器")
	}
	if !command.NoResponse(wire.CommandCancel) {
		t.Fatal("CANCEL 必须登记为不产生响应的命令")
	}
}
