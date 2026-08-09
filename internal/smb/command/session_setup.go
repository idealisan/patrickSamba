package command

import (
	"errors"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	// SESSION_SETUP 自己负责会话的创建与状态，不能要求"已认证会话"前置。
	register(wire.CommandSessionSetup, false, false, handleSessionSetup)
	register(wire.CommandLogoff, true, false, handleLogoff)
}

// handleSessionSetup 处理 SMB2 SESSION_SETUP（MS-SMB2 §3.3.5.5）。
//
// NTLM 是多轮握手：
//  1. 收到 NTLMSSP_NEGOTIATE → **分配非 0 SessionId 写入响应头**，
//     状态 STATUS_MORE_PROCESSING_REQUIRED，Buffer 带 NTLMSSP_CHALLENGE。
//     注意这是错误码但**必须带完整的 SESSION_SETUP 响应体**。
//  2. 收到 NTLMSSP_AUTHENTICATE → 成功 STATUS_SUCCESS / 失败 STATUS_LOGON_FAILURE。
func handleSessionSetup(ctx *Context) error {
	conn := ctx.Conn
	if !conn.NegotiateDone {
		// 未协商就 SESSION_SETUP 是协议错误（MS-SMB2 §3.3.5.5）。
		return status.InvalidParameter
	}

	req, err := wire.ParseSessionSetupRequest(ctx.Msg)
	if err != nil {
		ctx.Log.Debug("SESSION_SETUP 请求解析失败", "remote", conn.RemoteAddr, "err", err)
		return status.InvalidParameter
	}

	// 不支持多通道，因此不接受会话绑定（MS-SMB2 §3.3.5.5.1）。
	if req.Flags&wire.SessionSetupFlagBinding != 0 {
		return status.RequestNotAccepted
	}

	sess := ctx.Session
	switch {
	case ctx.Header.SessionID == 0:
		// 新会话。
		var st status.Status
		sess, st = conn.NewSession()
		if st != status.Success {
			return st
		}
	case sess == nil:
		// 客户端引用了一个不存在的 SessionId。
		return status.UserSessionDeleted
	case sess.Established():
		// 已认证会话上重新 SESSION_SETUP：重新认证（Windows 会这么做）。
		// 简化处理：拒绝，让客户端重新建会话。
		return status.RequestNotAccepted
	}

	ctx.Session = sess
	// 响应头必须带上 SessionId —— 第一轮就要带，否则客户端不知道往哪续。
	ctx.RespHeader.SessionID = sess.ID

	// 3.1.1：把**请求**滚进会话级 preauth hash（protocol-notes §6 第 3/5 步）。
	if conn.Dialect == dialect.SMB311 {
		sess.UpdatePreauthHash(ctx.Msg)
	}

	authCtx := sess.AuthContext()
	if authCtx == nil {
		authCtx = conn.Settings.Auth.NewContext()
		sess.SetAuthContext(authCtx)
	}

	token, done, authErr := authCtx.Step(req.SecurityBuffer)
	if authErr != nil {
		ctx.Log.Warn("认证失败", "remote", conn.RemoteAddr, "session", sess.ID, "err", authErr)
		conn.RemoveSession(sess.ID)
		ctx.Session = nil
		return authStatus(authErr)
	}

	resp := &wire.SessionSetupResponse{SecurityBuffer: token}

	if !done {
		// 中间轮次：带完整响应体 + STATUS_MORE_PROCESSING_REQUIRED。
		ctx.Status = status.MoreProcessingRequired
		out, err := resp.Append(ctx.Out)
		if err != nil {
			return status.InsuffServerResources
		}
		ctx.Out = out
		// 中间轮次的响应也要进 preauth hash。
		if conn.Dialect == dialect.SMB311 {
			ctx.HashResponseSession = sess
		}
		return nil
	}

	// —— 认证成功 ——
	id := authCtx.Identity()
	if id == nil {
		conn.RemoveSession(sess.ID)
		ctx.Session = nil
		return status.LogonFailure
	}

	if id.Guest {
		resp.SessionFlags |= wire.SessionFlagIsGuest
	}
	if id.Anonymous {
		resp.SessionFlags |= wire.SessionFlagIsNull
	}

	sessionKey := authCtx.SessionKey()
	sess.Establish(id)

	// 派生签名/加密密钥。
	//
	// 3.1.1 用**当前**（即含最后一条 SESSION_SETUP Request 的）会话级
	// preauth hash 作为 KDF 的 Context —— 最后一条成功响应不参与
	// （protocol-notes §6）。
	ph := sess.PreauthHash()
	d := uint16(conn.Dialect)
	keys := Keys{
		SessionKey:     sessionKey,
		SigningKey:     crypto.SigningKey(d, sessionKey, ph[:]),
		ApplicationKey: crypto.ApplicationKey(d, sessionKey, ph[:]),
	}
	if conn.Cipher != 0 {
		klen := cipherKeyLen(conn.Cipher)
		keys.EncryptKey = crypto.ServerOutKey(d, sessionKey, ph[:], klen)
		keys.DecryptKey = crypto.ServerInKey(d, sessionKey, ph[:], klen)
	}
	sess.SetKeys(keys)

	// guest / 匿名会话没有真正的 session key，**不能**要求签名
	// （protocol-notes §5：否则 Windows 会连不上）。
	signing := conn.SigningRequired && !id.Guest && !id.Anonymous
	sess.SetSigningRequired(signing)

	// 加密强制的**兜底**（backstop）。
	//
	// 正常流程下这条 `Cipher == 0` 分支不可达：协商层（handleNegotiate 与
	// AppendSMB1NegotiateReply 两个出口）已经在 EncryptionRequired 且协商不出
	// 算法时返回 ACCESS_DENIED。留这道兜底是因为这里曾经写的是
	// `EncryptionRequired && Cipher != 0` —— 那个 `&& Cipher != 0` 把一条安全
	// 要求变成了空操作：低方言下 Cipher 恒为 0，于是 SetEncryptData 被静默跳过，
	// 会话以明文继续。安全要求不满足时必须**失败**，绝不能降级成明文放行。
	// 将来若再新增第三条协商出口而忘了同样 fail closed，会在这里被挡下。
	if conn.Settings.EncryptionRequired {
		if conn.Cipher == 0 {
			ctx.Log.Error("协商层未能拦住无加密连接，会话建立阶段兜底拒绝（这是 BUG，请报告）",
				"dialect", conn.Dialect,
				"remote", conn.RemoteAddr)
			return status.AccessDenied
		}
		sess.SetEncryptData(true)
		resp.SessionFlags |= wire.SessionFlagEncryptData
	}

	out, err := resp.Append(ctx.Out)
	if err != nil {
		return status.InsuffServerResources
	}
	ctx.Out = out

	// MS-SMB2 §3.3.5.5.3：SESSION_SETUP 的最终成功响应**必须签名**
	// （只要有可用的签名密钥）。
	if !id.Guest && !id.Anonymous {
		ctx.SignKey = sess.SigningKey()
	}

	ctx.Log.Info("会话建立",
		"remote", conn.RemoteAddr,
		"session", sess.ID,
		"user", id.User,
		"domain", id.Domain,
		"guest", id.Guest,
		"anonymous", id.Anonymous,
		"signing", signing,
	)
	return nil
}

