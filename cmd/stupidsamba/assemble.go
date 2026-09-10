package main

// 装配层：把 internal/config 的声明式配置翻译成各层需要的运行时对象。
//
// 分层约束（AGENTS.md §5）：`internal/smb/command` 刻意**不 import config**，
// 所以 config.Config → command.Settings 的翻译只能放在 cmd 层做。
// 这里是全项目唯一一处「配置知识」与「协议知识」相遇的地方。

import (
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/config"
	"github.com/finalappstore/stupidsamba/internal/oscap"
	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/dialect"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// newLogger 按配置构造 slog.Logger。
//
// 返回的 io.Closer 在日志写文件时非 nil，调用方退出前应当关闭它。
func newLogger(cfg config.Log) (*slog.Logger, io.Closer, error) {
	level, err := parseLogLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}

	var (
		out    io.Writer = os.Stderr
		closer io.Closer
	)
	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, nil, fmt.Errorf("log.file: 打开日志文件失败: %w", err)
		}
		out, closer = f, f
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "json":
		h = slog.NewJSONHandler(out, opts)
	case "text", "":
		h = slog.NewTextHandler(out, opts)
	default:
		if closer != nil {
			_ = closer.Close()
		}
		return nil, nil, fmt.Errorf("log.format: 无法识别的日志格式 %q（可用值：text / json）", cfg.Format)
	}
	return slog.New(h), closer, nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("log.level: 无法识别的级别 %q（可用值：debug / info / warn / error）", s)
}

// buildAuth 构造认证后端。
//
// 账户只来自配置文件（AGENTS.md §1.1 C8）：不碰 PAM、/etc/passwd、NSS、
// winbind、系统 keyring 或系统 Kerberos 配置。
func buildAuth(cfg *config.Config) (auth.Provider, error) {
	store, err := auth.NewStaticStore(cfg.Auth, cfg.Server.Domain)
	if err != nil {
		return nil, err
	}
	return auth.NewNTLMProvider(auth.Options{
		Store:      store,
		ServerName: cfg.Server.Name,
		DomainName: cfg.Server.Domain,
		// 匿名（null）会话与 guest 共用同一个开关：两者都是「没有有效凭据
		// 也能进」，分开配置只会让运维困惑。
		AllowAnonymous: cfg.Auth.AllowGuest,
	}), nil
}

// listenerInstanceID 由监听配置推导出本服务实例的稳定标识，
// 作为旁路元数据默认落点的实例维度（internal/vfs LocalConfig.InstanceID /
// oscap.Options.InstanceID 的语义见其注释，OI-1）。
//
// 为什么用监听端点：两个服务进程不可能同时 bind 同一个 addr:port，
// 所以端点组合必然区分并存实例；又因为它完全来自配置，同一实例重启后
// 得到同一个值 —— 重启前后复用同一份旁路元数据。
//
// 多个地址按配置顺序用 "," 连接（validate 已拒绝重复地址）；单地址时
// 恰为 "addr:port"。addresses 留空表示监听所有接口，这里用字面量
// "0.0.0.0" 保持确定性（精确到哪一族不影响「区分并存进程」这个用途：
// 两个全默认配置的进程本来就会在 bind 阶段互斥，见 main.go 的启动顺序）。
func listenerInstanceID(l config.Listen) string {
	addrs := l.Addresses
	if len(addrs) == 0 {
		addrs = []string{"0.0.0.0"}
	}
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		parts[i] = net.JoinHostPort(a, strconv.Itoa(l.Port))
	}
	return strings.Join(parts, ",")
}

