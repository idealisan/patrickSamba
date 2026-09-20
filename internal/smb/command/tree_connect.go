package command

import (
	"github.com/idealisan/patrickSamba/internal/auth"
	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

func init() {
	register(wire.CommandTreeConnect, true, false, handleTreeConnect)
	register(wire.CommandTreeDisconnect, true, true, handleTreeDisconnect)
}

// handleTreeConnect 处理 SMB2 TREE_CONNECT（MS-SMB2 §3.3.5.7）。
//
// 路径形如 `\\SERVER\share`。服务端**忽略主机名部分**，只取最后一段做
// 大小写不敏感匹配（protocol-notes §7）。
func handleTreeConnect(ctx *Context) error {
	req, err := wire.ParseTreeConnectRequest(ctx.Msg)
	if err != nil {
		ctx.Log.Debug("TREE_CONNECT 请求解析失败", "remote", ctx.Conn.RemoteAddr, "err", err)
		return status.InvalidParameter
	}

	name := req.ShareName()
	if name == "" {
		return status.BadNetworkName
	}

	share := ctx.Conn.Settings.FindShare(name)
	if share == nil {
		ctx.Log.Debug("共享不存在", "remote", ctx.Conn.RemoteAddr, "share", name)
		return status.BadNetworkName
	}

	id := ctx.Session.Identity()
	if !share.Authorize(id) {
		ctx.Log.Warn("共享访问被拒绝",
			"remote", ctx.Conn.RemoteAddr, "share", share.Name, "user", identityName(id))
		return status.AccessDenied
	}

	tree, st := ctx.Session.NewTree(share)
	if st != status.Success {
		return st
	}
	ctx.Tree = tree
	ctx.Chain.Tree = tree
	ctx.RespHeader.TreeID = tree.ID

	resp := &wire.TreeConnectResponse{
		ShareType:     share.Type,
		MaximalAccess: share.MaximalAccess(),
	}
	if !share.IsIPC() {
		// MANUAL_CACHING(0x0) + FORCE_LEVELII_OPLOCK：我们不实现 oplock/lease，
		// 让客户端知道最多只能拿到 level II，避免它反复申请（protocol-notes §7）。
		resp.ShareFlags = wire.ShareFlagManualCaching | wire.ShareFlagForceLevelIIOplock
	}
	ctx.Out = resp.Append(ctx.Out)

	ctx.Log.Debug("树连接建立",
		"remote", ctx.Conn.RemoteAddr, "session", ctx.Session.ID,
		"tree", tree.ID, "share", share.Name, "read_only", share.ReadOnly)
	return nil
}

// handleTreeDisconnect 处理 SMB2 TREE_DISCONNECT（MS-SMB2 §3.3.5.8）。
func handleTreeDisconnect(ctx *Context) error {
	if _, err := wire.ParseTreeDisconnectRequest(ctx.Msg); err != nil {
		return status.InvalidParameter
	}
	id := ctx.Tree.ID
	if st := ctx.Session.RemoveTree(id); st != status.Success {
		return st
	}
	if ctx.Chain.Tree != nil && ctx.Chain.Tree.ID == id {
		ctx.Chain.Tree = nil
	}
	ctx.Chain.LastOpen = nil

	ctx.Out = (&wire.TreeDisconnectResponse{}).Append(ctx.Out)
	return nil
}

// identityName 返回用于日志的用户名（永远不打印口令或密钥）。
func identityName(id *auth.Identity) string {
	if id == nil {
		return "<nil>"
	}
	if id.Anonymous {
		return "<anonymous>"
	}
	if id.User == "" {
		return "<empty>"
	}
	return id.User
}
