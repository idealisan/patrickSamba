// Package auth 定义认证抽象与实现（SPNEGO / NTLMv2 服务端校验）。
//
// 本文件是**跨模块共享契约**，修改前必须通知所有相关 agent（AGENTS.md §7.3）。
package auth

import "errors"

// Identity 是认证成功后确定的用户身份。
type Identity struct {
	// User 是登录用户名（不含域）。guest/匿名时可为空。
	User string
	// Domain 是登录域/工作组。
	Domain string
	// Workstation 是客户端声明的机器名，仅用于日志。
	Workstation string

	// Guest 表示这是一个 guest 会话（对应 SMB2_SESSION_FLAG_IS_GUEST）。
	Guest bool
	// Anonymous 表示这是一个匿名/null 会话（对应 SMB2_SESSION_FLAG_IS_NULL）。
	Anonymous bool

	// UID/GID 是映射到的本地用户，供 VFS 层做权限决策。0 表示不映射。
	UID uint32
	GID uint32
}

// Account 是账户后端中的一条用户记录。
type Account struct {
	User   string
	Domain string

	// NTHash 是 MD4(UTF16LE(password))，即 NTLM 的 NT hash。
	// 服务端校验 NTLMv2 只需要它，不需要明文口令。
	NTHash [16]byte

	UID uint32
	GID uint32
}

// AccountStore 提供账户查询。实现必须是并发安全的。
//
// 查找应当**大小写不敏感**（Windows 语义）。
type AccountStore interface {
	// Lookup 返回指定用户的账户记录。不存在时返回 ErrNoSuchUser。
	//
	// 为避免用户枚举，调用方在用户不存在时也应执行一次等价耗时的
	// 校验运算后再返回失败。
	Lookup(user, domain string) (*Account, error)

	// AllowGuest 表示在用户不存在或口令错误时是否降级为 guest 登录。
	AllowGuest() bool
}

// Context 是一次会话认证的进行中状态（NTLM 是多轮握手）。
//
// 生命周期：Server 收到第一个 SESSION_SETUP 时创建，
// 认证完成或会话销毁时丢弃。非并发安全，由单个会话串行使用。
type Context interface {
	// Step 处理客户端送来的一个 GSS-API token，返回要回给客户端的 token。
	//
	// 返回值 done 为 false 表示还需要更多轮次，此时 SMB 层应回
	// STATUS_MORE_PROCESSING_REQUIRED；done 为 true 且 err 为 nil 表示认证成功，
	// 此时可以调用 Identity() 与 SessionKey()。
	Step(in []byte) (out []byte, done bool, err error)

	// Identity 返回认证成功后的身份。认证未完成时返回 nil。
	Identity() *Identity

	// SessionKey 返回 16 字节的 ExportedSessionKey，
	// 供 SMB 签名/加密密钥派生使用（MS-SMB2 §3.3.5.5.3）。
	// 匿名会话返回全零。
	SessionKey() []byte
}

// Provider 创建认证上下文。
type Provider interface {
	// Name 是机制名，用于日志（如 "ntlmssp"）。
	Name() string

	// InitialToken 返回放在 SMB2 NEGOTIATE Response 里的
	// SPNEGO negTokenInit2（宣告服务端支持的机制）。
	InitialToken() []byte

	// NewContext 创建一次新的认证会话。
	NewContext() Context
}

var (
	// ErrNoSuchUser 表示账户不存在。
	ErrNoSuchUser = errors.New("auth: no such user")
	// ErrLogonFailure 表示口令校验失败。映射为 STATUS_LOGON_FAILURE。
	ErrLogonFailure = errors.New("auth: logon failure")
	// ErrInvalidToken 表示收到的 GSS/NTLM token 格式非法。
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrMechUnsupported 表示客户端请求的认证机制不受支持。
	ErrMechUnsupported = errors.New("auth: unsupported mechanism")
)
