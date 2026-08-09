//go:build smoke

// 优雅退出的端到端冒烟测试。
//
// 为什么必须端到端：这里要防的回归是「代码里写了 Shutdown 和超时，但一行都没生效」。
// 真实发生过 —— main 把信号 ctx 传给了 srv.Serve，而 internal/server 在 ctx 取消时
// 会直接 Close 每条连接的 socket，于是「等待在途请求完成」被完全绕过，客户端看到硬断。
// 单元测试看不出这种问题：每个函数单独看都是对的，错的是它们的接线方式。
//
// 用 build tag 隔离：本测试要 go build 一个二进制、起进程、占端口，
// 不该拖慢日常的 go test ./...。CI 单独跑：
//
//	go test -tags smoke ./cmd/stupidsamba/ -v
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// smokePort 是本 agent 的专属调试端口，避免与其他并行测试抢端口。
const smokePort = 4468

// exitDeadline 是收到信号后允许的最长退出时间。
//
// main.go 的 shutdownTimeout 是 10s，加上 mDNS goodbye 与进程收尾的余量取 15s。
// 超过这个时间就认为退出流程卡死了。
const exitDeadline = 15 * time.Second

func TestGracefulShutdown(t *testing.T) {
	bin := buildBinary(t)

	t.Run("客户端断开后收到信号应迅速干净退出", func(t *testing.T) {
		srv := startServer(t, bin, smokePort)

		// 最小客户端操作：完成一次真实的 SMB2 NEGOTIATE。
		// 这一步同时证明了服务确实在提供服务，而不只是绑上了端口。
		conn := dialSMB(t, smokePort)
		negotiate(t, conn)
		_ = conn.Close()

		start := time.Now()
		srv.signal(t, syscall.SIGTERM)
		code := srv.waitExit(t, exitDeadline)

		if code != 0 {
			t.Errorf("退出码 = %d, 期望 0（优雅退出不应报错）\n服务端日志:\n%s", code, srv.log())
		}
		// 没有在途连接时不该干等满 shutdownTimeout。
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("无在途连接却花了 %v 才退出，说明没能提前结束等待\n服务端日志:\n%s", elapsed, srv.log())
		}
	})

	t.Run("有在途连接时应等待，二次信号立即强制退出", func(t *testing.T) {
		srv := startServer(t, bin, smokePort)

		// 保持连接不关：服务端应当为它等待，而不是当场把 socket 掐掉。
		conn := dialSMB(t, smokePort)
		negotiate(t, conn)
		defer conn.Close()

		srv.signal(t, syscall.SIGTERM)

		// 第一次信号之后应当还活着（正在等在途连接）。
		// 若这里已经退出，说明又退化成了「硬断」。
		time.Sleep(1500 * time.Millisecond)
		if srv.exited() {
			t.Fatalf("有在途连接时第一次 SIGTERM 就退出了，说明没有等待在途请求\n服务端日志:\n%s", srv.log())
		}

		start := time.Now()
		srv.signal(t, syscall.SIGTERM)
		code := srv.waitExit(t, exitDeadline)

		if code != 0 {
			t.Errorf("退出码 = %d, 期望 0\n服务端日志:\n%s", code, srv.log())
		}
		// 二次信号的意义就是不必干等满 10s 的 shutdownTimeout。
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("二次信号后仍花了 %v 才退出，强制退出通道没生效\n服务端日志:\n%s", elapsed, srv.log())
		}
	})
}

// ---------------------------------------------------------------- 测试脚手架

// buildBinary 编译出被测二进制。
//
// 刻意测「真二进制 + 真信号」而不是在进程内调 run()：信号处理、
// os.Exit 时机、defer 的执行与否，只有真进程才能验。
func buildBinary(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "stupidsamba-smoke")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("编译被测二进制失败: %v\n%s", err, out)
	}
	return bin
}

type smokeServer struct {
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
	code    int
}

