package server

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// ---------------------------------------------------------------------------
// 复合链中途挂起后续跑（v0.7.2）
//
// 此前 Context.Defer 只允许复合链**末条**挂起：异步响应是单发的，链中间挂起
// 意味着要把一条复合响应链拆成两帧。现在 runChain 会检测到"本条挂起且后面还有
// 消息"，把已处理部分的响应先发出、存下剩余字节与链状态（chainPause），
// 等异步请求完成后由 resumeChain 接着跑。
//
// 这是 oplock 能授予给复合链中间的 CREATE 的前提 —— 那条限制（v0.7.0 引入）
// 正是绕开"链中间挂起没法续跑"。
// ---------------------------------------------------------------------------

// newChainTestConn 造一条带可写 Transport 的测试连接，并返回管道的另一半。
func newChainTestConn(t *testing.T) (*Connection, net.Conn) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, peer := net.Pipe()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = peer.Close()
	})

	c := &Connection{
		log:     log,
		state:   command.NewConn(&command.Settings{Logger: log}, "test", "test"),
		credits: NewCredits(0),
		tr:      NewTransport(srv, 0),
	}
	// 注入异步补发通路：没有它，CHANGE_NOTIFY 会退回 STATUS_NOT_SUPPORTED。
	c.state.SetAsyncSink(asyncSender{c: c})
	return c, peer
}

// collectFrames 在后台把对端收到的帧收进 channel。
//
// 必须放在后台：net.Pipe 是**同步**的，WriteFrame 会一直阻塞到有人读走。
// 而触发写出的那次调用（下面的 handleSMB2Chain(cancel)）就跑在测试 goroutine 上，
// 若读取也放在同一个 goroutine 就会死锁。
func collectFrames(t *testing.T, peer net.Conn) <-chan []byte {
	t.Helper()
	out := make(chan []byte, 16)
	tr := NewTransport(peer, 0)
	go func() {
		for {
			f, err := tr.ReadFrame()
			if err != nil {
				close(out)
				return
			}
			cp := make([]byte, len(f))
			copy(cp, f)
			out <- cp
		}
	}()
	return out
}

func awaitFrame(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("对端连接已关闭，收不到更多帧")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("等待响应帧超时")
		return nil
	}
}

// notifyBody 是 CHANGE_NOTIFY Request 的报文体（MS-SMB2 §2.2.35，32 字节）。
func notifyBody(fid wire.FileID, filter wire.CompletionFilter) []byte {
	b := make([]byte, 32)
	b[0], b[1] = 32, 0 // StructureSize
	// Flags(2) = 0（不监视子树），OutputBufferLength(4)
	b[4], b[5], b[6], b[7] = 0x00, 0x10, 0x00, 0x00 // 4096
	// FileId(16) = Persistent(8) + Volatile(8)，小端。
	// wire 包里的 put 是私有的，server 包只能用导出的 Parse 反解，
	// 因此这里手工写字节（复合链测试只需要能构造出合法请求即可）。
	putU64(b[8:], fid.Persistent)
	putU64(b[16:], fid.Volatile)
	putU32(b[24:], uint32(filter))
	return b
}

func putU32(dst []byte, v uint32) {
	dst[0], dst[1], dst[2], dst[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func putU64(dst []byte, v uint64) {
	for i := 0; i < 8; i++ {
		dst[i] = byte(v >> (8 * i))
	}
}

// chainEnv 准备好一条可用于复合链测试的会话/树/目录句柄。
func chainEnv(t *testing.T, c *Connection) *command.Open {
	t.Helper()

	root := t.TempDir()
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: root, CaseInsensitive: false})
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	share := &command.Share{Name: "data", Type: wire.ShareTypeDisk, FS: fs}
	c.state.Settings.Shares = []*command.Share{share}

	sess, st := c.state.NewSession()
	if st != status.Success {
		t.Fatalf("NewSession: %v", st)
	}
	sess.Establish(&auth.Identity{User: "alice"})
	tree, st := sess.NewTree(share)
	if st != status.Success {
		t.Fatalf("NewTree: %v", st)
	}

	dir := &command.Open{Tree: tree, Session: sess, Path: "", IsDir: true}
	if st := sess.AddOpen(dir); st != status.Success {
		t.Fatalf("AddOpen: %v", st)
	}
	return dir
}

// buildCompoundMsg 拼一条复合链消息：头 + body。
//
// sess/tree 必须带上：CHANGE_NOTIFY 的 needSession / needTree 前置检查会拒掉
// 没带这两个 Id 的请求（回 STATUS_USER_SESSION_DELETED），handler 根本到不了，
// 自然也就不会挂起。CANCEL 也必须用**同一个** SessionId —— 它按
// (SessionId, MessageId) 匹配未决请求。
func buildCompoundMsg(cmd wire.Command, msgID uint64, sess uint64, tree uint32,
	body []byte, next uint32) []byte {
	h := wire.Header{
		Command:     cmd,
		MessageID:   msgID,
		Credits:     1,
		SessionID:   sess,
		TreeID:      tree,
		NextCommand: next,
	}
	return append(h.Append(nil), body...)
}

// padTo8 与运行时的 pad8 同规则（复合链每段起点 8 字节对齐）。
func padTo8(b []byte) []byte {
	if n := (8 - len(b)%8) % 8; n > 0 {
		b = append(b, make([]byte, n)...)
	}
	return b
}

