// admin.go —— 可选的测量管理端口（性能分析专用，默认关闭）。
//
// 仅当环境变量 STUPIDSAMBA_ADMIN_ADDR 非空时启动，例如：
//
//	STUPIDSAMBA_ADMIN_ADDR=127.0.0.1:6455 stupidsamba -config cfg.yaml
//
// 提供两类内容：
//   - /debug/pprof/*  Go pprof 采集点（CPU/heap/block/mutex/goroutine…）
//   - /debug/vars     expvar 计数器（含传输层帧计数）
//
// 安全边界（硬约束）：地址的 host 部分**必须是 loopback**，
// 否则拒绝启动 —— profile 数据绝不允许暴露到非回环接口。
// 开启时会顺带调高 block/mutex 采样率，属于测量态配置；
// 默认（未设环境变量）进程行为与不开启完全一致。
package main

import (
	"expvar"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"time"

	"github.com/idealisan/patrickSamba/internal/server"
)

func startAdminFromEnv(log *slog.Logger) error {
	addr := os.Getenv("STUPIDSAMBA_ADMIN_ADDR")
	if addr == "" {
		return nil
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("STUPIDSAMBA_ADMIN_ADDR 格式非法（要 host:port）: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		if host != "localhost" {
			return fmt.Errorf("STUPIDSAMBA_ADMIN_ADDR 的 host %q 不是 IP", host)
		}
	} else if !ip.IsLoopback() {
		return fmt.Errorf("STUPIDSAMBA_ADMIN_ADDR 只允许 loopback 地址，拒绝 %q", host)
	}

	// 测量态采样率：block 每 1µs 阻塞事件采一针，mutex 1/100 抽样。
	// 不开启管理端口时保持 Go 默认（0），零开销。
	runtime.SetBlockProfileRate(1000)
	runtime.SetMutexProfileFraction(100)

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range []string{"heap", "goroutine", "block", "mutex",
		"allocs", "threadcreate"} {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}

	expvar.Publish("smb_transport", expvar.Func(func() any {
		inF, inB, outF, outB := server.TransportStats()
		return map[string]uint64{
			"frames_in": inF, "bytes_in": inB,
			"frames_out": outF, "bytes_out": outB,
		}
	}))
	mux.Handle("/debug/vars", expvar.Handler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Info("测量管理端口已启动（仅 loopback）",
		"addr", addr, "pprof", addr+"/debug/pprof/")
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("管理端口异常退出", "err", err)
		}
	}()
	return nil
}
