//go:build windows

package mdns

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// setReuse 允许多个套接字绑定同一个 UDP 端口。
//
// Windows 没有 SO_REUSEPORT：在 Windows 上 SO_REUSEADDR 本身就允许多个
// 套接字绑定同一个 UDP 端口并各自收到组播报文，这正是 Bonjour for Windows
// 与本程序共存所需要的语义。
func setReuse(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return sockErr
}
