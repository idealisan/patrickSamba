package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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

// waitFor 轮询等待一个**确定性条件**成立，只有超时才失败。
//
// 这类等待只是等「事件已在别的 goroutine 发生」跨线程可见，
// 断言依据是条件里的计数器/状态值，不是任何耗时测量；
// 预算按「正常耗时 × 数十倍」取值，共享 runner 负载抖动下依然稳定。
//
// 本文件的所有用例都依赖进程全局计数器做 before/after 差值，
// 因此**禁止 t.Parallel**（同 vfs 包 pathFullScans 先例）。
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时（预算 %v）", what, within)
}

// waitConnCount 等待活跃连接数达到 want，超时则失败。
func waitConnCount(t *testing.T, s *Server, want int, within time.Duration) {
	t.Helper()
	waitFor(t, within,
		fmt.Sprintf("活跃连接数 = %d（实际 %d）", want, s.ConnectionCount()),
		func() bool { return s.ConnectionCount() == want })
}

// ---------------------------------------------------------------- 上限

// TestMaxConnectionsEnforced：并发连接上限必须**真的**生效
// （AGENTS.md §8）。超限的连接要被立刻关闭，而不是排队等着。
//
// 判据是 connRejected 计数（确定性事件），不是任何耗时测量。
func TestMaxConnectionsEnforced(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.MaxConnections = 2 })
	addr := serverAddr(t, srv)

	rejectedBase := connRejected.Load()

	// 占满 2 个槽位。
	for range 2 {
		c := dial(t, addr)
		writeFrame(t, c, realNegotiate(t))
		if _, err := readFrame(t, c, 3*time.Second); err != nil {
			t.Fatalf("占位连接协商失败: %v", err)
		}
	}
	waitConnCount(t, srv, 2, 5*time.Second)

	// 第三条：TCP 层能连上（内核 backlog），但服务端必须立刻关掉它。
	third, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("第三条连接建链失败: %v", err)
	}
	defer third.Close()

	_ = third.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 1)
	if _, err := third.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("超出并发上限的连接应当被立刻关闭（期望 EOF），实际 err=%v", err)
	}

	// 确定性断言 + 反向对照：第 N+1 个并发连接被拒
	// ⇒ connRejected 恰好发生一次（计数 >0，防测试空转恒绿）。
	waitFor(t, 5*time.Second, "超限拒绝计数 +1", func() bool {
		return connRejected.Load()-rejectedBase == 1
	})
	if got := srv.ConnectionCount(); got != 2 {
		t.Fatalf("超限连接不应被登记：连接数 = %d，期望 2", got)
	}
}

