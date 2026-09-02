// Command stupidsamba 是一个纯 Go 实现的 SMB/CIFS 文件共享服务器。
//
// 约束见 AGENTS.md：禁止 CGO、禁止外部动态库、禁止依赖外部进程。
// 特别地，mDNS/DNS-SD 广播由本进程自己收发报文实现（C4），
// 不依赖 avahi / Bonjour / systemd-resolved。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/finalappstore/stupidsamba/internal/config"
	"github.com/finalappstore/stupidsamba/internal/server"
)

// 版本信息由 scripts/build-release.sh 通过 -ldflags -X main.xxx 注入。
// 直接 go build 时保持下面的默认值 —— 默认值刻意不伪装成真版本号，
// 让「哪里来的二进制」一眼可辨。三个变量都会在 -version 输出里体现。
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// shutdownTimeout 是优雅关闭时等待既有连接结束的上限，超时则强制断开。
const shutdownTimeout = 10 * time.Second

func main() {
	var (
		configPath  = flag.String("config", "", "配置文件路径 (YAML)")
		showVersion = flag.Bool("version", false, "打印版本后退出")
		checkOnly   = flag.Bool("check", false, "只校验配置文件后退出，不启动服务")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("stupidsamba %s (commit %s, built %s, %s/%s, %s)\n",
			version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	if err := run(*configPath, *checkOnly); err != nil {
		fmt.Fprintf(os.Stderr, "stupidsamba: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, checkOnly bool) error {
	if configPath == "" {
		return errors.New("必须通过 -config 指定配置文件")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log, logFile, err := newLogger(cfg.Log)
	if err != nil {
		return err
	}
	if logFile != nil {
		defer logFile.Close()
	}
	// internal/mdns 等库在未显式注入 logger 时用 slog.Default()。
	slog.SetDefault(log)

	// 配置层的告警（特权端口、guest、明文口令、未强制签名……）统一在这里落日志。
	for _, w := range config.Warnings(cfg) {
		log.Warn(w)
	}

	if checkOnly {
		fmt.Printf("配置校验通过：%s（%d 个共享）\n", configPath, len(cfg.Shares))
		return nil
	}

	provider, err := buildAuth(cfg)
	if err != nil {
		return err
	}

	shares, err := buildShares(cfg)
	if err != nil {
		return err
	}
	defer closeShares(shares)

	settings, err := buildSettings(cfg, provider, shares, log)
	if err != nil {
		return err
	}

	srv, err := server.New(server.Options{
		Addresses:      cfg.Listen.Addresses,
		Port:           cfg.Listen.Port,
		Settings:       settings,
		MaxConnections: cfg.Server.MaxConnections,
		Logger:         log,
	})
	if err != nil {
		return err
	}

	// 先 bind 再启动后台任务：绑定失败要立刻报错退出，
	// 不能出现「mDNS 已经广播出去了但根本没在监听」的窗口。
	if err := srv.Listen(); err != nil {
		return bindError(cfg, err)
	}

	// 可选测量端口（STUPIDSAMBA_ADMIN_ADDR，仅 loopback）。
	if err := startAdminFromEnv(log); err != nil {
		return err
	}

	log.Info("SMB 服务已监听",
		"addrs", addrStrings(srv),
		"dialects", fmt.Sprintf("%s..%s", settings.MinDialect, settings.MaxDialect),
		"shares", shareNames(cfg))

	// 缓冲 2：第一个信号触发优雅关闭，第二个信号强制断开。
	// 用 signal.Notify 而不是 NotifyContext，就是为了能收到"第二次 Ctrl-C"。
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// 刻意传 Background 而不是信号 ctx：internal/server 在 ctx 取消时会
	// 直接 Close 每条连接的 socket，那样"等待在途请求完成"就无从谈起，
	// 客户端会看到连接被硬断。停止 accept 与等待排空统一由 srv.Shutdown 驱动。
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(context.Background()) }()

	discoveries := startDiscovery(cfg, log)

	select {
	case sig := <-sigCh:
		log.Info("收到退出信号，开始优雅关闭", "signal", sig.String())
	case err := <-serveErr:
		if err != nil && !errors.Is(err, server.ErrServerClosed) {
			shutdown(discoveries, srv, log, sigCh)
			return err
		}
	}

	shutdown(discoveries, srv, log, sigCh)
	return nil
}

// shutdown 优雅关闭：停止 accept → mDNS goodbye → 等待在途请求 → 强制收尾。
//
// 三段顺序都有理由：
//   - 先停 accept：关闭期间不该再放新客户端进来。srv.Shutdown 的第一件事
//     就是关监听套接字，所以先把它挂到 goroutine 上跑起来。
//   - 再停服务发现（mDNS goodbye TTL=0、NetBIOS 与 WS-Discovery 停止应答）：
//     把自己从客户端的服务列表里摘掉。晚于关监听会留下"Finder 里还看得见
//     但点进去连不上"的窗口，早于关监听则等于还在广播一个正在退场的服务。
//   - 最后等在途请求自然结束，超时（或再收到一次信号）才强制断开。
//
// force 用于接收第二次退出信号：卡住的客户端不该让 Ctrl-C 失效。
func shutdown(discoveries []discovery, srv *server.Server, log *slog.Logger, force <-chan os.Signal) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()

	// mDNS 的 Stop 会发两轮 goodbye（各隔 250ms），期间 accept 已经停了。
	// 三个发现组件用同一套顺序收尾：都先撤回宣告，再等在途请求。
	stopDiscovery(discoveries)

	go func() {
		select {
		case sig := <-force:
			log.Warn("再次收到退出信号，立即断开所有连接", "signal", sig.String())
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := <-done; err != nil {
		log.Warn("在途请求未能在超时内结束，已强制断开",
			"timeout", shutdownTimeout, "err", err)
	}
	log.Info("已退出")
}

// bindError 给监听失败补上人话提示。
//
// 445 是特权端口，「permission denied」对第一次跑的人毫无信息量。
func bindError(cfg *config.Config, err error) error {
	if cfg.Listen.Port >= 1024 || runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"listen.port=%d 是特权端口（<1024），当前进程不是 root。三种解决办法：\n"+
		"  1. 以 root 运行；\n"+
		"  2. 给二进制授权：sudo setcap 'cap_net_bind_service=+ep' %s；\n"+
		"  3. 改用 >=1024 的端口（客户端需显式指定，如 smbclient -p 4445）。",
		err, cfg.Listen.Port, selfPath())
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "<二进制路径>"
	}
	return p
}

func addrStrings(srv *server.Server) string {
	addrs := srv.Addrs()
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return strings.Join(out, ", ")
}

func shareNames(cfg *config.Config) string {
	out := make([]string, 0, len(cfg.Shares))
	for i := range cfg.Shares {
		out = append(out, cfg.Shares[i].Name)
	}
	return strings.Join(out, ", ")
}