// startServer 写一份最小配置并把服务跑起来，返回时端口已经可以连接。
func startServer(t *testing.T, bin string, port int) *smokeServer {
	t.Helper()

	dir := t.TempDir()
	share := filepath.Join(dir, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}

	// mDNS 关掉：CI 容器通常进不了组播组，那是环境限制，
	// 与本测试要验的退出流程无关，开着只会引入偶发失败。
	cfg := fmt.Sprintf(`server:
  name: SMOKETEST
listen:
  addresses: ["127.0.0.1"]
  port: %d
auth:
  users:
    - name: alice
      password: "smoke-secret"
shares:
  - name: data
    path: %s
mdns:
  enabled: false
log:
  level: debug
`, port, share)

	cfgPath := filepath.Join(dir, "smoke.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	cmd := exec.Command(bin, "-config", cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动服务失败: %v", err)
	}

	s := &smokeServer{cmd: cmd, logPath: logPath, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		s.code = exitCode(err)
		close(s.done)
	}()

	// 进程退出时兜底回收，避免测试失败后留下孤儿进程占着端口。
	t.Cleanup(func() {
		if !s.exited() {
			_ = cmd.Process.Kill()
			<-s.done
		}
	})

	s.waitListening(t, port)
	return s
}

// waitListening 轮询到端口可连接为止。
func (s *smokeServer) waitListening(t *testing.T, port int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		if s.exited() {
			t.Fatalf("服务进程在开始监听前就退出了（退出码 %d）\n日志:\n%s", s.code, s.log())
		}
		c, err := net.DialTimeout("tcp", listenAddr(port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("等待服务监听 %d 超时\n日志:\n%s", port, s.log())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *smokeServer) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := s.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("发送信号 %v 失败: %v", sig, err)
	}
}

func (s *smokeServer) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// waitExit 等待进程退出并返回退出码，超时即判定失败。
func (s *smokeServer) waitExit(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-s.done:
		return s.code
	case <-time.After(d):
		_ = s.cmd.Process.Kill()
		t.Fatalf("进程在 %v 内没有退出，优雅退出流程卡死了\n日志:\n%s", d, s.log())
		return -1
	}
}

func (s *smokeServer) log() string {
	b, err := os.ReadFile(s.logPath)
	if err != nil {
		return "(读取日志失败: " + err.Error() + ")"
	}
	return string(b)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func listenAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))
}

// ---------------------------------------------------------------- 最小客户端

func dialSMB(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", listenAddr(port), 5*time.Second)
	if err != nil {
		t.Fatalf("连接服务失败: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

// negotiate 完成一次真实的 SMB2 NEGOTIATE 往返。
//
// 只协商到 3.0.2：3.1.1 需要 preauth integrity context，那是方言协商测试
// （test/integration）的职责，这里只需要证明「服务端真的在处理请求」。
func negotiate(t *testing.T, conn net.Conn) {
	t.Helper()

	req := &wire.NegotiateRequest{
		SecurityMode: wire.NegotiateSigningEnabled,
		Dialects:     []wire.Dialect{wire.SMB202, wire.SMB210, wire.SMB300, wire.SMB302},
	}
	body, err := req.Append(nil)
	if err != nil {
		t.Fatalf("编码 NEGOTIATE 请求: %v", err)
	}
	msg := (&wire.Header{Command: wire.CommandNegotiate, Credits: 1}).Append(nil)
	msg = append(msg, body...)

	if err := writeFrame(conn, msg); err != nil {
		t.Fatalf("发送 NEGOTIATE: %v", err)
	}
	resp, err := readFrame(conn)
	if err != nil {
		t.Fatalf("读取 NEGOTIATE 响应: %v", err)
	}
	nr, err := wire.ParseNegotiateResponse(resp)
	if err != nil {
		t.Fatalf("解析 NEGOTIATE 响应: %v", err)
	}
	if nr.DialectRevision < wire.SMB202 || nr.DialectRevision > wire.SMB302 {
		t.Fatalf("协商出的方言 0x%04x 不在请求范围内", uint16(nr.DialectRevision))
	}
}

// writeFrame 按 Direct TCP 封帧：4 字节头，长度字段是**大端**（MS-SMB2 §2.1）。
func writeFrame(conn net.Conn, msg []byte) error {
	hdr := []byte{0, byte(len(msg) >> 16), byte(len(msg) >> 8), byte(len(msg))}
	if _, err := conn.Write(append(hdr, msg...)); err != nil {
		return err
	}
	return nil
}

func readFrame(conn net.Conn) ([]byte, error) {
	var hdr [4]byte
	if _, err := readFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n <= 0 || n > 1<<20 {
		return nil, fmt.Errorf("帧长度 %d 不合理", n)
	}
	buf := make([]byte, n)
	if _, err := readFull(conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
