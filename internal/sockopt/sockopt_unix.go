//go:build !windows

package sockopt

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setReuse 允许多个套接字绑定同一个 UDP 端口。
//
// 这些端口上很可能已经有系统的实现在跑（mDNS 5353、WS-Discovery 3702、
// NetBIOS 137/138），也可能有本程序的另一个实例。我们**不与它们通信**
// （AGENTS.md C4 禁止依赖系统服务），只是不去抢占端口，让各自独立收发。
//
// 纯 Go：golang.org/x/sys/unix 是系统调用号封装，不需要 CGO（C1）。
func setReuse(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			sockErr = err
			return
		}
		// SO_REUSEPORT 在 Linux 3.9+ / BSD / macOS 上才有。
		// 拿不到不影响组播接收，SO_REUSEADDR 才是关键的那一个，因此忽略错误 ——
		// 在只有 REUSEADDR 语义的平台（含 Windows）上 REUSEPORT 本就不存在。
		_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
	if err != nil {
		return err
	}
	return sockErr
}
