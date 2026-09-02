//go:build windows

package sockopt

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// setReuse 允许多个套接字绑定同一个 UDP 端口。
//
// Windows 没有 SO_REUSEPORT：在 Windows 上 SO_REUSEADDR 本身就允许多个
// 套接字绑定同一个 UDP 端口并各自收到组播/广播报文，这正是本程序与系统
// 实现共存所需要的语义。
func setReuse(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
		if sockErr == nil {
			sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_BROADCAST, 1)
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}

// setBroadcast 允许这个套接字发送广播报文（SO_BROADCAST）。
//
// Windows 有独立的 SO_BROADCAST 常量（BSD 里它与 SO_REUSEADDR 值相同，
// Windows 不是），必须显式设置。
func setBroadcast(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_BROADCAST, 1)
	})
	if err != nil {
		return err
	}
	return sockErr
}
