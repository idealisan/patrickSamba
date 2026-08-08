package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/command"
)

// 默认的服务端参数。
const (
	// DefaultPort 是 SMB Direct TCP 端口。
	DefaultPort = 445

	// DefaultMaxConnections 是并发连接数上限（AGENTS.md §8 资源限制）。
	DefaultMaxConnections = 256

	// DefaultIdleTimeout 是连接空闲多久没有收到任何帧就断开。
	//
	// 取值偏大：macOS Finder 挂载后可能长时间不发请求，
	// 但 SMB2 客户端通常有 keepalive（ECHO）。
	DefaultIdleTimeout = 15 * time.Minute

	// DefaultWriteTimeout 是单次写出的超时，用于淘汰慢客户端。
	DefaultWriteTimeout = 60 * time.Second

	// shutdownPollInterval 是优雅关闭时轮询连接数的间隔。
	shutdownPollInterval = 20 * time.Millisecond
)

// ErrServerClosed 在 Serve 因 Shutdown/Close 正常返回时给出。
var ErrServerClosed = errors.New("smb server: closed")

// Options 是 Server 的装配参数。
type Options struct {
	// Addresses 是要监听的 IP 列表。留空表示监听所有地址。
	//
	// 需求约定（AGENTS.md §6）：地址是**列表**，端口只有**一个**。
	Addresses []string
	// Port 是监听端口，0 表示 DefaultPort。
	Port int

	// Settings 是 SMB 协议层设置（共享表、认证后端、方言区间……）。
	Settings *command.Settings

	// MaxConnections 是并发连接上限，0 表示 DefaultMaxConnections。
	MaxConnections int
	// IdleTimeout / WriteTimeout 为 0 时使用默认值，负数表示不设超时。
	IdleTimeout  time.Duration
	WriteTimeout time.Duration
	// MaxFrameSize 是单帧上限，0 表示 DefaultMaxFrameSize。
	MaxFrameSize int

	// Logger 为 nil 时使用 slog.Default()。
	Logger *slog.Logger
}

func (o *Options) port() int {
	if o.Port <= 0 {
		return DefaultPort
	}
	return o.Port
}

func (o *Options) maxConnections() int {
	if o.MaxConnections <= 0 {
		return DefaultMaxConnections
	}
	return o.MaxConnections
}

func (o *Options) idleTimeout() time.Duration {
	switch {
	case o.IdleTimeout < 0:
		return 0
	case o.IdleTimeout == 0:
		return DefaultIdleTimeout
	default:
		return o.IdleTimeout
	}
}

func (o *Options) writeTimeout() time.Duration {
	switch {
	case o.WriteTimeout < 0:
		return 0
	case o.WriteTimeout == 0:
		return DefaultWriteTimeout
	default:
		return o.WriteTimeout
	}
}

// Server 监听若干地址并为每条连接跑一个 Connection。
type Server struct {
	opts Options
	log  *slog.Logger

	mu        sync.Mutex
	listeners []net.Listener
	conns     map[*Connection]struct{}
	closed    bool
	doneCh    chan struct{}

	// wg 覆盖 accept goroutine 与每条连接的 serve goroutine。
	wg sync.WaitGroup
}

// New 创建服务端。此时还没有绑定端口。
func New(opts Options) (*Server, error) {
	if opts.Settings == nil {
		return nil, errors.New("smb server: Options.Settings 不能为空")
	}
	if opts.Settings.Auth == nil {
		return nil, errors.New("smb server: Settings.Auth 不能为空")
	}
	if len(opts.Settings.Shares) == 0 {
		return nil, errors.New("smb server: 至少要配置一个共享")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		opts:   opts,
		log:    log,
		conns:  make(map[*Connection]struct{}),
		doneCh: make(chan struct{}),
	}, nil
}

// Listen 绑定 Options 里配置的全部地址。
//
// 任何一个地址绑定失败都会回滚已经绑定的监听套接字并返回错误 ——
// 部分成功会让运维困惑（"服务起来了但某个网卡上连不上"）。
func (s *Server) Listen() error {
	port := strconv.Itoa(s.opts.port())

	addrs := s.opts.Addresses
	if len(addrs) == 0 {
		// 空列表 = 所有地址。Go 的 "tcp" + 空 host 会同时覆盖 IPv4/IPv6
		// （双栈套接字），无需分别监听 0.0.0.0 与 ::。
		addrs = []string{""}
	}

	var ls []net.Listener
	for _, a := range addrs {
		l, err := net.Listen("tcp", net.JoinHostPort(a, port))
		if err != nil {
			for _, prev := range ls {
				_ = prev.Close()
			}
			where := a
			if where == "" {
				where = "所有地址"
			}
			return fmt.Errorf("smb server: 监听 %s:%s 失败: %w", where, port, err)
		}
		ls = append(ls, l)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		for _, l := range ls {
			_ = l.Close()
		}
		return ErrServerClosed
	}
	s.listeners = append(s.listeners, ls...)
	return nil
}

