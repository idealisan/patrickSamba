package auth

// static AccountStore —— 账户只来自本软件的 YAML 配置。
//
// AGENTS.md C8：**绝不**读取 /etc/passwd、/etc/shadow、/etc/group，
// 绝不调用 os/user 的查询语义、PAM、NSS、SSPI、OpenDirectory 或任何系统 keyring。
// 本文件唯一的数据来源是 config.Auth。

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/idealisan/patrickSamba/internal/config"
)

// StaticStore 是基于配置文件的静态账户表，并发安全（构造后只读）。
type StaticStore struct {
	// users 的 key 是小写用户名（Windows 语义下用户名大小写不敏感）。
	users      map[string]*Account
	allowGuest bool
}

var _ AccountStore = (*StaticStore)(nil)

// NewStaticStore 从配置构造账户表。
//
// 每个用户必须提供 password 或 nt_hash 之一；两者都给时**优先 nt_hash**
// （配置里就不必存明文口令）。
//
// domain 是服务端工作组名，写入每个 Account，NTOWFv2 计算时会用到。
func NewStaticStore(cfg config.Auth, domain string) (*StaticStore, error) {
	s := &StaticStore{
		users:      make(map[string]*Account, len(cfg.Users)),
		allowGuest: cfg.AllowGuest,
	}
	for i, u := range cfg.Users {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			return nil, fmt.Errorf("auth.users[%d]: name 不能为空", i)
		}
		key := strings.ToLower(name)
		if _, dup := s.users[key]; dup {
			return nil, fmt.Errorf("auth.users[%d]: 用户名 %q 重复（大小写不敏感）", i, name)
		}

		acct := &Account{User: name, Domain: domain}
		switch {
		case u.NTHash != "":
			h, err := parseNTHash(u.NTHash)
			if err != nil {
				return nil, fmt.Errorf("auth.users[%d] (%s): nt_hash %w", i, name, err)
			}
			acct.NTHash = h
		case u.Password != "":
			acct.NTHash = NTHash(u.Password)
		default:
			return nil, fmt.Errorf("auth.users[%d] (%s): 必须设置 password 或 nt_hash 之一", i, name)
		}
		s.users[key] = acct
	}
	return s, nil
}

// parseNTHash 解析 32 位十六进制的 NT hash。
func parseNTHash(s string) ([16]byte, error) {
	var out [16]byte
	s = strings.TrimSpace(s)
	if len(s) != 32 {
		return out, fmt.Errorf("必须是 32 位十六进制字符（当前 %d 位）", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, fmt.Errorf("不是合法的十六进制: %w", err)
	}
	copy(out[:], b)
	return out, nil
}

// Lookup 按用户名查账户（大小写不敏感）。domain 参数不参与匹配 ——
// 本软件不是域控，客户端送来的域名只用于 NTOWFv2 计算。
func (s *StaticStore) Lookup(user, _ string) (*Account, error) {
	a, ok := s.users[strings.ToLower(user)]
	if !ok {
		return nil, ErrNoSuchUser
	}
	return a, nil
}

// AllowGuest 报告口令校验失败时是否允许降级为 guest。
func (s *StaticStore) AllowGuest() bool { return s.allowGuest }

// UserCount 返回账户条数（供启动日志使用，不泄露用户名）。
func (s *StaticStore) UserCount() int { return len(s.users) }