// TestRejectLogIsThrottled：超限拒绝必须**可见但节流**。
//
// 静默拒绝会让运维完全查不出"为什么连不上"；而一条连接一行日志，
// 连接洪水就能顺带把磁盘写满，等于把一次拒绝服务放大成第二次。
//
// 判据是 connRejected / rejectLogSuppressed 计数（确定性事件）；
// 日志行数断言保留（节流行为本身），其稳定性依据见下方注释。
func TestRejectLogIsThrottled(t *testing.T) {
	var buf syncBuffer
	srv := newTestServer(t, func(o *Options) {
		o.MaxConnections = 1
		o.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	})
	addr := serverAddr(t, srv)

	rejectedBase := connRejected.Load()
	suppressedBase := rejectLogSuppressed.Load()

	dial(t, addr) // 占满唯一的槽位
	waitConnCount(t, srv, 1, 5*time.Second)

	const flood = 20
	floodStart := time.Now()
	for range flood {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("建链失败: %v", err)
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _ = c.Read(make([]byte, 1)) // 等服务端把它关掉
		_ = c.Close()
	}
	floodElapsed := time.Since(floodStart)

	// 确定性断言：20 条超限连接必须每一条都被拒（计数恰好 +flood），
	// 且每一条未写日志的拒绝都必须走了节流抑制分支。
	if got := connRejected.Load() - rejectedBase; got != flood {
		t.Fatalf("被拒连接计数 = %d，期望 %d", got, flood)
	}
	logged := strings.Count(buf.String(), "并发连接数已达上限")
	if got := rejectLogSuppressed.Load() - suppressedBase; got != int64(flood-logged) {
		t.Fatalf("抑制计数 = %d，与 拒绝总数 %d - 日志行数 %d 不一致", got, flood, logged)
	}
	// 反向对照：拒绝确实发生了，且至少留下一行日志（防测试空转恒绿）。
	if logged == 0 {
		t.Fatalf("超限拒绝必须留下日志，实际日志:\n%s", buf.String())
	}
	// 节流窗口是 rejectLogInterval（10s），洪水全程通常 <0.1s、有百倍余量，
	// 因此「恰好一行」在负载抖动下依然稳定；若这里偶发 >1，先看
	// t.Logf 输出的洪水耗时是否已逼近窗口再下结论。
	if logged > 1 {
		t.Fatalf("%d 次拒绝写了 %d 行日志，节流没生效:\n%s", flood, logged, buf.String())
	}
	// 那唯一一行必须带上被拒总数，否则节流会把信息量也一起丢掉。
	if !strings.Contains(buf.String(), "rejected=") {
		t.Fatalf("拒绝日志应当带上区间内的被拒次数:\n%s", buf.String())
	}
	// 诊断用，勿改回计时门禁：洪水耗时属墙钟测量，共享 runner 一抖就假红，
	// 判据已换成上面的 connRejected / rejectLogSuppressed 计数。
	t.Logf("诊断用，勿改回计时门禁：%d 次拒绝耗时 %v（节流窗口 %v）",
		flood, floodElapsed, rejectLogInterval)
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
//
// 判据是 frameTooLargeDrops 计数（确定性事件），不是任何耗时测量。
func TestOversizedFrameDropsOnlyThatConnection(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.MaxFrameSize = 4096 })
	addr := serverAddr(t, srv)

	dropsBase := frameTooLargeDrops.Load()

	c := dial(t, addr)
	// 声明 0xFFFFFF（16 MiB - 1）字节，远超 4096 上限，但一个字节的
	// 载荷都不发 —— 服务端绝不能先分配这块内存再去读。
	if _, err := c.Write([]byte{0x00, 0xFF, 0xFF, 0xFF}); err != nil {
		t.Fatalf("写超长帧头失败: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("超长帧应当导致断连（期望 EOF），实际 err=%v", err)
	}

	// 确定性断言 + 反向对照：慢输入必须真的触发「超长帧丢弃」计数 >0，
	// 否则下面的「服务端还活着」检查会空转恒绿。
	waitFor(t, 5*time.Second, "超长帧丢弃计数 +1", func() bool {
		return frameTooLargeDrops.Load()-dropsBase == 1
	})

	// 服务端还活着。
	mustNegotiate(t, addr)
}

// TestMalformedFrameDropsOnlyThatConnection：畸形帧只断当前连接，
// 不影响别的客户端（panic 逃逸会让整个进程死掉，那才是发布阻断项）。
//
// 判据是 malformedFrameDrops 计数（确定性事件）：5 个畸形帧必须
// 每一个都触发致命协议错误断连，而不是靠客户端读超时去猜。
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
		{0x00},                                  // 1 字节，连 SMB 魔数都不完整
		{0xFE, 'S', 'M', 'B'},                   // 只有魔数，没有头
		{0xFD, 'S', 'M', 'B', 0x01, 0x02},       // TRANSFORM 魔数 + 垃圾
		{0xFF, 'S', 'M', 'B', 0x72, 0x00, 0x00}, // SMB1 魔数 + 截断
		append(realNegotiate(t)[:20], 0xFF, 0xFF, 0xFF, 0xFF), // NextCommand 撒谎
	}
	badBase := malformedFrameDrops.Load()
	for i, b := range bad {
		c := dial(t, addr)
		writeFrame(t, c, b)
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		// 断连或回一条错误响应都可以接受，唯独不能是挂死；
		// 「确实断连」由下面的计数断言兜底，这里只排挂死。
		if _, err := c.Read(make([]byte, 512)); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("畸形帧 #%d 让连接挂死（读超时）", i)
			}
		}
	}
	// 确定性断言 + 反向对照：5 个畸形帧必须恰好触发 5 次协议错误断连。
	waitFor(t, 10*time.Second, "畸形帧断连计数 +5", func() bool {
		return malformedFrameDrops.Load()-badBase == int64(len(bad))
	})

	// 正常连接不受影响，服务端也还能接新连接。
	writeFrame(t, good, realNegotiate(t))
	if _, err := readFrame(t, good, 3*time.Second); err != nil {
		t.Fatalf("畸形帧影响了无辜的连接: %v", err)
	}
	mustNegotiate(t, addr)
}

