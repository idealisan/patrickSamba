package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/smb/command"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// newPipeConn 构造一条跑在 net.Pipe 上的服务端连接，并返回客户端侧的
// Transport 用于读取服务端写出的帧。
//
// net.Pipe 是**同步无缓冲**的：每次 Write 都要等对端 Read 才返回。
// 这正是我们想要的 —— 想验证"两条写出路径会不会交错"，就需要写出真的
// 落到同一个流上，而不是各自进各自的缓冲区。
func newPipeConn(t *testing.T) (*Connection, *Transport) {
	t.Helper()

	srvEnd, cliEnd := net.Pipe()
	t.Cleanup(func() {
		_ = srvEnd.Close()
		_ = cliEnd.Close()
	})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &Connection{
		nc:      srvEnd,
		tr:      NewTransport(srvEnd, 0),
		log:     log,
		state:   command.NewConn(&command.Settings{Logger: log}, "test", "test"),
		credits: NewCredits(0),
	}
	c.state.SetBreakSender(breakSender{c: c})
	return c, NewTransport(cliEnd, 0)
}

// readFrameAsync 在后台读一帧，返回一个只写一次的 channel。
// 配合 net.Pipe 的同步语义使用：必须先挂上读，写才不会死锁。
func readFrameAsync(tr *Transport) <-chan []byte {
	ch := make(chan []byte, 1)
	go func() {
		f, err := tr.ReadFrame()
		if err != nil {
			close(ch)
			return
		}
		ch <- f
	}()
	return ch
}

func mustRecv(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("读取帧失败")
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("等待帧超时")
		return nil
	}
}

// TestOplockBreakNotificationHeaderIsExact 逐字段锁定 oplock break 通知的
// SMB2 头。
//
// 这些取值不是随手填的，逐条对齐 Samba source3/smbd/smb2_server.c 的
// smbd_smb2_break_send()（MS-SMB2 §3.3.4.6）。任何一条被"顺手优化"掉
// （比如好心给 1 个 credit、或者给通知也签个名）都会在这里红。
func TestOplockBreakNotificationHeaderIsExact(t *testing.T) {
	c, cli := newPipeConn(t)

	fid := wire.FileID{Persistent: 0x1122334455667788, Volatile: 0x99AABBCCDDEEFF00}
	ch := readFrameAsync(cli)

	err := c.state.BreakSender().SendOplockBreak(
		command.BreakTarget{SessionID: 0x0102030405060708, TreeID: 7},
		wire.OplockBreak{OplockLevel: wire.OplockLevelII, FileID: fid},
	)
	if err != nil {
		t.Fatalf("SendOplockBreak: %v", err)
	}

	frame := mustRecv(t, ch)
	if len(frame) != wire.HeaderSize+24 {
		t.Fatalf("帧长 = %d，期望 %d（64 头 + 24 体）", len(frame), wire.HeaderSize+24)
	}

	h, err := wire.ParseHeader(frame)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Command != wire.CommandOplockBreak {
		t.Errorf("Command = %#x，期望 OPLOCK_BREAK(0x0012)", uint16(h.Command))
	}
	if h.MessageID != breakNotifyMessageID {
		t.Errorf("MessageId = %#x，期望 0xFFFFFFFFFFFFFFFF（§3.3.4.6）", h.MessageID)
	}
	if h.Flags != wire.FlagServerToRedir {
		t.Errorf("Flags = %#x，期望仅 SERVER_TO_REDIR(0x1)；"+
			"特别是不得置 SMB2_FLAGS_SIGNED", uint32(h.Flags))
	}
	if h.CreditCharge != 0 {
		t.Errorf("CreditCharge = %d，期望 0", h.CreditCharge)
	}
	if h.Credits != 0 {
		t.Errorf("Credits = %d，期望 0：通知不是响应，授予 credit 会让客户端水位虚高", h.Credits)
	}
	if h.Status != 0 {
		t.Errorf("Status = %#x，期望 0", h.Status)
	}
	if h.NextCommand != 0 {
		t.Errorf("NextCommand = %d，期望 0：通知永远单发", h.NextCommand)
	}
	if h.TreeID != 0 {
		t.Errorf("TreeId = %d，期望 0（恒 0，与句柄实际所在的树无关）", h.TreeID)
	}
	if h.Reserved != 0 {
		t.Errorf("Reserved = %#x，期望 0", h.Reserved)
	}
	if h.SessionID != 0x0102030405060708 {
		t.Errorf("SessionId = %#x，期望回填 BreakTarget.SessionID", h.SessionID)
	}
	var zeroSig [wire.SignatureSize]byte
	if !bytes.Equal(h.Signature[:], zeroSig[:]) {
		t.Errorf("Signature = % x，期望全 0：break 通知不签名", h.Signature)
	}

	// 报文体照 §2.2.23.1 复核一遍。
	b, err := wire.ParseOplockBreak(frame)
	if err != nil {
		t.Fatalf("ParseOplockBreak: %v", err)
	}
	if b.OplockLevel != wire.OplockLevelII {
		t.Errorf("OplockLevel = %#x，期望 LEVEL_II(0x01)", byte(b.OplockLevel))
	}
	if b.FileID != fid {
		t.Errorf("FileId = %+v，期望 %+v", b.FileID, fid)
	}
}

