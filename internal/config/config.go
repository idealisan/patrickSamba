// Package config 定义 stupidSamba 的 YAML 配置结构与校验。
//
// 本文件是**跨模块共享契约**，修改前必须通知所有相关 agent（AGENTS.md §7.3）。
package config

// Config 是配置文件根结构。
type Config struct {
	Server Server  `yaml:"server"`
	Listen Listen  `yaml:"listen"`
	Auth   Auth    `yaml:"auth"`
	Shares []Share `yaml:"shares"`
	MDNS   MDNS    `yaml:"mdns"`
	Log    Log     `yaml:"log"`
}

// Server 是服务器级设置。
type Server struct {
	// Name 是 NetBIOS/主机名，出现在 NTLM TargetInfo 与 mDNS 中。
	// 留空则使用系统主机名。
	Name string `yaml:"name"`
	// Domain 是工作组/域名，默认 "WORKGROUP"。
	Domain string `yaml:"domain"`
	// MaxDialect / MinDialect 限定协商范围，形如 "3.1.1" / "2.0.2"。
	MaxDialect string `yaml:"max_dialect"`
	MinDialect string `yaml:"min_dialect"`
	// SigningRequired 强制要求 SMB 签名。
	SigningRequired bool `yaml:"signing_required"`
	// EncryptionRequired 强制要求 SMB3 加密。
	EncryptionRequired bool `yaml:"encryption_required"`
	// MaxConnections 是并发连接数上限，0 表示不限。
	MaxConnections int `yaml:"max_connections"`
	// SMB1 控制是否响应 SMB1 多协议协商入口（用于升级到 SMB2）。
	SMB1 bool `yaml:"smb1"`
}

// Listen 是监听设置。
//
// 按需求：地址是**列表**（可监听多个 IP），端口只允许**一个**。
type Listen struct {
	// Addresses 是要监听的 IP 列表。留空表示监听所有地址（0.0.0.0 + ::）。
	Addresses []string `yaml:"addresses"`
	// Port 是监听端口，只允许一个。默认 445。
	Port int `yaml:"port"`
}

// Auth 是认证设置。
type Auth struct {
	// AllowGuest 允许 guest 登录（口令错误或用户不存在时降级）。
	AllowGuest bool `yaml:"allow_guest"`
	// Users 是静态用户表。
	Users []User `yaml:"users"`
}

// User 是一个静态账户。
//
// Password 与 NTHash 二选一，优先使用 NTHash（避免明文口令落盘）。
type User struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password"`
	// NTHash 是 32 位十六进制字符串，即 MD4(UTF16LE(password))。
	NTHash string `yaml:"nt_hash"`
	UID    uint32 `yaml:"uid"`
	GID    uint32 `yaml:"gid"`
}

// Share 是一个共享目录。
type Share struct {
	// Name 是共享名（客户端看到的名字，如 \\server\Name）。
	Name string `yaml:"name"`
	// Path 是本地目录绝对路径。
	Path string `yaml:"path"`
	// Comment 是共享描述。
	Comment string `yaml:"comment"`
	// ReadOnly 只读共享。
	ReadOnly bool `yaml:"read_only"`
	// Browseable 控制是否出现在共享枚举中。
	Browseable *bool `yaml:"browseable"`
	// GuestOK 允许 guest 访问本共享。
	GuestOK bool `yaml:"guest_ok"`
	// ValidUsers 限定可访问的用户，留空表示所有已认证用户。
	ValidUsers []string `yaml:"valid_users"`
	// TimeMachine 把本共享宣告为 Time Machine 备份目标（阶段二）。
	TimeMachine bool `yaml:"time_machine"`
	// TimeMachineMaxSize 限制 Time Machine 可用容量（字节），0 表示不限。
	TimeMachineMaxSize uint64 `yaml:"time_machine_max_size"`
	// MetadataPath 是 POSIX 元数据旁路存储（纯 Go 嵌入式 KV）的落盘路径。
	//
	// **仅 Windows 使用**（AGENTS.md §5 P7）：NTFS 表达不了 POSIX 的
	// uid/gid/mode，需要旁路存储；Linux/macOS 原生能力足够，该字段留空即可，
	// 填了也会被忽略（会有一条启动 WARN）。
	//
	// 留空时由 vfs 层决定默认落点，config 不替它做决定。
	MetadataPath string `yaml:"metadata_path"`
}

// MDNS 是 mDNS/DNS-SD 广播设置。
type MDNS struct {
	// Enabled 开关。
	Enabled bool `yaml:"enabled"`
	// Instance 是服务实例名，留空则用 Server.Name。
	Instance string `yaml:"instance"`
	// Interfaces 限定广播网卡名，留空表示所有可用网卡。
	Interfaces []string `yaml:"interfaces"`
	// Apple 控制苹果生态扩展 TXT 记录。
	Apple AppleMDNS `yaml:"apple"`
}

// AppleMDNS 是 Apple 生态的 mDNS 扩展字段。
type AppleMDNS struct {
	// Enabled 是否广播 _device-info._tcp 等 Apple 专用记录。
	Enabled bool `yaml:"enabled"`
	// Model 是 _device-info._tcp 的 model= 值，决定 Finder 里显示的图标。
	// 例如 "MacSamba"、"Xserve"、"TimeCapsule8,119"。
	Model string `yaml:"model"`
	// AdvertiseTimeMachine 广播 _adisk._tcp（Time Machine 磁盘宣告）。
	AdvertiseTimeMachine bool `yaml:"advertise_time_machine"`
}

// Log 是日志设置。
type Log struct {
	// Level 取值 debug/info/warn/error。
	Level string `yaml:"level"`
	// Format 取值 text/json。
	Format string `yaml:"format"`
	// File 是日志文件路径，留空输出到 stderr。
	File string `yaml:"file"`
}

// 默认值常量。
const (
	DefaultPort       = 445
	DefaultDomain     = "WORKGROUP"
	DefaultLogLevel   = "info"
	DefaultLogFormat  = "text"
	DefaultMaxDialect = "3.1.1"
	DefaultMinDialect = "2.0.2"
	DefaultAppleModel = "MacSamba"
)
