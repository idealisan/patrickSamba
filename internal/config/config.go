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
	// SMB1 控制是否响应 SMB1 多协议协商入口，**默认开启**。
	//
	// 这里说的"SMB1"只是 MS-SMB2 §3.3.5.3.1 的那个协商入口：客户端先发
	// SMB1 `SMB_COM_NEGOTIATE`、方言列表里带 "SMB 2.???"，服务端直接用
	// SMB2 NEGOTIATE Response 回应把它升上 SMB2。
	// **本服务不提供任何 SMB1 文件操作**（没有 trans2、没有 SMB1 读写），
	// 所以 EternalBlue 那一类针对 SMB1 文件操作实现的攻击面在这里为零。
	//
	// 默认开启是为了兼容性：impacket 的 SMBConnection 默认就走
	// negotiateSessionWildcard（先发 SMB1 协商），而它是 AGENTS.md §3
	// 必测客户端矩阵的第 3 项。关掉它这类客户端会直接连不上。
	//
	// 用 *bool 而非 bool：YAML 里分不清"未设置"与"显式 false"，
	// 而本项默认值是 true（与 Share.Browseable 同理）。
	SMB1 *bool `yaml:"smb1"`
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
	//
	// 这个值的用途是**广播**：写进 mDNS 的 _adisk._tcp TXT 记录
	// （ADdF / disk size 字段，阶段二 Time Machine 磁盘宣告），让 Finder
	// 在「选择备份磁盘」界面显示容量。它不改变向 SMB 客户端上报的卷大小。
	TimeMachineMaxSize uint64 `yaml:"time_machine_max_size"`
	// QuotaBytes 限制本共享**向客户端上报的卷容量**（字节），0 表示不限。
	//
	// 主要给 Time Machine 用：macOS 的 Time Machine 会一直备份到把整个卷吃满
	// 为止，真实 NAS 都提供「给 TM 共享设配额」的能力。设了这个值以后，
	// SMB 的 FileFsFullSizeInformation 会按 min(宿主真实剩余, 配额剩余) 上报。
	//
	// 注意这**不是**强制配额：它只影响向客户端上报的数字，不阻止本地写入。
	// 真正的强制配额要靠宿主文件系统，不在本软件职责范围内。
	//
	// 与 TimeMachineMaxSize 的区别：后者只进 mDNS 广播、不参与 SMB 卷容量上报，
	// 且只针对 TM；本字段对所有客户端（不止 TM）生效。两者语义不同、并存。
	QuotaBytes uint64 `yaml:"quota_bytes"`
	// MetadataPath 是 POSIX 元数据旁路存储（纯 Go 嵌入式 KV）的落盘路径。
	//
	// **仅 Windows 使用**（AGENTS.md §5 P7）：NTFS 表达不了 POSIX 的
	// uid/gid/mode，需要旁路存储；Linux/macOS 原生能力足够，该字段留空即可，
	// 填了也会被忽略（会有一条启动 WARN）。
	//
	// 留空时由 vfs 层的 defaultMetadataPath 决定默认落点，config 不替它做决定：
	//   os.UserConfigDir()/stupidsamba/metadata-<fnv32a(root)>.db
	// 即 Windows 上为 %AppData%\stupidsamba\metadata-xxxxxxxx.db
	// （root 路径先 ToLower 再哈希，规避 Windows 路径大小写不敏感导致的重复库）。
	// 默认落点刻意放在共享目录之外，避免客户端在共享里看到这个数据库文件。
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
	// DefaultSMB1Negotiate 是 Server.SMB1 的默认值：开启 SMB1 多协议协商入口。
	// 只是协商入口，不含任何 SMB1 文件操作，见 Server.SMB1 字段注释。
	DefaultSMB1Negotiate = true
)
