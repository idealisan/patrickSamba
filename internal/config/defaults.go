package config

import (
	"os"
	"strings"
)

// ApplyDefaults 为未显式设置的字段填入默认值。
//
// 只在**零值**时填默认，不覆盖用户的显式设置。
// 注意：布尔字段无法区分「未设置」与「显式 false」，
// 因此所有布尔项的默认值都必须是 false（Share.Browseable 例外，用 *bool）。
func ApplyDefaults(c *Config) {
	if c.Server.Name == "" {
		c.Server.Name = defaultServerName()
	}
	// SMB 侧的 NetBIOS/主机名一律大写（MS-SMB2 里主机名大小写不敏感，
	// 但 Windows 习惯显示大写，统一后便于与 NTLM TargetInfo 对齐）。
	c.Server.Name = strings.ToUpper(c.Server.Name)

	if c.Server.Domain == "" {
		c.Server.Domain = DefaultDomain
	}
	c.Server.Domain = strings.ToUpper(c.Server.Domain)

	if c.Server.MinDialect == "" {
		c.Server.MinDialect = DefaultMinDialect
	}
	if c.Server.MaxDialect == "" {
		c.Server.MaxDialect = DefaultMaxDialect
	}

	if c.Listen.Port == 0 {
		c.Listen.Port = DefaultPort
	}

	for i := range c.Shares {
		s := &c.Shares[i]
		if s.Browseable == nil {
			t := true
			s.Browseable = &t
		}
		// Share.MetadataPath 故意不填默认值：它只在 Windows 生效，
		// 落点由 vfs 层的 defaultMetadataPath 自己决定（AGENTS.md §5 P7），
		// config 不替它做决定。默认落点为
		// %AppData%\stupidsamba\metadata-<fnv32a(root)>.db（见 config.go 该字段注释）。
	}

	if c.MDNS.Instance == "" {
		// DNS-SD 实例名默认用服务器名。RFC 6763 §4.1.1 允许任意 UTF-8，
		// 这里保持与 Server.Name 一致，Finder 里显示什么就是什么。
		c.MDNS.Instance = c.Server.Name
	}
	if c.MDNS.Apple.Model == "" {
		c.MDNS.Apple.Model = DefaultAppleModel
	}

	if c.Log.Level == "" {
		c.Log.Level = DefaultLogLevel
	}
	if c.Log.Format == "" {
		c.Log.Format = DefaultLogFormat
	}
}

// defaultServerName 返回系统主机名的第一段（去掉域名部分）。
// 取不到主机名时退回 "STUPIDSAMBA"。
func defaultServerName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "STUPIDSAMBA"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	// NetBIOS 名上限 15 字节（MS-NBTE §2.2.1）。虽然我们不走 139 端口，
	// 但 NTLM TargetInfo 的 NbComputerName 仍沿用该限制。
	if len(h) > 15 {
		h = h[:15]
	}
	return h
}