// TestIdleTimeoutClosesConnection：读超时必须真的挂在 socket 上。
//
// 判据是 idleTimeoutCloses 计数（确定性事件），并反证它没有被
// 误分类成握手超时；原「断开耗时 ≤ 2s」的计时门禁已降级为 t.Logf。
func TestIdleTimeoutClosesConnection(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.IdleTimeout = 150 * time.Millisecond })
	addr := serverAddr(t, srv)

	idleBase := idleTimeoutCloses.Load()
	hsBase := handshakeTimeoutCloses.Load()

	c := dial(t, addr)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("空闲连接应当被超时断开（期望 EOF），实际 err=%v", err)
	}
	elapsed := time.Since(start)

	// 确定性断言 + 反向对照：空闲期限到期断连恰好发生一次（计数 >0）。
	waitFor(t, 5*time.Second, "空闲超时断连计数 +1", func() bool {
		return idleTimeoutCloses.Load()-idleBase == 1
	})
	// 本连接未认证，若期限来源分类有错会记到握手头上 —— 用 0 断言钉死。
	if got := handshakeTimeoutCloses.Load() - hsBase; got != 0 {
		t.Fatalf("空闲断连被误分类为握手超时 %d 次", got)
	}
	waitConnCount(t, srv, 0, 5*time.Second)

	// 诊断用，勿改回计时门禁：断开耗时属墙钟测量，共享 runner 一抖就假红，
	// 判据已换成上面的 idleTimeoutCloses 计数。
	t.Logf("诊断用，勿改回计时门禁：空闲 150ms 断开耗时 %v", elapsed)
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
//
// 判据是 handshakeTimeoutCloses 计数（确定性事件）+ 连接数归零，
// 并用 idleTimeoutCloses == 0 反证未认证连接没有吃空闲超时。
func TestSlowlorisCannotHoldAllSlots(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.MaxConnections = 2
		o.HandshakeTimeout = 150 * time.Millisecond
		// 空闲超时故意设得很长：本测试要证明的是「未认证连接不吃这个长超时」。
		o.IdleTimeout = time.Hour
	})
	addr := serverAddr(t, srv)

	hsBase := handshakeTimeoutCloses.Load()
	idleBase := idleTimeoutCloses.Load()

	for range 2 {
		dial(t, addr) // 建链后一个字节都不发
	}
	waitConnCount(t, srv, 2, 5*time.Second)

	// 确定性断言 + 反向对照：两条一言不发的连接都必须由**握手绝对期限**清掉
	//（计数 >0，且恰好 +2 —— 不是被别的路径断开的）。
	waitFor(t, 5*time.Second, "握手超时断连计数 +2", func() bool {
		return handshakeTimeoutCloses.Load()-hsBase == 2
	})
	// 计数只证「事件发生过」，这里补证槽位状态真的被回收了。
	waitConnCount(t, srv, 0, 5*time.Second)
	if got := idleTimeoutCloses.Load() - idleBase; got != 0 {
		t.Fatalf("未认证连接不应由空闲超时清掉，误分类 %d 次", got)
	}

	// 正常客户端能连上并完成协商。
	mustNegotiate(t, addr)
}

