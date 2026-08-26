package config

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
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
	// good.yaml 里显式写了 smb1: false。用 *bool 的全部意义就在这里：
	// 显式 false 必须被保留，不能被"默认 true"覆盖掉。
	if c.Server.SMB1 == nil {
		t.Fatal("server.smb1 显式设为 false 后不应为 nil")
	}
	if *c.Server.SMB1 {
		t.Error("server.smb1 显式 false 被默认值覆盖了")
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
	// SMB1 协商入口默认开启：impacket 等客户端默认先发 SMB1 协商，
	// 默认关掉会让 AGENTS.md §3 必测矩阵里的客户端开箱即用失败。
	if c.Server.SMB1 == nil || !*c.Server.SMB1 {
		t.Errorf("server.smb1 默认应为 true，实际 %v", c.Server.SMB1)
	}
	if c.MDNS.Apple.Model != DefaultAppleModel {
		t.Errorf("mdns.apple.model = %q, 期望默认 %q", c.MDNS.Apple.Model, DefaultAppleModel)
	}
	// Apple 扩展记录默认开启（2026-08-26 项目所有者拍板的默认规则）：
	// 没有苹果设备时这些 TXT 记录对其他客户端没有影响，开着方便调试。
	if c.MDNS.Apple.Enabled == nil || !*c.MDNS.Apple.Enabled {
		t.Errorf("mdns.apple.enabled 默认应为 true，实际 %v", c.MDNS.Apple.Enabled)
	}
}

// TestAppleMDNSDisabledRespected 钉住三态语义：显式 false 不被默认值覆盖。
func TestAppleMDNSDisabledRespected(t *testing.T) {
	c, err := Parse([]byte("auth:\n  users:\n    - name: alice\n      password: x\nshares:\n  - name: public\n    path: " +
		t.TempDir() + "\nmdns:\n  enabled: true\n  apple:\n    enabled: false\n"))
	if err != nil {
		t.Fatalf("期望解析成功，实际: %v", err)
	}
	if c.MDNS.Apple.Enabled == nil || *c.MDNS.Apple.Enabled {
		t.Fatalf("显式 mdns.apple.enabled=false 应当被保留，实际 %v", c.MDNS.Apple.Enabled)
	}
	if c.MDNS.Apple.EnabledOn() {
		t.Error("EnabledOn() 应当返回 false")
	}
}

// TestAppleMDNSEnabledOnNilDefault 钉住未设置时的取值：nil → DefaultAppleMDNS。
// 这是 EnabledOn 的契约，validate 与 mdns 两处消费方都依赖它不炸 nil。
func TestAppleMDNSEnabledOnNilDefault(t *testing.T) {
	var a AppleMDNS
	if a.EnabledOn() != DefaultAppleMDNS {
		t.Errorf("nil 时 EnabledOn() = %v, 期望 %v", a.EnabledOn(), DefaultAppleMDNS)
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

// TestLoadToleratesUTF8BOM 是 v0.5.0 Windows 用户真实事故的回归用例：
// 记事本/PowerShell 保存的 UTF-8 配置常带 BOM，解析器在 [1:1] 报
// "unexpected key name"，用户对着一个看不见的字符毫无办法。
// 修复后 UTF-8 BOM 应当被静默剥掉，配置照常加载。
func TestLoadToleratesUTF8BOM(t *testing.T) {
	c, err := loadTestdata(t, "bom_utf8.yaml")
	if err != nil {
		t.Fatalf("带 UTF-8 BOM 的配置应能加载，实际: %v", err)
	}
	if len(c.Shares) != 1 || c.Shares[0].Name != "public" {
		t.Errorf("shares 解析错误: %+v", c.Shares)
	}
}

// 同一份语义钉在 Decode 层：BOM 剥离必须发生在任何入口（Load/Parse/Decode）之前。
func TestDecodeToleratesUTF8BOM(t *testing.T) {
	bom := append([]byte{0xEF, 0xBB, 0xBF},
		[]byte("auth:\n  users:\n    - name: alice\n      password: x\nshares:\n  - name: public\n    path: "+t.TempDir()+"\n")...)
	c, err := Parse(bom)
	if err != nil {
		t.Fatalf("Parse 应容忍 UTF-8 BOM，实际: %v", err)
	}
	if c.Auth.Users[0].Name != "alice" {
		t.Errorf("auth.users[0].name = %q, 期望 alice", c.Auth.Users[0].Name)
	}
}

// UTF-16 没法只剥个 BOM 就当 UTF-8 解，必须给人话报错而不是一屏乱码。
func TestLoadRejectsUTF16WithFriendlyError(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "minimal.yaml"))
	if err != nil {
		t.Fatalf("读取 testdata/minimal.yaml: %v", err)
	}
	u16 := utf16.Encode([]rune(string(raw)))
	data := make([]byte, 2+2*len(u16))
	data[0], data[1] = 0xFF, 0xFE // UTF-16 LE BOM
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(data[2+2*i:], v)
	}

	path := filepath.Join(t.TempDir(), "utf16.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写临时配置: %v", err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("UTF-16 编码的配置必须报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "UTF-16") || !strings.Contains(msg, "UTF-8") {
		t.Errorf("错误信息应指出编码问题并给出改法，实际: %v", msg)
	}
}

// TestLoadReportsEveryFieldPath 是「人话错误信息」的验收用例（AGENTS.md §6）：
// 一份到处写错的配置必须一次性报出全部问题，且每条都指出出错的字段路径，
// 而不是让用户改一个跑一次。
func TestLoadReportsEveryFieldPath(t *testing.T) {
	_, err := loadTestdata(t, "many_errors.yaml")
	if err == nil {
		t.Fatal("这份配置到处都是错，必须报错")
	}
	msg := err.Error()

	wantFields := []string{
		"server.name",
		"server.min_dialect",
		"listen.addresses[0]",
		"listen.addresses[2]",
		"listen.port",
		"auth.users[0].nt_hash",
		"auth.users[1].name",
		"shares[0].valid_users[1]",
		"shares[1].name",
		"shares[1].path",
		"log.level",
		"log.format",
	}
	for _, f := range wantFields {
		if !strings.Contains(msg, f) {
			t.Errorf("错误信息里缺少字段路径 %q", f)
		}
	}
	if t.Failed() {
		t.Logf("实际错误信息:\n%s", msg)
	}
}