// TestLeaseBreakNotificationSessionIDIsAlwaysZero 是"lease 族 SessionId 恒 0"
// 这条规则的**可证伪**用例。
//
// 反向对照就在同一个测试里：BreakTarget 里显式塞了一个非 0 的 SessionID。
// 如果哪天有人"顺手"把它透传进报文头（看起来更"自然"），这里立刻红。
//
// 依据：Samba smbd_smb2_send_lease_break() 调用
// smbXsrv_pending_break_create(client, 0 /* no session_id */)。
// 租约按 LeaseKey 定位、跨会话共享，绑到某个会话上是错的。
func TestLeaseBreakNotificationSessionIDIsAlwaysZero(t *testing.T) {
	c, cli := newPipeConn(t)

	var key [16]byte
	copy(key[:], []byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})

	ch := readFrameAsync(cli)
	err := c.state.BreakSender().SendLeaseBreak(
		// 故意给一个非 0 的 SessionID —— 它只该进日志，不该进报文。
		command.BreakTarget{SessionID: 0xAABBCCDD, TreeID: 3},
		wire.LeaseBreakNotification{
			NewEpoch:          2,
			Flags:             wire.LeaseBreakAckRequired,
			LeaseKey:          key,
			CurrentLeaseState: wire.LeaseReadCaching | wire.LeaseWriteCaching | wire.LeaseHandleCaching,
			NewLeaseState:     wire.LeaseReadCaching,
		},
	)
	if err != nil {
		t.Fatalf("SendLeaseBreak: %v", err)
	}

	frame := mustRecv(t, ch)
	if len(frame) != wire.HeaderSize+44 {
		t.Fatalf("帧长 = %d，期望 %d（64 头 + 44 体）", len(frame), wire.HeaderSize+44)
	}

	h, err := wire.ParseHeader(frame)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.SessionID != 0 {
		t.Errorf("lease break 的 SessionId = %#x，必须为 0；"+
			"BreakTarget.SessionID 只进日志不进报文", h.SessionID)
	}
	if h.TreeID != 0 {
		t.Errorf("TreeId = %d，期望 0", h.TreeID)
	}
	if h.MessageID != breakNotifyMessageID {
		t.Errorf("MessageId = %#x，期望 0xFFFFFFFFFFFFFFFF", h.MessageID)
	}

	n, err := wire.ParseLeaseBreakNotification(frame)
	if err != nil {
		t.Fatalf("ParseLeaseBreakNotification: %v", err)
	}
	if n.NewEpoch != 2 || n.Flags != wire.LeaseBreakAckRequired || n.LeaseKey != key {
		t.Errorf("通知字段回读不一致：%+v", n)
	}
	if n.CurrentLeaseState != (wire.LeaseReadCaching|wire.LeaseWriteCaching|wire.LeaseHandleCaching) ||
		n.NewLeaseState != wire.LeaseReadCaching {
		t.Errorf("租约状态回读不一致：current=%#x new=%#x",
			uint32(n.CurrentLeaseState), uint32(n.NewLeaseState))
	}
}

