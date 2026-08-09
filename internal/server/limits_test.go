package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/config"
	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// newTestServer 起一个监听 127.0.0.1 随机端口的真实服务端。
//
// 与 newFullTestConn 不同，这里走完整的 accept → serve 路径，
// 用于验证「资源上限真的会被触发」而不只是「代码里写了一个上限」。
func newTestServer(t *testing.T, tune func(*Options)) *Server {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	store, err := auth.NewStaticStore(config.Auth{
		AllowGuest: true,
		Users:      []config.User{{Name: "alice", Password: "secret"}},
	}, "WORKGROUP")
	if err != nil {
		t.Fatalf("构造账户库失败: %v", err)
	}
	fs, err := vfs.NewLocalFS(vfs.LocalConfig{Root: t.TempDir(), VolumeLabel: "data"})
	if err != nil {
		t.Fatalf("构造 VFS 失败: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	opts := Options{
		Addresses: []string{"127.0.0.1"},
		Port:      0, // 随机端口，避免与其它 agent 抢
		Logger:    log,
		Settings: &command.Settings{
			ServerName:         "TESTSRV",
			Domain:             "WORKGROUP",
			StartTime:          time.Now(),
			MinDialect:         dialect.SMB202,
			MaxDialect:         dialect.SMB311,
			AllowSMB1Negotiate: true,
			AllowGuest:         true,
			Auth: auth.NewNTLMProvider(auth.Options{
				Store: store, ServerName: "TESTSRV", DomainName: "WORKGROUP",
				AllowAnonymous: true,
			}),
			Shares: []*command.Share{
				{Name: "data", Type: wire.ShareTypeDisk, FS: fs, GuestOK: true, Browseable: true},
				{Name: command.IPCShareName, Type: wire.ShareTypePipe, GuestOK: true},
			},
			Logger: log,
		},
	}
	if tune != nil {
		tune(&opts)
	}

	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})
	return srv
}

func serverAddr(t *testing.T, s *Server) string {
	t.Helper()
	addrs := s.Addrs()
	if len(addrs) == 0 {
		t.Fatal("服务端没有绑定任何地址")
	}
	return addrs[0].String()
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接 %s 失败: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// writeFrame 写一个 Direct TCP 帧（4 字节大端长度前缀 + 载荷）。
func writeFrame(t *testing.T, c net.Conn, payload []byte) {
	t.Helper()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := c.Write(hdr[:]); err != nil {
		t.Fatalf("写帧头失败: %v", err)
	}
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("写载荷失败: %v", err)
	}
}

// readFrame 读一个 Direct TCP 帧。
func readFrame(t *testing.T, c net.Conn, timeout time.Duration) ([]byte, error) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:]) & 0x00FFFFFF
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// realNegotiate 返回一条真实抓包的 SMB2 NEGOTIATE 请求。
func realNegotiate(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(captureRoot, "negotiate-smb311", "001-c2s-NEGOTIATE.bin"))
	if err != nil {
		t.Fatalf("读取 NEGOTIATE 抓包失败: %v", err)
	}
	return b
}

// mustNegotiate 完成一次协商，确认服务端仍在正常工作。
func mustNegotiate(t *testing.T, addr string) {
	t.Helper()
	c := dial(t, addr)
	writeFrame(t, c, realNegotiate(t))
	resp, err := readFrame(t, c, 3*time.Second)
	if err != nil {
		t.Fatalf("读取 NEGOTIATE 响应失败: %v", err)
	}
	h, err := wire.ParseHeader(resp)
	if err != nil {
		t.Fatalf("解析 NEGOTIATE 响应头失败: %v", err)
	}
	if h.Command != wire.CommandNegotiate {
		t.Fatalf("响应命令 = %s，期望 NEGOTIATE", h.Command)
	}
}

// waitConnCount 等待活跃连接数达到 want，超时则失败。
func waitConnCount(t *testing.T, s *Server, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s.ConnectionCount() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待连接数 = %d 超时，实际 %d", want, s.ConnectionCount())
}

// ---------------------------------------------------------------- 上限

// TestMaxConnectionsEnforced：并发连接上限必须**真的**生效
// （AGENTS.md §8）。超限的连接要被立刻关闭，而不是排队等着。
func TestMaxConnectionsEnforced(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.MaxConnections = 2 })
	addr := serverAddr(t, srv)

	// 占满 2 个槽位。
	for range 2 {
		c := dial(t, addr)
		writeFrame(t, c, realNegotiate(t))
		if _, err := readFrame(t, c, 3*time.Second); err != nil {
			t.Fatalf("占位连接协商失败: %v", err)
		}
	}
	waitConnCount(t, srv, 2, 2*time.Second)

	// 第三条：TCP 层能连上（内核 backlog），但服务端必须立刻关掉它。
	third, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("第三条连接建链失败: %v", err)
	}
	defer third.Close()

	_ = third.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := third.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("超出并发上限的连接应当被立刻关闭（期望 EOF），实际 err=%v", err)
	}
	if got := srv.ConnectionCount(); got != 2 {
		t.Fatalf("超限连接不应被登记：连接数 = %d，期望 2", got)
	}
}

