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
	"os"
	"strings"
	"time"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/config"
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

// buildShares 为每个配置的共享构造 VFS，并追加 IPC$ 管道共享。
//
// 任何一个共享构造失败都会关掉已经建好的 VFS 再返回错误 ——
// 部分成功地启动会让客户端看到一个残缺的共享列表。
func buildShares(cfg *config.Config) ([]*command.Share, error) {
	out := make([]*command.Share, 0, len(cfg.Shares)+1)

	for i := range cfg.Shares {
		s := &cfg.Shares[i]
		fs, err := vfs.NewLocalFS(vfs.LocalConfig{
			Root:     s.Path,
			ReadOnly: s.ReadOnly,
			// SMB 语义要求大小写不敏感查找：Windows 客户端经常用与磁盘上
			// 不同的大小写打开文件（MS-SMB2 §3.3.5.9）。
			CaseInsensitive: true,
			VolumeLabel:     s.Name,
			// UID/GID 留 0：config.Share 不提供属主标签，
			// 且这些数字**不做系统用户解析**（AGENTS.md §1.1 C8）。
			MetadataPath: s.MetadataPath,
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

	return &command.Settings{
		ServerName: cfg.Server.Name,
		Domain:     cfg.Server.Domain,
		ServerGUID: guid,
		StartTime:  time.Now(),

		MinDialect: minD,
		MaxDialect: maxD,

		SigningRequired: cfg.Server.SigningRequired,
		// 只要方言区间里有 SMB3 就允许加密；是否强制由配置决定。
		EncryptionEnabled:  maxD.SupportsEncryption(),
		EncryptionRequired: cfg.Server.EncryptionRequired,

		// *bool 默认 true（ApplyDefaults 会填），nil 只可能出现在
		// 绕过 ApplyDefaults 直接构造 Config 的场景，此时按默认值处理。
		AllowSMB1Negotiate: cfg.Server.SMB1 == nil || *cfg.Server.SMB1,
		AllowGuest:         cfg.Auth.AllowGuest,

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
