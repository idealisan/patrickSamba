//go:build integration

// Package integration 是 stupidSamba 的端到端集成测试（AGENTS.md §3）。
//
// 与 scripts/acceptance.sh 的分工：
//   - acceptance.sh 用**第三方客户端**（smbclient / impacket / go-smb2）验证
//     互操作性，覆盖真实客户端会走的路径；
//   - 本包用**自研裸客户端**（rawclient_test.go）精确构造第三方客户端根本
//     发不出来的报文：篡改过的签名、裸 CANCEL、手工拼的复合链……
//
// 两者互补，缺一不可：第三方客户端能发现「我们理解错了规范」，
// 裸客户端能发现「规范里的边界分支没实现对」。
//
// 跑法（build tag 隔离，默认 go test ./... 不会执行）：
//
//	CGO_ENABLED=0 go test -tags integration ./test/integration/ -v
package integration

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/config"
	"github.com/finalappstore/stupidsamba/internal/server"
	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 测试账户。口令只存在于本文件，不读任何系统用户数据库（AGENTS.md C8）。
const (
	testUser   = "testuser"
	testPass   = "testpass123"
	testDomain = "WORKGROUP"
	testShare  = "public"
)

// harnessOptions 控制被测服务端的装配方式。零值即「最宽松」的默认配置。
type harnessOptions struct {
	// MinDialect / MaxDialect 为 0 时用 2.0.2 ~ 3.1.1 全区间。
	MinDialect dialect.Dialect
	MaxDialect dialect.Dialect

	SigningRequired    bool
	EncryptionRequired bool
	// EncryptionDisabled 强制服务端**不**对外宣告 SMB3 加密能力
	// （即使 MaxDialect 本身就支持加密）。用于反向探针：确认「关掉加密后
	// 客户端不会拿到加密会话、数据平面保持明文」。
	EncryptionDisabled bool
	AllowGuest         bool
	ReadOnly           bool
}

// harness 是一个跑在本进程内的服务端实例。
//
// 刻意**不**通过 exec 启动 cmd/stupidsamba：进程内启动能拿到真实的
// server.Server 对象、失败时直接有 Go 栈、也不受 AGENTS.md §10.3 里
// 「pkill -f 会自杀」那类外部进程管理坑的影响。
type harness struct {
	Addr string // "127.0.0.1:port"
	Root string // 共享根目录（t.TempDir 下）

	srv    *server.Server
	cancel context.CancelFunc
	done   chan struct{}
}

// startServer 启动一个服务端并在测试结束时自动关闭。
func startServer(t *testing.T, opt harnessOptions) *harness {
	t.Helper()

	root := t.TempDir()

	if opt.MinDialect == 0 {
		opt.MinDialect = dialect.SMB202
	}
	if opt.MaxDialect == 0 {
		opt.MaxDialect = dialect.SMB311
	}

	store, err := auth.NewStaticStore(config.Auth{
		AllowGuest: opt.AllowGuest,
		Users:      []config.User{{Name: testUser, Password: testPass}},
	}, testDomain)
	if err != nil {
		t.Fatalf("构造账户表失败: %v", err)
	}

	fs, err := vfs.NewLocalFS(vfs.LocalConfig{
		Root: root,
		// 与 cmd 层装配保持一致：SMB 语义要求大小写不敏感查找。
		CaseInsensitive: true,
		ReadOnly:        opt.ReadOnly,
		VolumeLabel:     testShare,
	})
	if err != nil {
		t.Fatalf("构造 VFS 失败: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	var guid [16]byte
	if _, err := rand.Read(guid[:]); err != nil {
		t.Fatalf("生成 ServerGuid 失败: %v", err)
	}

	settings := &command.Settings{
		ServerName: "STUPIDSAMBA",
		Domain:     testDomain,
		ServerGUID: guid,
		StartTime:  time.Now(),

		MinDialect: opt.MinDialect,
		MaxDialect: opt.MaxDialect,

		SigningRequired:    opt.SigningRequired,
		EncryptionEnabled:  opt.MaxDialect.SupportsEncryption() && !opt.EncryptionDisabled,
		EncryptionRequired: opt.EncryptionRequired,
		AllowSMB1Negotiate: true,
		AllowGuest:         opt.AllowGuest,

		Auth: auth.NewNTLMProvider(auth.Options{
			Store:          store,
			ServerName:     "STUPIDSAMBA",
			DomainName:     testDomain,
			AllowAnonymous: opt.AllowGuest,
		}),
		Shares: []*command.Share{
			{
				Name:       testShare,
				Type:       wire.ShareTypeDisk,
				FS:         fs,
				ReadOnly:   opt.ReadOnly,
				Browseable: true,
			},
			{
				Name:       command.IPCShareName,
				Type:       wire.ShareTypePipe,
				GuestOK:    true,
				Browseable: false,
			},
		},
		// 测试里默认静音：出错时用 -v 配合 SMB_TEST_LOG=debug 打开。
		Logger: testLogger(),
	}

	port := freePort(t)
	srv, err := server.New(server.Options{
		Addresses: []string{"127.0.0.1"},
		Port:      port,
		Settings:  settings,
	})
	if err != nil {
		t.Fatalf("server.New 失败: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("监听 127.0.0.1:%d 失败: %v", port, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		Addr:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Root:   root,
		srv:    srv,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(h.done)
		_ = srv.Serve(ctx)
	}()

	t.Cleanup(h.stop)
	return h
}

// stop 关闭服务端。多次调用安全。
func (h *harness) stop() {
	h.cancel()
	h.srv.Close()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
	}
}

// WriteFile 在共享根下写一个文件，返回其宿主机绝对路径。
func (h *harness) WriteFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(h.Root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	return p
}

// Mkdir 在共享根下建一个目录。
func (h *harness) Mkdir(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(h.Root, filepath.FromSlash(name))
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("建测试目录失败: %v", err)
	}
	return p
}

// testLogger 按环境变量 SMB_TEST_LOG 决定日志级别，默认只留 ERROR。
//
// 排查失败用例时：SMB_TEST_LOG=debug go test -tags integration ./test/integration/ -run Xxx -v
func testLogger() *slog.Logger {
	level := slog.LevelError
	switch os.Getenv("SMB_TEST_LOG") {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// freePort 找一个当前空闲的 TCP 端口。
//
// server.Options.Port == 0 表示「用默认端口 445」而不是「随机端口」，
// 所以不能直接传 0；这里先 bind :0 拿到内核分配的端口再释放。
// 存在极小的竞态窗口，但每个用例独立进程内运行，实践中足够。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("探测空闲端口失败: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("释放探测端口失败: %v", err)
	}
	return port
}
