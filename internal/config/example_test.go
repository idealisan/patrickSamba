package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exampleConfigPath 是随发行一起提供的示例配置。
const exampleConfigPath = "../../configs/example.yaml"

// TestExampleConfigIsValid 保证 configs/example.yaml **照抄就能用**。
//
// 这条测试是有来历的：示例里曾经写着
//
//	addresses:
//	  - 0.0.0.0
//	  - "::"
//
// 照抄的人一启动就 bind 失败 —— Go 的 tcp 监听是双栈的，先绑 "::" 就已经
// 把 0.0.0.0 占了，第二个 net.Listen 必然 EADDRINUSE。示例配置是大多数人
// 接触本项目的第一份文件，它错了等于开箱即坏，所以必须进 CI。
//
// 共享目录会被替换成临时目录：我们要校验的是**配置语义**（监听地址、
// 用户引用、mDNS 开关组合……），而不是构建机上有没有 /srv/share。
func TestExampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile(exampleConfigPath)
	if err != nil {
		t.Fatalf("读取示例配置: %v", err)
	}

	dir := t.TempDir()
	for _, name := range []string{"public", "docs", "private", "backup"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	content := strings.ReplaceAll(string(raw), "/srv/share", dir)

	// 走完整的 Parse：Decode（严格模式，未知字段报错）+ ApplyDefaults + Validate。
	cfg, err := Parse([]byte(content))
	if err != nil {
		t.Fatalf("示例配置校验不通过 —— 照抄它的人会直接起不来:\n%v", err)
	}

	// 示例的价值在于「覆盖面」，退化成三行小配置就失去意义了。
	if len(cfg.Shares) < 3 {
		t.Errorf("示例只有 %d 个共享，应当展示多共享用法", len(cfg.Shares))
	}
	if len(cfg.Auth.Users) < 2 {
		t.Errorf("示例只有 %d 个用户，应当同时展示 password 与 nt_hash 两种写法", len(cfg.Auth.Users))
	}
	if !cfg.MDNS.Enabled || !cfg.MDNS.Apple.Enabled {
		t.Error("示例应当展示 mDNS 与 Apple 扩展的开启写法")
	}
}

// TestExampleConfigListenAddressesActuallyBind 是上一条的加强版：
// 光通过校验还不够，示例里的监听地址必须真的能同时绑上。
//
// 端口用 0（内核分配）以免和构建机上任何东西冲突；这里验的是
// 「这一组地址能不能共存」，与具体端口号无关。
func TestExampleConfigListenAddressesActuallyBind(t *testing.T) {
	raw, err := os.ReadFile(exampleConfigPath)
	if err != nil {
		t.Fatalf("读取示例配置: %v", err)
	}
	dir := t.TempDir()
	content := strings.ReplaceAll(string(raw), "/srv/share", dir)

	cfg, err := Decode([]byte(content))
	if err != nil {
		t.Fatalf("解析示例配置: %v", err)
	}
	if len(cfg.Listen.Addresses) == 0 {
		t.Skip("示例监听全部地址，没有需要检查共存性的地址列表")
	}
	assertAddressesCoexist(t, cfg.Listen.Addresses)
}

// assertAddressesCoexist 断言这一组地址能在**同一个端口**上同时监听成功。
//
// 复刻 internal/server.Listen 的做法：逐个 net.Listen("tcp", ip:port)。
// 端口取第一个监听器拿到的内核分配端口，后续地址复用它 —— 这正是真实启动
// 时的情形，也是 0.0.0.0 与 :: 并列会翻车的地方。
func assertAddressesCoexist(t *testing.T, addrs []string) {
	t.Helper()

	var (
		ls   []net.Listener
		port string
	)
	defer func() {
		for _, l := range ls {
			_ = l.Close()
		}
	}()

	for i, a := range addrs {
		if port == "" {
			port = "0"
		}
		l, err := net.Listen("tcp", net.JoinHostPort(a, port))
		if err != nil {
			// 构建环境可能没有 IPv6，那是环境限制不是配置错误。
			if i == 0 && isUnsupportedAddrErr(err) {
				t.Skipf("跳过：本机不支持监听 %s: %v", a, err)
			}
			t.Fatalf("示例配置的 listen.addresses 无法共存：绑定第 %d 项 %q 失败: %v\n"+
				"（完整列表 %v。注意 Go 的 tcp 监听是双栈的，0.0.0.0 与 :: 会互相抢端口）",
				i, a, err, addrs)
		}
		ls = append(ls, l)
		if port == "0" {
			_, p, err := net.SplitHostPort(l.Addr().String())
			if err != nil {
				t.Fatalf("解析监听地址 %q: %v", l.Addr(), err)
			}
			port = p
		}
	}
}

// isUnsupportedAddrErr 判断错误是否为「本机不支持这个地址族」。
func isUnsupportedAddrErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "cannot assign requested address") ||
		strings.Contains(s, "address family not supported") ||
		strings.Contains(s, "protocol not supported")
}