// buildShares 为每个配置的共享构造 VFS，并追加 IPC$ 管道共享。
//
// 任何一个共享构造失败都会关掉已经建好的 VFS 再返回错误 ——
// 部分成功地启动会让客户端看到一个残缺的共享列表。
func buildShares(cfg *config.Config) ([]*command.Share, error) {
	out := make([]*command.Share, 0, len(cfg.Shares)+1)

	// filesystem_mode 是全局策略，在这里解析一次。
	//
	// 配置校验层（internal/config/validate.go）已经用同一个 ParseMode 拦过非法值，
	// 这里再解析一次是因为**校验结果没有被带下来** —— 校验层只回答"合不合法"，
	// 不产出 oscap.Mode。重复解析而不是让校验层返回，是为了不让 config 包
	// 在类型上依赖 oscap 的枚举（它现在只依赖一个校验函数）。
	mode, err := oscap.ParseMode(cfg.FilesystemMode)
	if err != nil {
		return nil, err
	}

	// 实例标识对全部共享是同一个值：它标识的是**本服务进程**（由监听端点
	// 决定），不是某个共享。放在循环外算一次，避免每个共享重复推导。
	instID := listenerInstanceID(cfg.Listen)

	for i := range cfg.Shares {
		s := &cfg.Shares[i]
		fs, err := vfs.NewLocalFS(vfs.LocalConfig{
			Root:     s.Path,
			ReadOnly: s.ReadOnly,
			// 逐共享探测：同一次运行里 /srv/ext4 可以走 native、
			// /mnt/exfat 落到 builtin（oscap.SelectMatrix 按共享根目录探测）。
			FilesystemMode: mode,
			// SMB 语义要求大小写不敏感查找：Windows 客户端经常用与磁盘上
			// 不同的大小写打开文件（MS-SMB2 §3.3.5.9）。
			CaseInsensitive: true,
			VolumeLabel:     s.Name,
			// UID/GID 留 0：config.Share 不提供属主标签，
			// 且这些数字**不做系统用户解析**（AGENTS.md §1.1 C8）。
			MetadataPath: s.MetadataPath,
			// 本进程的稳定标识：多个进程共享同一共享目录时，
			// 各自的旁路元数据库按它区分，不再互抢 bbolt 文件锁（OI-1）。
			InstanceID: instID,
			// 向客户端上报的卷容量上限（0=不限）；真正生效依赖 vfs.LocalConfig 的对应字段。
			QuotaBytes: s.QuotaBytes,
		})
		if err != nil {
			closeShares(out)
			return nil, fmt.Errorf("shares[%d] %q: %w", i, s.Name, err)
		}
		out = append(out, &command.Share{
			Name:        s.Name,
			Comment:     s.Comment,
			Type:        wire.ShareTypeDisk,
			FS:          fs,
			ReadOnly:    s.ReadOnly,
			GuestOK:     s.GuestOK,
			Browseable:  s.Browseable == nil || *s.Browseable,
			ValidUsers:  s.ValidUsers,
			TimeMachine: s.TimeMachine,
		})
	}

	// IPC$ 是浏览服务器根目录（`\\host`、Finder 的 smb://host）的必需品：
	// 客户端先连 IPC$ 再走 srvsvc 枚举共享。它没有文件系统后端。
	out = append(out, &command.Share{
		Name:    command.IPCShareName,
		Comment: "IPC Service",
		Type:    wire.ShareTypePipe,
		// IPC$ 必须对 guest 开放，否则 guest 连根本无法枚举共享。
		GuestOK: true,
		// 不出现在共享枚举列表里（Windows 也是隐藏的）。
		Browseable: false,
	})

	return out, nil
}

// closeShares 释放共享持有的文件系统资源。
func closeShares(shares []*command.Share) {
	for _, sh := range shares {
		if sh.FS != nil {
			_ = sh.FS.Close()
		}
	}
}

// buildSettings 把配置翻译成协议层设置。
func buildSettings(cfg *config.Config, provider auth.Provider, shares []*command.Share, log *slog.Logger) (*command.Settings, error) {
	minD, err := dialect.Parse(cfg.Server.MinDialect)
	if err != nil {
		return nil, fmt.Errorf("server.min_dialect: %w", err)
	}
	maxD, err := dialect.Parse(cfg.Server.MaxDialect)
	if err != nil {
		return nil, fmt.Errorf("server.max_dialect: %w", err)
	}
	if minD > maxD {
		return nil, fmt.Errorf("server.min_dialect (%s) 高于 server.max_dialect (%s)", minD, maxD)
	}

	guid, err := newServerGUID()
	if err != nil {
		return nil, err
	}

	// 签名算法策略：config 层已校验取值，这里做穷举映射；
	// default 分支按不可达处理（防御未来新增取值忘了同步）。
	var signingPref command.SigningPreference
	switch cfg.Server.SigningAlgorithm {
	case "", config.DefaultSigningAlgorithm:
		signingPref = command.SigningAuto
	case "aes-cmac":
		signingPref = command.SigningPreferAESCMAC
	case "aes-gmac":
		signingPref = command.SigningPreferAESGMAC
	default:
		return nil, fmt.Errorf("server.signing_algorithm: 非法取值 %q（validate 未拦截？）",
			cfg.Server.SigningAlgorithm)
	}

	return &command.Settings{
		ServerName: cfg.Server.Name,
		Domain:     cfg.Server.Domain,
		ServerGUID: guid,
		StartTime:  time.Now(),

		MinDialect: minD,
		MaxDialect: maxD,

		SigningRequired:   cfg.Server.SigningRequired,
		SigningPreference: signingPref,
		// 只要方言区间里有 SMB3 就允许加密；是否强制由配置决定。
		EncryptionEnabled:  maxD.SupportsEncryption(),
		EncryptionRequired: cfg.Server.EncryptionRequired,

		// *bool 默认 true（ApplyDefaults 会填），nil 只可能出现在
		// 绕过 ApplyDefaults 直接构造 Config 的场景，此时按默认值处理。
		AllowSMB1Negotiate: cfg.Server.SMB1 == nil || *cfg.Server.SMB1,
		AllowGuest:         cfg.Auth.AllowGuest,
		// 默认开启（config.Server.OplocksOn）：能开就尽量开。
		// 授予规则本身已经是"安全时才授予"，开不起来也不会有副作用。
		Oplocks: cfg.Server.OplocksOn(),

		// AAPL ModelString 与 mDNS _device-info._tcp 的 model 取同一个配置值，
		// 否则改了配置只有一边跟着变（Finder 图标与宣告不一致）。
		AppleModel: cfg.MDNS.Apple.Model,

		Auth:   provider,
		Shares: shares,
		Logger: log,
	}, nil
}

// newServerGUID 生成 NEGOTIATE Response 里宣告的 ServerGuid
// （MS-SMB2 §3.3.1.5：每次服务启动生成一个新的 GUID）。
func newServerGUID() ([16]byte, error) {
	var g [16]byte
	if _, err := rand.Read(g[:]); err != nil {
		return g, fmt.Errorf("生成 ServerGuid 失败: %w", err)
	}
	return g, nil
}