// cipherKeyLen 返回加密算法对应的密钥长度（字节）。
func cipherKeyLen(cipher uint16) int {
	switch cipher {
	case wire.CipherAES256CCM, wire.CipherAES256GCM:
		return 32
	default:
		return 16
	}
}

// authStatus 把认证错误映射为 NTSTATUS（AGENTS.md §5 P5）。
func authStatus(err error) status.Status {
	switch {
	case errors.Is(err, auth.ErrLogonFailure), errors.Is(err, auth.ErrNoSuchUser):
		return status.LogonFailure
	case errors.Is(err, auth.ErrInvalidToken):
		return status.InvalidParameter
	case errors.Is(err, auth.ErrMechUnsupported):
		return status.NotSupported
	default:
		return status.LogonFailure
	}
}

// handleLogoff 处理 SMB2 LOGOFF（MS-SMB2 §3.3.5.6）：拆除会话及其全部树与句柄。
func handleLogoff(ctx *Context) error {
	if _, err := wire.ParseLogoffRequest(ctx.Msg); err != nil {
		return status.InvalidParameter
	}
	id := ctx.Session.ID
	ctx.Conn.RemoveSession(id)
	ctx.Chain.Session = nil
	ctx.Chain.Tree = nil
	ctx.Chain.LastOpen = nil

	ctx.Out = (&wire.LogoffResponse{}).Append(ctx.Out)
	ctx.Log.Debug("会话注销", "remote", ctx.Conn.RemoteAddr, "session", id)
	return nil
}
