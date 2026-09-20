package command

import (
	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

func init() {
	// ECHO 是 keepalive，MS-SMB2 §3.3.5.13 允许在没有会话时发送，
	// 因此 needSession = false。
	register(wire.CommandEcho, false, false, handleEcho)
}

// handleEcho 处理 SMB2 ECHO（MS-SMB2 §3.3.5.13）：原样回一个空响应。
func handleEcho(ctx *Context) error {
	if _, err := wire.ParseEchoRequest(ctx.Msg); err != nil {
		return status.InvalidParameter
	}
	ctx.Out = (&wire.EchoResponse{}).Append(ctx.Out)
	return nil
}