// TestRejectLogIsThrottled：超限拒绝必须**可见但节流**。
//
// 静默拒绝会让运维完全查不出"为什么连不上"；而一条连接一行日志，
// 连接洪水就能顺带把磁盘写满，等于把一次拒绝服务放大成第二次。
func TestRejectLogIsThrottled(t *testing.T) {
	var buf syncBuffer
	srv := newTestServer(t, func(o *Options) {
		o.MaxConnections = 1
		o.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	addr := serverAddr(t, srv)

	dial(t, addr) // 占满唯一的槽位
	waitConnCount(t, srv, 1, 2*time.Second)

	const flood = 20
	for range flood {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("建链失败: %v", err)
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _ = c.Read(make([]byte, 1)) // 等服务端把它关掉
		_ = c.Close()
	}

	logged := strings.Count(buf.String(), "并发连接数已达上限")
	if logged == 0 {
		t.Fatalf("超限拒绝必须留下日志，实际日志:\n%s", buf.String())
	}
	if logged > 1 {
		t.Fatalf("%d 次拒绝写了 %d 行日志，节流没生效:\n%s", flood, logged, buf.String())
	}
	// 那唯一一行必须带上被拒总数，否则节流会把信息量也一起丢掉。
	if !strings.Contains(buf.String(), "rejected=") {
		t.Fatalf("拒绝日志应当带上区间内的被拒次数:\n%s", buf.String())
	}
}

// syncBuffer 是并发安全的 bytes.Buffer（accept goroutine 与测试 goroutine 同时访问）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestOversizedFrameDropsOnlyThatConnection：声明超过单帧上限的长度前缀
// 必须只断这一条连接，服务端本身要活着（AGENTS.md §8 单帧大小上限）。
func TestOversizedFrameDropsOnlyThatConnection(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.MaxFrameSize = 4096 })
	addr := serverAddr(t, srv)

	c := dial(t, addr)
	// 声明 0xFFFFFF（16 MiB - 1）字节，远超 4096 上限，但一个字节的
	// 载荷都不发 —— 服务端绝不能先分配这块内存再去读。
	if _, err := c.Write([]byte{0x00, 0xFF, 0xFF, 0xFF}); err != nil {
		t.Fatalf("写超长帧头失败: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("超长帧应当导致断连（期望 EOF），实际 err=%v", err)
	}

	// 服务端还活着。
	mustNegotiate(t, addr)
}

// TestMalformedFrameDropsOnlyThatConnection：畸形帧只断当前连接，
// 不影响别的客户端（panic 逃逸会让整个进程死掉，那才是发布阻断项）。
func TestMalformedFrameDropsOnlyThatConnection(t *testing.T) {
	srv := newTestServer(t, nil)
	addr := serverAddr(t, srv)

	// 一条正常连接，全程保持。
	good := dial(t, addr)
	writeFrame(t, good, realNegotiate(t))
	if _, err := readFrame(t, good, 3*time.Second); err != nil {
		t.Fatalf("正常连接协商失败: %v", err)
	}

	// 一堆畸形帧，每条一个新连接。
	bad := [][]byte{
		{0x00},                                     // 1 字节，连 SMB 魔数都不完整
		{0xFE, 'S', 'M', 'B'},                      // 只有魔数，没有头
		{0xFD, 'S', 'M', 'B', 0x01, 0x02},          // TRANSFORM 魔数 + 垃圾
		{0xFF, 'S', 'M', 'B', 0x72, 0x00, 0x00},    // SMB1 魔数 + 截断
		append(realNegotiate(t)[:20], 0xFF, 0xFF, 0xFF, 0xFF), // NextCommand 撒谎
	}
	for i, b := range bad {
		c := dial(t, addr)
		writeFrame(t, c, b)
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		// 断连或回一条错误响应都可以接受，唯独不能是超时（挂死）。
		if _, err := c.Read(make([]byte, 512)); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("畸形帧 #%d 让连接挂死（读超时）", i)
			}
		}
	}

	// 正常连接不受影响，服务端也还能接新连接。
	writeFrame(t, good, realNegotiate(t))
	if _, err := readFrame(t, good, 3*time.Second); err != nil {
		t.Fatalf("畸形帧影响了无辜的连接: %v", err)
	}
	mustNegotiate(t, addr)
}

