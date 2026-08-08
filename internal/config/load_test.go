package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadTestdata 读取 testdata 下的配置，把占位符 __SHARE_DIR__ 换成一个真实存在的临时目录，
// 写到临时文件后用 Load 加载（这样也顺带覆盖了文件读取路径）。
func loadTestdata(t *testing.T, name string) (*Config, error) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 testdata/%s: %v", name, err)
	}
	dir := t.TempDir()
	content := strings.ReplaceAll(string(raw), "__SHARE_DIR__", dir)

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时配置: %v", err)
	}
	return Load(path)
}

func TestLoadGood(t *testing.T) {
	c, err := loadTestdata(t, "good.yaml")
	if err != nil {
		t.Fatalf("期望加载成功，实际: %v", err)
	}

	if c.Server.Name != "TESTSRV" {
		t.Errorf("server.name = %q, 期望 TESTSRV", c.Server.Name)
	}
	if c.Listen.Port != 4445 {
		t.Errorf("listen.port = %d, 期望 4445", c.Listen.Port)
	}
	if len(c.Listen.Addresses) != 2 {
		t.Errorf("listen.addresses 长度 = %d, 期望 2", len(c.Listen.Addresses))
	}
	if len(c.Shares) != 2 {
		t.Fatalf("shares 长度 = %d, 期望 2", len(c.Shares))
	}
	if !c.Shares[1].TimeMachine {
		t.Error("shares[1].time_machine 应为 true")
	}
	if len(c.Auth.Users) != 2 || c.Auth.Users[1].NTHash != "31d6cfe0d16ae931b73c59d7e0c089c0" {
		t.Errorf("auth.users 解析错误: %+v", c.Auth.Users)
	}
	if c.Log.Level != "debug" || c.Log.Format != "json" {
		t.Errorf("log = %+v, 期望 debug/json", c.Log)
	}
	// instance 留空应回落到 server.name
	if c.MDNS.Instance != "TESTSRV" {
		t.Errorf("mdns.instance = %q, 期望回落为 TESTSRV", c.MDNS.Instance)
	}
}

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	c, err := loadTestdata(t, "minimal.yaml")
	if err != nil {
		t.Fatalf("期望加载成功，实际: %v", err)
	}

	if c.Listen.Port != DefaultPort {
		t.Errorf("listen.port = %d, 期望默认 %d", c.Listen.Port, DefaultPort)
	}
	if c.Server.Domain != DefaultDomain {
		t.Errorf("server.domain = %q, 期望默认 %q", c.Server.Domain, DefaultDomain)
	}
	if c.Server.MinDialect != DefaultMinDialect || c.Server.MaxDialect != DefaultMaxDialect {
		t.Errorf("方言默认值错误: min=%q max=%q", c.Server.MinDialect, c.Server.MaxDialect)
	}
	if c.Log.Level != DefaultLogLevel || c.Log.Format != DefaultLogFormat {
		t.Errorf("日志默认值错误: %+v", c.Log)
	}
	if c.Server.Name == "" {
		t.Error("server.name 应回落为系统主机名")
	}
	if c.Shares[0].Browseable == nil || !*c.Shares[0].Browseable {
		t.Error("share.browseable 默认应为 true")
	}
	if c.MDNS.Apple.Model != DefaultAppleModel {
		t.Errorf("mdns.apple.model = %q, 期望默认 %q", c.MDNS.Apple.Model, DefaultAppleModel)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := loadTestdata(t, "unknown_field.yaml")
	if err == nil {
		t.Fatal("拼错的配置项必须报错")
	}
	if !strings.Contains(err.Error(), "naem") {
		t.Errorf("错误信息应指出未知字段 naem，实际: %v", err)
	}
}

func TestLoadRejectsBadSyntax(t *testing.T) {
	_, err := loadTestdata(t, "bad_syntax.yaml")
	if err == nil {
		t.Fatal("YAML 语法错误必须报错")
	}
	if !strings.Contains(err.Error(), "解析配置文件失败") {
		t.Errorf("错误信息应说明是解析失败，实际: %v", err)
	}
}

func TestLoadRejectsBadTypes(t *testing.T) {
	_, err := loadTestdata(t, "bad_types.yaml")
	if err == nil {
		t.Fatal("类型不匹配必须报错")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("文件不存在必须报错")
	}
	if !strings.Contains(err.Error(), "读取配置文件失败") {
		t.Errorf("错误信息不够清楚: %v", err)
	}
}

func TestLoadEmptyPath(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("空路径必须报错")
	}
}