// TestSendUnsolicitedBlocksOnWriteMu 证明主动推送确实走 writeMu，
// 而不是绕过它直接写 socket。
//
// 这条是 connection.go:50 那句"当前只有读循环会写"失效之后必须补的证明：
// 只靠读代码断言"我加锁了"是不够的，加错地方、加了别的锁、或者将来有人
// 把锁去掉，代码读起来都一样。这里用**行为**来证：
//
//	持锁 → 推送必须挂住（反向对照）
//	放锁 → 推送必须立刻完成（正向）
//
// 只有正向那半会退化成"什么都没验"，所以两半都要有。
func TestSendUnsolicitedBlocksOnWriteMu(t *testing.T) {
	c, cli := newPipeConn(t)

	// 顺序很关键：**先**把读方挂上，再去抢 writeMu。
	//
	// 反过来写（先锁、后挂读）看着更自然，但那样是个假的反向对照：
	// net.Pipe 是同步无缓冲的，没人读的时候写本来就会阻塞，于是
	// "推送没完成"既可能是被锁挡住、也可能只是没人读，两者分不开 ——
	// 把锁去掉这个用例照样"通过"。实测过，确实测不出来。
	// 先挂读之后，唯一还能挡住推送的就只剩 writeMu 了。
	ch := readFrameAsync(cli)

	c.writeMu.Lock()

	done := make(chan error, 1)
	go func() {
		done <- c.state.BreakSender().SendOplockBreak(
			command.BreakTarget{SessionID: 1},
			wire.OplockBreak{OplockLevel: wire.OplockLevelNone},
		)
	}()

	// 反向对照：锁被别人拿着，推送**不许**完成。
	select {
	case err := <-done:
		c.writeMu.Unlock()
		t.Fatalf("writeMu 被持有时推送仍然完成了（err=%v）——"+
			"说明主动推送绕过了写锁，会与读循环的响应交错", err)
	case <-time.After(200 * time.Millisecond):
	}

	// 正向：放锁后必须真的写出去。
	c.writeMu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("放锁后推送失败：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("放锁后推送仍未完成")
	}
	if got := mustRecv(t, ch); len(got) != wire.HeaderSize+24 {
		t.Fatalf("帧长 = %d，期望 %d", len(got), wire.HeaderSize+24)
	}
}