// TestSlowlorisPartialFrameCannotHoldSlot：只发半个帧头（经典 slowloris）
// 同样必须在握手超时内被清掉。判据同上，换成 handshakeTimeoutCloses 计数。
func TestSlowlorisPartialFrameCannotHoldSlot(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.HandshakeTimeout = 150 * time.Millisecond
		o.IdleTimeout = time.Hour
	})
	addr := serverAddr(t, srv)

	hsBase := handshakeTimeoutCloses.Load()
	idleBase := idleTimeoutCloses.Load()

	c := dial(t, addr)
	// 声明一个 1024 字节的帧，只发 2 字节头就装死。
	if _, err := c.Write([]byte{0x00, 0x00}); err != nil {
		t.Fatalf("写半个帧头失败: %v", err)
	}
	waitConnCount(t, srv, 1, 5*time.Second)
	// 确定性断言 + 反向对照：半帧头连接必须由握手绝对期限清掉（计数 >0）。
	waitFor(t, 5*time.Second, "握手超时断连计数 +1", func() bool {
		return handshakeTimeoutCloses.Load()-hsBase == 1
	})
	waitConnCount(t, srv, 0, 5*time.Second)
	if got := idleTimeoutCloses.Load() - idleBase; got != 0 {
		t.Fatalf("未认证连接不应由空闲超时清掉，误分类 %d 次", got)
	}
}

// TestEstablishedSessionKeepsLongIdleTimeout：握手超时**只**作用于认证之前。
// 已认证会话（macOS Finder 挂载后可能长时间不发请求）必须继续享受长空闲超时，
// 否则修 slowloris 会把正常客户端一起踢掉。
//
// HandshakeTimeout 从旧写法的 150ms 放宽到 1s，理由：握手硬期限在 accept
// 时武装，而「已认证」要等下一帧被读过才被观察到位；高负载下第二轮
// ReadFrame 入口可能已越过旧期限导致好连接被误杀（正是本用例的假红源）。
// 1s 给两轮本地回环往返留足负载余量，仍远小于生产默认值 30s。
func TestEstablishedSessionKeepsLongIdleTimeout(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.HandshakeTimeout = time.Second
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

	// 基线必须在此刻快照：读循环此刻阻塞在 ReadFrame 上，
	// 不可能已经观察到会话；而下一帧的「期限解除」计数发生在
	// 写响应之前 —— 等客户端读到响应再快照就把它算进基线了。
	clearsBase := handshakeDeadlineClears.Load()
	hsBase := handshakeTimeoutCloses.Load()
	idleBase := idleTimeoutCloses.Load()

	// 再发一帧，逼读循环走完一轮 —— 期限是在每次 ReadFrame 入口重新装载的，
	// 只有走过一轮读循环才会观察到"已认证"并把握手死线摘掉。
	// 这也正是真实路径的样子（SESSION_SETUP 响应写出后回到 ReadFrame）。
	writeFrame(t, c, realNegotiate(t))
	if _, err := readFrame(t, c, 3*time.Second); err != nil {
		t.Fatalf("认证后再次交互失败: %v", err)
	}

	// 确定性观察点：读循环必须已经走过一轮、真的摘掉了握手死线
	// （handshakeDeadlineClears 恰好 +1）。旧写法直接开始睡，
	// 观察是否发生全凭墙钟 —— 那是竞态来源。
	waitFor(t, 5*time.Second, "握手期限解除计数 +1", func() bool {
		return handshakeDeadlineClears.Load()-clearsBase == 1
	})

	// 远超握手超时（1s），远小于空闲超时（5s）—— 连接必须还活着。
	//
	// 这里必须保留墙钟等待（AGENTS.md §3 允许的例外）：要证的是
	// 「定时器没有在 T_accept+HT 触发」，负时间性质无法只用事件计数表达，
	// 只能等到过期时刻之后再检查。断言本身已被计数器钉死：
	// 下面三个 delta==0 分别证明握手期限、空闲期限都没触发过误杀断连，
	// 墙钟只负责把「过期时刻已过去」这件事坐实。
	time.Sleep(1500 * time.Millisecond)
	if got := srv.ConnectionCount(); got != 1 {
		t.Fatalf("已认证连接被握手超时误杀：连接数 = %d", got)
	}
	if got := handshakeTimeoutCloses.Load() - hsBase; got != 0 {
		t.Fatalf("已认证连接触发握手超时 %d 次（期限应已解除）", got)
	}
	if got := idleTimeoutCloses.Load() - idleBase; got != 0 {
		t.Fatalf("已认证连接在远小于空闲超时的窗口内触发空闲超时 %d 次", got)
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