// TestIdleTimeoutClosesConnection：读超时必须真的挂在 socket 上。
func TestIdleTimeoutClosesConnection(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.IdleTimeout = 150 * time.Millisecond })
	addr := serverAddr(t, srv)

	c := dial(t, addr)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("空闲连接应当被超时断开（期望 EOF），实际 err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("断开耗时 %v，超时没有生效", elapsed)
	}
	waitConnCount(t, srv, 0, 2*time.Second)
}

// TestSlowlorisCannotHoldAllSlots 覆盖一个真实的拒绝服务向量：
//
// 未认证的连接如果能一直占着并发槽位直到 IdleTimeout（默认 15 分钟），
// 任何人都能用 `for i in $(seq 256); do nc host 445 & done` 把服务打没。
// 因此**认证完成之前**必须有一个独立的、短得多的握手超时。
//
// 这里把 HandshakeTimeout 设成 150ms、并发上限设成 2，
// 然后用 2 条一言不发的连接占满槽位：握手超时到期后槽位必须自动释放，
// 正常客户端要能重新连上。
func TestSlowlorisCannotHoldAllSlots(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.MaxConnections = 2
		o.HandshakeTimeout = 150 * time.Millisecond
		// 空闲超时故意设得很长：本测试要证明的是「未认证连接不吃这个长超时」。
		o.IdleTimeout = time.Hour
	})
	addr := serverAddr(t, srv)

	for range 2 {
		dial(t, addr) // 建链后一个字节都不发
	}
	waitConnCount(t, srv, 2, 2*time.Second)

	// 握手超时到期后槽位应当被释放。
	waitConnCount(t, srv, 0, 3*time.Second)

	// 正常客户端能连上并完成协商。
	mustNegotiate(t, addr)
}

// TestSlowlorisPartialFrameCannotHoldSlot：只发半个帧头（经典 slowloris）
// 同样必须在握手超时内被清掉。
func TestSlowlorisPartialFrameCannotHoldSlot(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.HandshakeTimeout = 150 * time.Millisecond
		o.IdleTimeout = time.Hour
	})
	addr := serverAddr(t, srv)

	c := dial(t, addr)
	// 声明一个 1024 字节的帧，只发 2 字节头就装死。
	if _, err := c.Write([]byte{0x00, 0x00}); err != nil {
		t.Fatalf("写半个帧头失败: %v", err)
	}
	waitConnCount(t, srv, 1, 2*time.Second)
	waitConnCount(t, srv, 0, 3*time.Second)
}

// TestEstablishedSessionKeepsLongIdleTimeout：握手超时**只**作用于认证之前。
// 已认证会话（macOS Finder 挂载后可能长时间不发请求）必须继续享受长空闲超时，
// 否则修 slowloris 会把正常客户端一起踢掉。
func TestEstablishedSessionKeepsLongIdleTimeout(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.HandshakeTimeout = 150 * time.Millisecond
		o.IdleTimeout = 5 * time.Second
	})
	addr := serverAddr(t, srv)

	c := dial(t, addr)
	writeFrame(t, c, realNegotiate(t))
	if _, err := readFrame(t, c, 3*time.Second); err != nil {
		t.Fatalf("协商失败: %v", err)
	}
	// 伪造一个已认证会话：真正的 NTLM 握手由 auth 包的测试覆盖，
	// 这里只关心「连接是否还在被握手超时盯着」。
	establishSessionOn(t, onlyConnection(t, srv))
	// 再发一帧，逼读循环走完一轮 —— 期限是在每次 ReadFrame 入口重新装载的，
	// 只有走过一轮读循环才会观察到"已认证"并把握手死线摘掉。
	// 这也正是真实路径的样子（SESSION_SETUP 响应写出后回到 ReadFrame）。
	writeFrame(t, c, realNegotiate(t))
	if _, err := readFrame(t, c, 3*time.Second); err != nil {
		t.Fatalf("认证后再次交互失败: %v", err)
	}

	// 远超握手超时，但远小于空闲超时 —— 连接必须还活着。
	time.Sleep(600 * time.Millisecond)
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("已认证连接被握手超时误杀：连接数 = %d", got)
	}
	writeFrame(t, c, realNegotiate(t))
	if _, err := readFrame(t, c, 3*time.Second); err != nil {
		t.Fatalf("已认证连接被提前断开: %v", err)
	}
}

// onlyConnection 返回服务端当前唯一的连接。
func onlyConnection(t *testing.T, s *Server) *Connection {
	t.Helper()
	waitConnCount(t, s, 1, 2*time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		return c
	}
	t.Fatal("没有活跃连接")
	return nil
}

// establishSessionOn 在给定连接上直接造一个已认证会话。
func establishSessionOn(t *testing.T, c *Connection) {
	t.Helper()
	sess, st := c.state.NewSession()
	if st != 0 {
		t.Fatalf("NewSession 失败: %s", st)
	}
	sess.Establish(&auth.Identity{User: "alice"})
}