// Addrs 返回已绑定的本地地址，便于测试里取随机端口。
func (s *Server) Addrs() []net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]net.Addr, 0, len(s.listeners))
	for _, l := range s.listeners {
		out = append(out, l.Addr())
	}
	return out
}

// Serve 在已绑定的监听套接字上接受连接，直到 ctx 取消或 Shutdown 被调用。
//
// 返回 ErrServerClosed 表示正常停止。
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrServerClosed
	}
	if len(s.listeners) == 0 {
		s.mu.Unlock()
		return errors.New("smb server: 尚未调用 Listen")
	}
	listeners := append([]net.Listener(nil), s.listeners...)
	s.mu.Unlock()

	// ctx 取消时主动关闭监听套接字，让 Accept 立刻返回。
	stop := context.AfterFunc(ctx, func() { s.closeListeners() })
	defer stop()

	for _, l := range listeners {
		s.wg.Add(1)
		go func(l net.Listener) {
			defer s.wg.Done()
			s.acceptLoop(ctx, l)
		}(l)
	}

	<-s.doneCh
	return ErrServerClosed
}

// ListenAndServe 是 Listen + Serve 的组合。
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
}

func (s *Server) acceptLoop(ctx context.Context, l net.Listener) {
	// 临时错误（EMFILE、ECONNABORTED 等）退避重试，不要让 accept 循环退出。
	var backoff time.Duration

	for {
		conn, err := l.Accept()
		if err != nil {
			if s.isClosed() || ctx.Err() != nil {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// 未知错误：退避后重试，避免 CPU 空转。
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff < time.Second {
				backoff *= 2
			}
			s.log.Warn("accept 失败，稍后重试", "listener", l.Addr().String(), "err", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-s.doneCh:
				return
			case <-ctx.Done():
				return
			}
			continue
		}
		backoff = 0

		if !s.trackConn(ctx, conn) {
			// 超出并发上限：直接断开。SMB 没有"服务器忙"的传输层表达，
			// 关闭连接是唯一选择。
			s.log.Warn("并发连接数已达上限，拒绝新连接",
				"remote", conn.RemoteAddr().String(), "max", s.opts.maxConnections())
			_ = conn.Close()
		}
	}
}

// trackConn 登记连接并启动其服务 goroutine。超出上限时返回 false。
func (s *Server) trackConn(ctx context.Context, nc net.Conn) bool {
	c := newConnection(s, nc)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	if len(s.conns) >= s.opts.maxConnections() {
		s.mu.Unlock()
		return false
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.untrackConn(c)
		c.serve(ctx)
	}()
	return true
}

func (s *Server) untrackConn(c *Connection) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// ConnectionCount 返回当前活跃连接数，用于日志与测试。
func (s *Server) ConnectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// closeListeners 关闭全部监听套接字并唤醒 Serve。
func (s *Server) closeListeners() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ls := s.listeners
	s.listeners = nil
	close(s.doneCh)
	s.mu.Unlock()

	for _, l := range ls {
		_ = l.Close()
	}
}

// Shutdown 优雅关闭：先停止接受新连接，再等待既有连接自然结束。
//
// ctx 超时后强制断开所有仍在的连接。
func (s *Server) Shutdown(ctx context.Context) error {
	s.closeListeners()

	// 轮询等待连接自然退出。
	ticker := time.NewTicker(shutdownPollInterval)
	defer ticker.Stop()
	for {
		if s.ConnectionCount() == 0 {
			s.wg.Wait()
			return nil
		}
		select {
		case <-ctx.Done():
			s.Close()
			s.wg.Wait()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Close 立刻关闭监听与全部连接，不等待请求处理完成。
func (s *Server) Close() {
	s.closeListeners()

	s.mu.Lock()
	conns := make([]*Connection, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		c.Close()
	}
}
