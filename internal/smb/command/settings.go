// Package command 实现 SMB2 命令处理：状态机（Connection/Session/Tree/Open）
// 与策略模式的命令分发（AGENTS.md §5 P2）。
//
// 分层说明（AGENTS.md §5）：依赖只能自上而下，`internal/server` 依赖本包，
// 因此**本包绝不能 import internal/server**。为了让 handler 能直接操作
// 会话/树/句柄状态而不引入反向依赖，这些状态类型定义在本包内，
// `internal/server` 只负责监听、传输帧、复合链拆分与写回。
//
// 本包内所有对客户端输入的解析都委托给 internal/smb/wire（P1：报文层与状态层
// 严格分离），所有错误最终都表达为 internal/smb/status.Status（P5）。
package command

import (
	"log/slog"
	"strings"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// 资源上限默认值（AGENTS.md §8：每连接资源必须有上限）。
const (
	// DefaultMaxSessionsPerConn 是单连接允许建立的会话数上限。
	DefaultMaxSessionsPerConn = 64
	// DefaultMaxOpensPerSession 是单会话允许持有的打开句柄数上限。
	DefaultMaxOpensPerSession = 16384
	// DefaultMaxTreesPerSession 是单会话允许连接的树数上限。
	DefaultMaxTreesPerSession = 256
)

// IPCShareName 是 IPC$ 管道共享名（MS-SRVS）。
// 浏览服务器根目录（`\\host` / Finder `smb://host`）需要它。
const IPCShareName = "IPC$"

// SigningPreference 是配置层 server.signing_algorithm 在协议层的表达
// （由 cmd 装配层从 config.Server.SigningAlgorithm 映射而来；
// 本包不 import config，保持协议层可独立测试）。
type SigningPreference uint8

const (
	// SigningAuto 是默认值：不回应 SIGNING_CAPABILITIES negotiate context，
	// 签名算法按方言默认值（2.x HMAC-SHA256，3.x AES-CMAC）。
	// 客户端宣告 GMAC 也不采用 —— 与引入协商之前的行为完全一致。
	SigningAuto SigningPreference = iota
	// SigningPreferAESCMAC 显式选择 AES-CMAC（RFC 4493）。
	SigningPreferAESCMAC
	// SigningPreferAESGMAC 选择 AES-GMAC（MS-SMB2 §3.1.4.1；GCM 硬件路径）。
	SigningPreferAESGMAC
)

// Settings 是 command 层需要的服务端级设置。
//
// 由 internal/server 从 internal/config.Config 装配而来 —— 本包**不 import config**，
// 以保持协议层可以脱离配置文件单独测试。创建后视为只读。
type Settings struct {
	// ServerName 是 NetBIOS/主机名，出现在 NTLM TargetInfo 与共享路径中。
	ServerName string
	// Domain 是工作组/域名。
	Domain string
	// ServerGUID 是 NEGOTIATE Response 里宣告的服务端 GUID，进程启动时随机生成。
	ServerGUID [16]byte
	// StartTime 是服务启动时刻，用于 NEGOTIATE 的 ServerStartTime。
	StartTime time.Time

	// MinDialect / MaxDialect 限定方言协商区间。
	MinDialect dialect.Dialect
	MaxDialect dialect.Dialect

	// SigningRequired 表示强制要求客户端对请求签名。
	SigningRequired bool
	// SigningPreference 控制 3.1.1 SIGNING_CAPABILITIES 协商策略
	// （默认 SigningAuto = 历史行为）。选择规则见 negotiate.go 的
	// negotiateSigning。
	SigningPreference SigningPreference
	// EncryptionEnabled 表示允许 SMB3 加密（协商 cipher）。
	EncryptionEnabled bool
	// EncryptionRequired 表示强制要求 SMB3 加密。
	EncryptionRequired bool

	// AllowSMB1Negotiate 允许响应 SMB1 多协议协商入口（只用于升级到 SMB2）。
	AllowSMB1Negotiate bool

	// AllowGuest 允许 guest 降级登录。
	AllowGuest bool

	// AppleModel 是 AAPL ModelString，决定 Finder 里显示的图标形状。
	//
	// 与配置里的 mdns.apple.model 是**同一个值**（cmd 层装配时传入）：此前
	// AAPL 侧硬编码 DefaultAppleModel，改了配置只有 mDNS 的 _device-info._tcp
	// 跟着变，AAPL 响应里还是 MacSamba —— 属于「配置说的和协议回的不一致」。
	// 空字符串表示回落到 DefaultAppleModel。
	AppleModel string

	// Oplocks 允许授予 oplock / lease（客户端本地缓存）。
	//
	// 关闭时 CREATE 一律回 SMB2_OPLOCK_LEVEL_NONE 且不宣告
	// SMB2_GLOBAL_CAP_LEASING —— 与引入本特性之前的行为完全一致。
	// 默认关闭的理由见 docs / CHANGELOG：本特性尚未在 Windows / macOS
	// 真机上验收，而授予缓存许可出错的后果是**静默的脏数据**，
	// 不是连不上。没有真机验证之前，保守一侧是正确的默认。
	Oplocks bool

	// Auth 是认证后端（SPNEGO/NTLMv2）。
	Auth auth.Provider

	// Shares 是全部已装配的共享（含 IPC$）。
	Shares []*Share

	// Pipes 是 IPC$ 上的命名管道后端（由 internal/dcerpc 实现、cmd 层装配）。
	// nil 时对 IPC$ 的 CREATE 一律回 STATUS_OBJECT_NAME_NOT_FOUND。
	Pipes PipeOpener

	// 资源上限，0 表示使用默认值。
	MaxSessionsPerConn int
	MaxTreesPerSession int
	MaxOpensPerSession int

	// Logger 是日志句柄，nil 时使用 slog.Default()。
	Logger *slog.Logger
}

// Log 返回可用的日志句柄。
func (s *Settings) Log() *slog.Logger {
	if s == nil || s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func (s *Settings) maxSessions() int {
	if s.MaxSessionsPerConn > 0 {
		return s.MaxSessionsPerConn
	}
	return DefaultMaxSessionsPerConn
}

func (s *Settings) maxTrees() int {
	if s.MaxTreesPerSession > 0 {
		return s.MaxTreesPerSession
	}
	return DefaultMaxTreesPerSession
}

func (s *Settings) maxOpens() int {
	if s.MaxOpensPerSession > 0 {
		return s.MaxOpensPerSession
	}
	return DefaultMaxOpensPerSession
}

// appleModel 返回 AAPL ModelString 的取值。
//
// s 为 nil（直接构造 Conn 而没给 Settings 的极简场景）或未配置时回落到
// DefaultAppleModel —— 与 config 层 mdns.apple.model 的默认值是同一个常量，
// 两侧不会有第二个真相源。
func (s *Settings) appleModel() string {
	if s == nil || s.AppleModel == "" {
		return DefaultAppleModel
	}
	return s.AppleModel
}

// FindShare 按共享名查找共享。
//
// MS-SMB2 §3.3.5.7：share 名匹配**大小写不敏感**。
func (s *Settings) FindShare(name string) *Share {
	for _, sh := range s.Shares {
		if strings.EqualFold(sh.Name, name) {
			return sh
		}
	}
	return nil
}

// Share 是一个已装配好的共享：配置项 + VFS 后端。
type Share struct {
	// Name 是共享名（客户端看到的 \\server\Name）。
	Name string
	// Comment 是共享描述，出现在共享枚举里。
	Comment string

	// Type 是 SMB2 ShareType。磁盘共享为 DISK，IPC$ 为 PIPE。
	Type wire.ShareType

	// FS 是后端文件系统。IPC$ 没有文件系统，此处为 nil。
	FS vfs.FileSystem

	// ReadOnly 只读共享。
	ReadOnly bool
	// GuestOK 允许 guest 身份访问。
	GuestOK bool
	// Browseable 控制是否出现在共享枚举中。
	Browseable bool
	// ValidUsers 限定可访问的用户名（大小写不敏感），空表示所有已认证用户。
	ValidUsers []string
	// TimeMachine 把本共享宣告为 Time Machine 备份目标（阶段二）。
	TimeMachine bool

	// locks 是本共享的字节范围锁表（SMB2 LOCK，见 lock.go）。
	//
	// 锁挂在 Share 而不是 Session/Tree 上：字节范围锁的意义就是跨客户端
	// 互斥，两个会话连到同一个共享的同一个文件必须能看见彼此的锁。
	// 零值可用，惰性建表。
	locks lockTable

	// shareModes 是本共享的共享模式（ShareAccess）表（见 share_access.go）。
	// 与 locks 同理：跨会话可见才有意义。零值可用。
	shareModes shareModeTable

	// oplocks 是本共享的 oplock / lease 表（见 oplock_state.go）。
	// 与 locks 同理挂在 Share 上。零值可用（未开启时全程不碰）。
	oplocks oplockTable

	// notify 是本共享的目录变更事件中心（见 notify_hub.go）。
	//
	// 与 locks 同理挂在 Share 上：一个客户端改动目录，要能通知到另一个
	// 客户端挂着的 CHANGE_NOTIFY。零值可用（无订阅时投递是空转）。
	notify notifyHub
}

// IsIPC 报告本共享是否为 IPC$ 管道共享。
func (s *Share) IsIPC() bool { return s.Type == wire.ShareTypePipe }

// Authorize 判定某个身份是否可以连接本共享。
//
// 授权**完全由配置决定**（AGENTS.md §1.1 C8）：不读取宿主文件系统 ACL，
// 不做系统用户解析。
//
// 规则：
//   - 匿名/guest 身份只有在 GuestOK 时才放行；
//   - ValidUsers 非空时，用户名必须在列表内（大小写不敏感）。
func (s *Share) Authorize(id *auth.Identity) bool {
	if id == nil {
		return false
	}
	if id.Guest || id.Anonymous {
		if !s.GuestOK {
			return false
		}
		// guest 不可能出现在 valid_users 里（那是具名用户列表），
		// 因此 GuestOK 就是 guest 的全部授权依据。
		return true
	}
	if len(s.ValidUsers) == 0 {
		return true
	}
	for _, u := range s.ValidUsers {
		if strings.EqualFold(u, id.User) {
			return true
		}
	}
	return false
}

// MaximalAccess 返回 TREE_CONNECT Response 应当回报的 MaximalAccess
// （protocol-notes §7）。
func (s *Share) MaximalAccess() uint32 {
	if s.ReadOnly {
		return wire.MaximalAccessReadOnly
	}
	return wire.MaximalAccessReadWrite
}

// WritableFor 报告在给定树上是否允许写操作。
func (s *Share) WritableFor() bool {
	if s.ReadOnly {
		return false
	}
	if s.FS != nil && s.FS.ReadOnly() {
		return false
	}
	return true
}
