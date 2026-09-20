package auth

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/idealisan/patrickSamba/internal/config"
)

func TestStaticStorePasswordAndNTHash(t *testing.T) {
	// 同一个口令，一个写明文一个写 nt_hash，得到的 NTHash 必须一致。
	want := NTHash("Password")
	s, err := NewStaticStore(config.Auth{Users: []config.User{
		{Name: "plain", Password: "Password"},
		{Name: "hashed", NTHash: hex.EncodeToString(want[:])},
		// 两者都给时优先 nt_hash。
		{Name: "both", Password: "something-else", NTHash: hex.EncodeToString(want[:])},
	}}, "WORKGROUP")
	if err != nil {
		t.Fatalf("构造: %v", err)
	}
	if s.UserCount() != 3 {
		t.Errorf("UserCount = %d", s.UserCount())
	}

	for _, name := range []string{"plain", "hashed", "both"} {
		a, err := s.Lookup(name, "")
		if err != nil {
			t.Fatalf("查 %s: %v", name, err)
		}
		if a.NTHash != want {
			t.Errorf("%s 的 NTHash = %x, want %x", name, a.NTHash, want)
		}
		if a.Domain != "WORKGROUP" {
			t.Errorf("%s 的 Domain = %q", name, a.Domain)
		}
	}
}

// 用户名查找大小写不敏感（Windows 语义），但原始大小写要保留。
func TestStaticStoreCaseInsensitive(t *testing.T) {
	s, err := NewStaticStore(config.Auth{Users: []config.User{
		{Name: "Alice", Password: "x"},
	}}, "WG")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"Alice", "alice", "ALICE", "aLiCe"} {
		a, err := s.Lookup(q, "")
		if err != nil {
			t.Errorf("查 %q 失败: %v", q, err)
			continue
		}
		if a.User != "Alice" {
			t.Errorf("User = %q, want 原始大小写 Alice", a.User)
		}
	}
	if _, err := s.Lookup("bob", ""); err != ErrNoSuchUser {
		t.Errorf("不存在的用户 err = %v, want ErrNoSuchUser", err)
	}
}

func TestStaticStoreAllowGuest(t *testing.T) {
	off, _ := NewStaticStore(config.Auth{}, "WG")
	if off.AllowGuest() {
		t.Error("默认不应允许 guest")
	}
	on, _ := NewStaticStore(config.Auth{AllowGuest: true}, "WG")
	if !on.AllowGuest() {
		t.Error("配置开了 guest")
	}
}

// 配置错误必须给出人话错误信息，并指出是哪一条。
func TestStaticStoreConfigErrors(t *testing.T) {
	cases := []struct {
		name  string
		users []config.User
		want  string
	}{
		{"空用户名", []config.User{{Password: "x"}}, "name 不能为空"},
		{"缺凭据", []config.User{{Name: "a"}}, "必须设置 password 或 nt_hash"},
		{"重复用户名", []config.User{
			{Name: "Alice", Password: "x"}, {Name: "alice", Password: "y"},
		}, "重复"},
		{"nt_hash 长度不对", []config.User{{Name: "a", NTHash: "abcd"}}, "32 位十六进制"},
		{"nt_hash 非法字符", []config.User{
			{Name: "a", NTHash: strings.Repeat("z", 32)},
		}, "不是合法的十六进制"},
	}
	for _, c := range cases {
		_, err := NewStaticStore(config.Auth{Users: c.users}, "WG")
		if err == nil {
			t.Errorf("%s: 应当报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %q, 应包含 %q", c.name, err, c.want)
		}
		// 错误信息必须指出是第几条 auth.users。
		if !strings.Contains(err.Error(), "auth.users[") {
			t.Errorf("%s: 错误信息未指出位置: %q", c.name, err)
		}
	}
}

// 口令绝不能出现在任何可导出的地方 —— Account 里只有 NTHash。
func TestStaticStoreDoesNotKeepPlaintext(t *testing.T) {
	const pw = "correct-horse-battery-staple"
	s, err := NewStaticStore(config.Auth{Users: []config.User{
		{Name: "a", Password: pw},
	}}, "WG")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Lookup("a", "")
	if strings.Contains(a.User, pw) || strings.Contains(a.Domain, pw) {
		t.Fatal("明文口令泄漏进了 Account")
	}
	if a.NTHash != NTHash(pw) {
		t.Error("NTHash 不正确")
	}
}