// TestCompoundMidChainDeferResumes：链中间的挂起消息之后，剩下的消息必须
// **同样被处理**，而不是被丢掉（丢掉会造成客户端等一个永远不来的响应）。
func TestCompoundMidChainDeferResumes(t *testing.T) {
	c, peer := newChainTestConn(t)
	frames := collectFrames(t, peer)
	dir := chainEnv(t, c)
	fid := wire.FileID{Persistent: dir.Persistent, Volatile: dir.Volatile}
	sessID, treeID := dir.Session.ID, dir.Tree.ID

	// 链：[ECHO(1)] → [CHANGE_NOTIFY(2)，会被挂起] → [ECHO(3)]
	echo1 := padTo8(buildCompoundMsg(wire.CommandEcho, 1, sessID, treeID, echoBody(), 0))
	notify := padTo8(buildCompoundMsg(wire.CommandChangeNotify, 2, sessID, treeID,
		notifyBody(fid, wire.NotifyChangeFileName), 0))
	echo3 := padTo8(buildCompoundMsg(wire.CommandEcho, 3, sessID, treeID, echoBody(), 0))

	// 回填前两条的 NextCommand（最后一条为 0）。
	frame := make([]byte, 0, len(echo1)+len(notify)+len(echo3))
	frame = append(frame, echo1...)
	frame = append(frame, notify...)
	frame = append(frame, echo3...)
	setNextCommand(frame, 0, uint32(len(echo1)))
	setNextCommand(frame, len(echo1), uint32(len(notify)))

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain: %v", err)
	}

	// 第一帧只回**已处理完的部分** —— 即 ECHO(1)。挂起的那条与它之后的都还没回。
	parts := splitChain(t, resp)
	if len(parts) != 1 {
		t.Fatalf("第一帧应只有 ECHO(1) 的响应，实际 %d 条", len(parts))
	}
	if parts[0].MessageID != 1 {
		t.Fatalf("第一帧应回 MessageId=1，实际 %d", parts[0].MessageID)
	}
	if parts[0].NextCommand != 0 {
		t.Fatal("第一帧末条不应再声明后续消息（剩下的走续跑，不在这一帧里）")
	}

	// 用 CANCEL 触发挂起请求结束：它补发 STATUS_CANCELLED，随后续跑剩下的 ECHO(3)。
	cancel := buildCompoundMsg(wire.CommandCancel, 2, sessID, treeID, cancelBody(), 0)
	if _, err := c.handleSMB2Chain(cancel); err != nil {
		t.Fatalf("CANCEL: %v", err)
	}

	f1 := awaitFrame(t, frames)
	h1, err := wire.ParseHeader(f1)
	if err != nil {
		t.Fatalf("解析补发响应失败: %v", err)
	}
	if h1.MessageID != 2 {
		t.Fatalf("补发的应是 MessageId=2 的响应，实际 %d", h1.MessageID)
	}
	if got := status.Status(h1.Status); got != status.Cancelled {
		t.Fatalf("被取消的挂起请求应回 STATUS_CANCELLED，实际 %v", got)
	}

	// 关键断言：链里剩下的 ECHO(3) **没有丢**。
	f2 := awaitFrame(t, frames)
	h2, err := wire.ParseHeader(f2)
	if err != nil {
		t.Fatalf("解析续跑响应失败: %v", err)
	}
	if h2.MessageID != 3 {
		t.Fatalf("续跑应回 MessageId=3，实际 %d", h2.MessageID)
	}
	if h2.NextCommand != 0 {
		t.Fatal("续跑帧的末条不应再声明后续消息")
	}
}

// TestCompoundLastChainDeferDoesNotPause：末条挂起时不产生续跑状态，
// 响应走异步单发即可 —— 这是两条路径的分界，别让它们互相串。
func TestCompoundLastChainDeferDoesNotPause(t *testing.T) {
	c, peer := newChainTestConn(t)
	frames := collectFrames(t, peer)
	dir := chainEnv(t, c)
	fid := wire.FileID{Persistent: dir.Persistent, Volatile: dir.Volatile}
	sessID, treeID := dir.Session.ID, dir.Tree.ID

	echo1 := padTo8(buildCompoundMsg(wire.CommandEcho, 1, sessID, treeID, echoBody(), 0))
	notify := padTo8(buildCompoundMsg(wire.CommandChangeNotify, 2, sessID, treeID,
		notifyBody(fid, wire.NotifyChangeFileName), 0))

	frame := append(append([]byte{}, echo1...), notify...)
	setNextCommand(frame, 0, uint32(len(echo1)))

	resp, err := c.handleSMB2Chain(frame)
	if err != nil {
		t.Fatalf("handleSMB2Chain: %v", err)
	}
	parts := splitChain(t, resp)
	if len(parts) != 1 || parts[0].MessageID != 1 {
		t.Fatalf("末条挂起时第一帧仍应只回 ECHO(1)，实际 %+v", parts)
	}

	// 取消后应**只有**一条补发响应，不应再有续跑帧。
	cancel := buildCompoundMsg(wire.CommandCancel, 2, sessID, treeID, cancelBody(), 0)
	if _, err := c.handleSMB2Chain(cancel); err != nil {
		t.Fatalf("CANCEL: %v", err)
	}
	f1 := awaitFrame(t, frames)
	h1, _ := wire.ParseHeader(f1)
	if h1.MessageID != 2 {
		t.Fatalf("应只补发 MessageId=2，实际 %d", h1.MessageID)
	}

	select {
	case extra, ok := <-frames:
		if ok {
			t.Fatalf("末条挂起不应产生续跑帧，却多出一条（MessageId 见解析）: %d 字节", len(extra))
		}
	case <-time.After(300 * time.Millisecond):
		// 预期：没有更多帧。
	}
}
