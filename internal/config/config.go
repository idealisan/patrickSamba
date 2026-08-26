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

	// FilesystemMode 决定 OS 能力（扩展属性、稀疏文件、命名流、稳定 FileID、
	// 创建时间、DOS 属性）走原生实现还是本项目自带实现（AGENTS.md §1.2 C9）。
	//
	// 两态，默认 auto：
	//   auto     —— 逐项探测宿主能力，能 native 就 native，不能就落到 builtin
	//   portable —— 强制全部走 builtin，完全不碰 OS 的可选能力
	//
	// （"native" 档已在 v0.5 开发版移除：原契约「全部能力原生」在任何平台
	// 都无法满足。旧配置写了 native 会启动报错，请改 auto 或 portable。）
	//
	// 是**全局策略**而非逐共享设置：它表达的是"这台机器上我们信不信任宿主能力"。
	// 各共享的落点仍然逐个决定 —— 同一次运行里 /srv/ext4 可以走 native、
	// /mnt/exfat 落到 builtin，因为探测是按共享根目录做的（oscap.SelectMatrix）。
	//
	// 取值**大小写敏感**：ParseMode 刻意不做 ToLower / trim，
	// 写成 "Auto" 会被拒绝而不是被悄悄纠正。
	FilesystemMode string `yaml:"filesystem_mode"`
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
	// SigningAlgorithm 控制 3.1.1 签名算法协商策略
	// （MS-SMB2 §2.2.3.1.7 SMB2_SIGNING_CAPABILITIES）。
	//
	//	auto      （默认）行为与历史版本完全一致：不回应 SIGNING_CAPABILITIES，
	//	          签名算法按方言默认值（2.x HMAC-SHA256，3.x AES-CMAC）。
	//	          客户端即便宣告支持 AES-GMAC 也**不会**被采用。
	//	aes-cmac  显式选择 AES-CMAC：3.1.1 且客户端宣告了 CMAC 时回应并钉死 CMAC；
	//	          客户端宣告的列表里没有 CMAC 则协商失败（显式失败优于静默降级）。
	//	aes-gmac  选择 AES-GMAC（GCM 硬件路径，比 CMAC 快）：仅当协商到 3.1.1
	//	          且客户端宣告 GMAC 时生效；客户端不支持或协商到更低方言时，
	//          该连接直接协商失败，**绝不静默降级回 CMAC**。
	//          要求 server.max_dialect >= 3.1.1（validate.go 启动校验）。
	//
	// 取值大小写敏感、只接受上述小写拼写（与 filesystem_mode 同规矩）。
	SigningAlgorithm string `yaml:"signing_algorithm"`
	// EncryptionRequired 强制要求 SMB3 加密。
	EncryptionRequired bool `yaml:"encryption_required"`
	// MaxConnections 是并发连接数上限。
	//
	// 0 或不填 = 使用默认上限（256）。**本项不支持"不限"** ——
	// AGENTS.md §8 明确要求并发连接数必须有上限，无上限意味着任何人
	// 都能用建链把服务端的内存与文件描述符吃光。
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
	// QuotaBytes 限制本共享**向客户端上报的卷容量**（字节），0 表示不限。
	//
	// 主要给 Time Machine 用：macOS 的 Time Machine 会一直备份到把整个卷吃满
	// 为止，真实 NAS 都提供「给 TM 共享设配额」的能力。设了这个值以后，
	// SMB 的 FileFsFullSizeInformation 会按 min(宿主真实剩余, 配额剩余) 上报。
	//
	// 注意这**不是**强制配额：它只影响向客户端上报的数字，不阻止本地写入。
	// 真正的强制配额要靠宿主文件系统，不在本软件职责范围内。
	//
	// 这也是限制 Time Machine 备份体积的**唯一**有效手段：
	// macOS 就是照着 SMB 上报的卷容量决定备份磁盘有多大的
	// —— mDNS `_adisk._tcp` 的 TXT 词汇表里没有任何经过验证的容量键
	// （AGENTS.md §9 不许臆造字段值），所以广播那条路走不通。
	QuotaBytes uint64 `yaml:"quota_bytes"`
	// MetadataPath 是元数据旁路存储（纯 Go 嵌入式 KV）的落盘路径。
	//
	// **所有平台都生效**：它既决定 Windows 上 POSIX 属主/权限旁路库
	// （internal/vfs/metadata_windows.go）的位置，也决定 oscap builtin 六项能力
	// 旁路库（internal/oscap/builtin）的位置——后者在 auto/portable 档于任何
	// 平台都会真实创建（PR #159 之后「填了被忽略」就是假话，校验层已按全平台
	// 生效处理，见 validate.go）。
	//
	// 留空时由 vfs 层的 defaultMetadataPath 决定默认落点，config 不替它做决定：
	// 共享根目录的**兄弟**位置，文件名同时编入共享根哈希与服务实例标识
	// （监听 addr:port，见 oscap.Options.InstanceID）——多个服务进程共享同一
	// 共享目录时各开各的库，不会在 bbolt 的 flock 上互相卡死。
	// 显式配置本字段时，**多进程唯一性由配置者自己负责**（两个进程指向同一个
	// 文件，后到的会因 flock 超时启动失败）。默认落点刻意放在共享目录之外，
	// 避免客户端在共享里看到这个数据库文件。
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
	//
	// 默认 **true**（项目所有者 2026-08-26 拍板的默认规则）：没有苹果设备时
	// 这些 TXT 记录对其他客户端没有任何影响，开着方便用 Finder 调试。
	// 用 *bool 是因为要区分「未设置（→true）」与「显式 false」，
	// 同 Server.SMB1 与 Share.Browseable 的做法（见 ApplyDefaults 开头注释）。
	Enabled *bool `yaml:"enabled"`
	// Model 是 _device-info._tcp 的 model= 值，决定 Finder 里显示的图标。
	// 例如 "MacSamba"、"Xserve"、"TimeCapsule8,119"。
	Model string `yaml:"model"`
	// AdvertiseTimeMachine 广播 _adisk._tcp（Time Machine 磁盘宣告）。
	AdvertiseTimeMachine bool `yaml:"advertise_time_machine"`
}