// TestBreakInterleavesCleanlyWithReadLoop 是真刀真枪的并发对撞：
// 读循环在应答 ECHO 的同时，另外几个 goroutine 不停推送 break 通知。
//
// 检验的是**帧完整性**：客户端侧收到的每一帧都必须是一条结构完好、长度
// 精确的 SMB2 消息。只要有一次交错，长度或 ProtocolId 就对不上。
//
// 请配合 -race 跑：Transport 复用同一份写头缓冲（Transport.whdr），
// 交错时 race detector 会直接指出来。
func TestBreakInterleavesCleanlyWithReadLoop(t *testing.T) {
	c, cli := newPipeConn(t)

	// 给两侧都设写超时。net.Pipe 没人读时写会**永久**阻塞：一旦报文真的
	// 交错、收帧方提前退出，所有写方就会挂死，测试变成超时挂起而不是失败，
	// 排查起来极其难受。设了超时，故障就表现为干净的错误。
	c.tr.SetTimeouts(0, 3*time.Second)
	cli.SetTimeouts(10*time.Second, 3*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 服务端读循环。
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		c.serve(ctx)
	}()

	const (
		echoes  = 40
		breaks  = 40
		senders = 4
	)

	// 客户端侧收帧并校验，直到收够为止。
	type result struct {
		echo, brk, bad int
		err            error
	}
	recvDone := make(chan result, 1)
	go func() {
		var r result
		// 出错也**继续排空**，不提前 return —— 提前退出会让还在写的
		// goroutine 全部挂在 pipe 上，把一次干净的失败变成一次挂起。
		fail := func(err error) {
			r.bad++
			if r.err == nil {
				r.err = err
			}
		}
		for r.echo+r.brk+r.bad < echoes+breaks {
			frame, err := cli.ReadFrame()
			if err != nil {
				if r.err == nil {
					r.err = err
				}
				break
			}
			h, err := wire.ParseHeader(frame)
			if err != nil {
				fail(err)
				continue
			}
			switch h.Command {
			case wire.CommandOplockBreak:
				// break 通知：64 头 + 24 体，一个字节都不能多。
				switch {
				case len(frame) != wire.HeaderSize+24:
					fail(errFrameLen(len(frame), wire.HeaderSize+24, "OPLOCK_BREAK"))
				case h.MessageID != breakNotifyMessageID:
					fail(errBadField("MessageId", h.MessageID))
				default:
					r.brk++
				}
			case wire.CommandEcho:
				// ECHO 响应：64 头 + 4 体。
				if len(frame) != wire.HeaderSize+4 {
					fail(errFrameLen(len(frame), wire.HeaderSize+4, "ECHO"))
				} else {
					r.echo++
				}
			default:
				fail(errBadField("Command", uint64(h.Command)))
			}
		}
		recvDone <- r
	}()

	// 推送方。
	var wg sync.WaitGroup
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for i := s; i < breaks; i += senders {
				if err := c.state.BreakSender().SendOplockBreak(
					command.BreakTarget{SessionID: uint64(i)},
					wire.OplockBreak{OplockLevel: wire.OplockLevelII},
				); err != nil {
					return
				}
			}
		}(s)
	}

	// 请求方：往服务端灌 ECHO。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < echoes; i++ {
			if err := cli.WriteFrame(buildMsg(wire.CommandEcho, uint64(i+1), 0, echoBody())); err != nil {
				return
			}
		}
	}()

	wg.Wait()

	select {
	case r := <-recvDone:
		if r.err != nil {
			t.Fatalf("收帧校验失败（echo=%d break=%d bad=%d）：%v", r.echo, r.brk, r.bad, r.err)
		}
		if r.echo != echoes || r.brk != breaks {
			t.Fatalf("收到 echo=%d break=%d，期望 %d/%d", r.echo, r.brk, echoes, breaks)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("并发收帧超时")
	}

	cancel()
	<-serveDone
}

// TestNewConnectionInjectsBreakSender 防止有人把 newConnection 里那行注入
// 删掉 —— 删了之后所有 break 都会静默变成 nil 接口，编译照过，
// 直到运行时才 panic 或者干脆什么都不发。
func TestNewConnectionInjectsBreakSender(t *testing.T) {
	srvEnd, cliEnd := net.Pipe()
	defer func() { _ = srvEnd.Close(); _ = cliEnd.Close() }()

	s := &Server{
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		opts: Options{Settings: &command.Settings{}},
	}
	c := newConnection(s, srvEnd)
	if c.state.BreakSender() == nil {
		t.Fatal("newConnection 没有注入 BreakSender，协议层将无法推送 break 通知")
	}
}

// TestSendUnsolicitedOnClosedConnection 断言连接关掉后推送返回错误而不是
// panic —— break 是异步发的，"发的时候连接已经没了"是常态而非异常。
func TestSendUnsolicitedOnClosedConnection(t *testing.T) {
	c, _ := newPipeConn(t)
	c.Close()

	err := c.state.BreakSender().SendOplockBreak(
		command.BreakTarget{SessionID: 1},
		wire.OplockBreak{OplockLevel: wire.OplockLevelNone},
	)
	if err == nil {
		t.Fatal("连接已关闭，推送却报告成功")
	}
}

// TestSendUnsolicitedRejectsEmptyFrame 守住"空帧不许上线"这条底线。
func TestSendUnsolicitedRejectsEmptyFrame(t *testing.T) {
	c, _ := newPipeConn(t)
	if err := c.sendUnsolicited(nil); err != ErrEmptyFrame {
		t.Fatalf("空帧返回 %v，期望 ErrEmptyFrame", err)
	}
}

// errFrameLen / errBadField 是并发用例里的报错构造器。
// 收帧 goroutine 不能直接 t.Fatalf（不是测试 goroutine），只能把错误带回去。
func errFrameLen(got, want int, what string) error {
	return fmt.Errorf("%s 帧长 = %d，期望 %d —— 报文交错了", what, got, want)
}

func errBadField(name string, v uint64) error {
	return fmt.Errorf("字段 %s = %#x 非法 —— 报文交错或串台了", name, v)
}