// EnabledOn 返回 Apple 扩展记录的生效取值：未设置时为 DefaultAppleMDNS（true）。
// 供 ApplyDefaults 之外的读取方使用，避免各自解引用 nil 指针。
func (a AppleMDNS) EnabledOn() bool {
	if a.Enabled == nil {
		return DefaultAppleMDNS
	}
	return *a.Enabled
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
	// DefaultSigningAlgorithm 是 Server.SigningAlgorithm 的默认值：
	// auto = 与历史版本行为完全一致（只按方言默认值签名，不协商 GMAC）。
	DefaultSigningAlgorithm = "auto"
	DefaultAppleModel       = "MacSamba"
	// DefaultAppleMDNS 是 MDNS.Apple.Enabled 的默认值：默认开启 Apple 扩展记录。
	// 理由见 AppleMDNS.Enabled 字段注释。与 DefaultFilesystemMode 同理保持 const。
	DefaultAppleMDNS = true
	// DefaultSMB1Negotiate 是 Server.SMB1 的默认值：开启 SMB1 多协议协商入口。
	// 只是协商入口，不含任何 SMB1 文件操作，见 Server.SMB1 字段注释。
	DefaultSMB1Negotiate = true
	// DefaultFilesystemMode 是 Config.FilesystemMode 的默认值。
	//
	// 这里写字面量而不是 oscap.DefaultMode.String()，是为了让它保持 const
	// （配置默认值被谁在运行期改掉是很难查的一类 bug）。两处不漂移由
	// TestDefaultFilesystemModeMatchesOscap 这条测试兜住。
	DefaultFilesystemMode = "auto"
)
