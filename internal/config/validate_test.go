package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseConfig 返回一个通过校验的最小配置，测试在此基础上单点破坏。
func baseConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	c := &Config{
		Shares: []Share{{Name: "public", Path: dir}},
		Auth: Auth{
			Users: []User{{Name: "alice", Password: "changeme"}},
		},
	}
	ApplyDefaults(c)
	if err := Validate(c); err != nil {
		t.Fatalf("基准配置本身就不合法: %v", err)
	}
	return c
}

// boolPtr 是 *bool 配置项（smb1/browseable/apple.enabled）测试用的取址助手。
func boolPtr(b bool) *bool { return &b }

// assertInvalid 断言校验失败，且错误信息里出现了所有期望的关键字。
func assertInvalid(t *testing.T, c *Config, wants ...string) {
	t.Helper()
	err := Validate(c)
	if err == nil {
		t.Fatalf("期望校验失败，实际通过。配置: %+v", c)
	}
	if _, ok := err.(ValidationErrors); !ok {
		t.Fatalf("错误类型应为 ValidationErrors，实际 %T", err)
	}
	msg := err.Error()
	for _, w := range wants {
		if !strings.Contains(msg, w) {
			t.Errorf("错误信息缺少 %q，实际:\n%s", w, msg)
		}
	}
}

func TestValidateSharesRequired(t *testing.T) {
	c := baseConfig(t)
	c.Shares = nil
	assertInvalid(t, c, "shares", "至少要配置一个共享")
}

func TestValidateShareNameEmpty(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].Name = ""
	assertInvalid(t, c, "shares[0].name")
}

func TestValidateShareNameReservedIPC(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].Name = "IPC$"
	assertInvalid(t, c, "shares[0].name", "保留共享名")
}

func TestValidateShareNameInvalidChars(t *testing.T) {
	for _, name := range []string{`a\b`, "a/b", "a:b", "a*b", "a?b", `a"b`, "a<b", "a|b"} {
		c := baseConfig(t)
		c.Shares[0].Name = name
		assertInvalid(t, c, "shares[0].name", "非法字符")
	}
}

func TestValidateShareNameControlChar(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].Name = "a\tb"
	assertInvalid(t, c, "shares[0].name", "控制字符")
}

func TestValidateShareNameTooLong(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].Name = strings.Repeat("x", ShareNameMaxLen+1)
	assertInvalid(t, c, "shares[0].name", "超过上限")
}

func TestValidateShareNameDuplicateCaseInsensitive(t *testing.T) {
	c := baseConfig(t)
	c.Shares = append(c.Shares, Share{Name: "PUBLIC", Path: c.Shares[0].Path})
	assertInvalid(t, c, "shares[1].name", "重复")
}

func TestValidateSharePathRelative(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].Path = "relative/dir"
	assertInvalid(t, c, "shares[0].path", "绝对路径")
}

func TestValidateSharePathMissing(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].Path = filepath.Join(t.TempDir(), "does-not-exist")
	assertInvalid(t, c, "shares[0].path", "不存在")
}

func TestValidateSharePathNotDir(t *testing.T) {
	c := baseConfig(t)
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Shares[0].Path = f
	assertInvalid(t, c, "shares[0].path", "不是目录")
}

func TestValidateSharePathEmpty(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].Path = ""
	assertInvalid(t, c, "shares[0].path")
}

func TestValidateTimeMachineReadOnly(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].TimeMachine = true
	c.Shares[0].ReadOnly = true
	assertInvalid(t, c, "shares[0].time_machine", "只读")
}

func TestValidateListenAddress(t *testing.T) {
	c := baseConfig(t)
	c.Listen.Addresses = []string{"127.0.0.1", "not-an-ip"}
	assertInvalid(t, c, "listen.addresses[1]", "合法 IP")
}

func TestValidateListenPortRange(t *testing.T) {
	for _, p := range []int{-1, 0, 65536, 70000} {
		c := baseConfig(t)
		c.Listen.Port = p
		assertInvalid(t, c, "listen.port", "1-65535")
	}
}

func TestValidateListenAddressDuplicate(t *testing.T) {
	c := baseConfig(t)
	c.Listen.Addresses = []string{"192.0.2.1", "192.0.2.1"}
	assertInvalid(t, c, "listen.addresses[1]", "重复")

	// 同一地址的不同写法也算重复。
	c = baseConfig(t)
	c.Listen.Addresses = []string{"::1", "0:0:0:0:0:0:0:1"}
	assertInvalid(t, c, "listen.addresses[1]", "重复")
}

// 通配地址与具体地址并列会 bind 失败（实测：EADDRINUSE），
// 同时写 0.0.0.0 与 :: 也一样 —— Go 的 tcp 监听是双栈的。
func TestValidateListenWildcardConflict(t *testing.T) {
	c := baseConfig(t)
	c.Listen.Addresses = []string{"0.0.0.0", "::"}
	assertInvalid(t, c, "listen.addresses[0]", "双栈")

	c = baseConfig(t)
	c.Listen.Addresses = []string{"0.0.0.0", "127.0.0.1"}
	assertInvalid(t, c, "listen.addresses[0]", "通配地址")

	// 单独一个通配地址是合法的。
	c = baseConfig(t)
	c.Listen.Addresses = []string{"::"}
	if err := Validate(c); err != nil {
		t.Fatalf("单个通配地址应当合法，实际: %v", err)
	}
}

func TestValidateValidUsersEmptyAndDuplicate(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].ValidUsers = []string{""}
	assertInvalid(t, c, "shares[0].valid_users[0]", "空条目")

	c = baseConfig(t)
	c.Shares[0].ValidUsers = []string{"alice", "ALICE"}
	assertInvalid(t, c, "shares[0].valid_users[1]", "重复")
}

func TestValidateLogFileDirMissing(t *testing.T) {
	c := baseConfig(t)
	c.Log.File = filepath.Join(t.TempDir(), "no-such-dir", "smb.log")
	assertInvalid(t, c, "log.file", "目录不存在")

	// 目录存在时合法（文件本身可以还不存在）。
	c = baseConfig(t)
	c.Log.File = filepath.Join(t.TempDir(), "smb.log")
	if err := Validate(c); err != nil {
		t.Fatalf("目录存在时应当合法，实际: %v", err)
	}
}

// 要求加密时 min_dialect 低于 3.0 必须**启动报错**（不是告警、更不是悄悄抬高）。
//
// 2.0.2/2.1 没有加密能力，协商阶段会被 fail closed 拒掉，
// 用户在运行期只看到"连不上"，猜不到是这个组合导致的。
func TestEncryptionRequiredRejectsLowMinDialect(t *testing.T) {
	c := baseConfig(t)
	c.Server.EncryptionRequired = true
	c.Server.MinDialect = "2.0.2"
	c.Server.MaxDialect = "3.1.1"
	// 错误里必须带字段路径，并直接告诉用户改成什么。
	assertInvalid(t, c, "server.min_dialect", "不能低于 3.0")

	c = baseConfig(t)
	c.Server.EncryptionRequired = true
	c.Server.MinDialect = "2.1"
	c.Server.MaxDialect = "3.1.1"
	assertInvalid(t, c, "server.min_dialect", "encryption_required")

	// 抬到 3.0 之后就合法了。
	c = baseConfig(t)
	c.Server.EncryptionRequired = true
	c.Server.MinDialect = "3.0"
	c.Server.MaxDialect = "3.1.1"
	if err := Validate(c); err != nil {
		t.Fatalf("min_dialect=3.0 + 要求加密应当合法，实际: %v", err)
	}

	// 不要求加密时，低 min_dialect 一切照旧。
	c = baseConfig(t)
	c.Server.MinDialect = "2.0.2"
	if err := Validate(c); err != nil {
		t.Fatalf("不要求加密时 min_dialect=2.0.2 应当合法，实际: %v", err)
	}
}

// 默认 min_dialect 是 2.0.2，所以「只写 encryption_required: true」会直接
// 启动失败 —— 这是有意为之的 fail fast，但错误必须让人一眼知道怎么办。
func TestEncryptionRequiredWithDefaultMinDialectFailsLoudly(t *testing.T) {
	c := baseConfig(t)
	c.Server.MinDialect = DefaultMinDialect
	c.Server.MaxDialect = DefaultMaxDialect
	c.Server.EncryptionRequired = true

	err := Validate(c)
	if err == nil {
		t.Fatal("默认 min_dialect 低于 3.0，只开 encryption_required 应当启动失败")
	}
	msg := err.Error()
	for _, want := range []string{"server.min_dialect", "3.0", "encryption_required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺少 %q，无法指导用户修改:\n%s", want, msg)
		}
	}
}

func TestWarningsSharePathOverlap(t *testing.T) {
	c := baseConfig(t)
	c.Shares = append(c.Shares, Share{Name: "mirror", Path: c.Shares[0].Path})
	if !strings.Contains(strings.Join(Warnings(c), "\n"), "同一个目录") {
		t.Error("两个共享指向同一目录时应告警")
	}

	// 只读共享嵌在可写共享里 = 只读形同虚设，必须点破。
	c = baseConfig(t)
	inner := filepath.Join(c.Shares[0].Path, "sub")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	c.Shares = append(c.Shares, Share{Name: "inner", Path: inner, ReadOnly: true})
	ws := strings.Join(Warnings(c), "\n")
	if !strings.Contains(ws, "位于") {
		t.Errorf("嵌套共享应告警，实际:\n%s", ws)
	}
	if !strings.Contains(ws, "绕过只读限制") {
		t.Errorf("只读共享套在可写共享内应特别点破，实际:\n%s", ws)
	}
}

func TestValidateDialects(t *testing.T) {
	c := baseConfig(t)
	c.Server.MinDialect = "1.0"
	assertInvalid(t, c, "server.min_dialect", "非法方言")

	c = baseConfig(t)
	c.Server.MaxDialect = "9.9.9"
	assertInvalid(t, c, "server.max_dialect", "非法方言")

	c = baseConfig(t)
	c.Server.MinDialect = "3.1.1"
	c.Server.MaxDialect = "2.0.2"
	assertInvalid(t, c, "server.min_dialect", "不能高于")
}

func TestValidateEncryptionNeedsSMB3(t *testing.T) {
	c := baseConfig(t)
	c.Server.EncryptionRequired = true
	c.Server.MaxDialect = "2.1"
	assertInvalid(t, c, "server.encryption_required", "3.0")
}

func TestValidateServerNameTooLong(t *testing.T) {
	c := baseConfig(t)
	c.Server.Name = strings.Repeat("X", 16)
	assertInvalid(t, c, "server.name", "NetBIOS")
}

func TestValidateNoLoginPossible(t *testing.T) {
	c := baseConfig(t)
	c.Auth.Users = nil
	c.Auth.AllowGuest = false
	assertInvalid(t, c, "auth", "没有人能登录")
}

func TestValidateGuestOnlyIsAllowed(t *testing.T) {
	c := baseConfig(t)
	c.Auth.Users = nil
	c.Auth.AllowGuest = true
	if err := Validate(c); err != nil {
		t.Fatalf("允许 guest 时无用户应当合法，实际: %v", err)
	}
}

func TestValidateUserCredentials(t *testing.T) {
	c := baseConfig(t)
	c.Auth.Users[0].Password = ""
	c.Auth.Users[0].NTHash = ""
	assertInvalid(t, c, "auth.users[0]", "password 或 nt_hash")
}

func TestValidateUserNTHash(t *testing.T) {
	for _, h := range []string{"deadbeef", strings.Repeat("z", 32), strings.Repeat("0", 31)} {
		c := baseConfig(t)
		c.Auth.Users[0].Password = ""
		c.Auth.Users[0].NTHash = h
		assertInvalid(t, c, "auth.users[0].nt_hash", "32 位十六进制")
	}

	c := baseConfig(t)
	c.Auth.Users[0].Password = ""
	c.Auth.Users[0].NTHash = "31D6CFE0D16AE931B73C59D7E0C089C0" // 大写 hex 也合法
	if err := Validate(c); err != nil {
		t.Fatalf("大写十六进制 nt_hash 应当合法，实际: %v", err)
	}
}

func TestValidateUserDuplicate(t *testing.T) {
	c := baseConfig(t)
	c.Auth.Users = append(c.Auth.Users, User{Name: "ALICE", Password: "x"})
	assertInvalid(t, c, "auth.users[1].name", "重复")
}

func TestValidateUserReservedGuest(t *testing.T) {
	c := baseConfig(t)
	c.Auth.Users[0].Name = "guest"
	assertInvalid(t, c, "auth.users[0].name", "保留用户名")
}

func TestValidateValidUsersUnknown(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].ValidUsers = []string{"alice", "nobody"}
	assertInvalid(t, c, "shares[0].valid_users[1]", "未定义的用户")
}

func TestValidateMDNSInstanceTooLong(t *testing.T) {
	c := baseConfig(t)
	c.MDNS.Enabled = true
	c.MDNS.Instance = strings.Repeat("x", 64)
	assertInvalid(t, c, "mdns.instance", "63")
}

func TestValidateMDNSInterfaceUnknown(t *testing.T) {
	c := baseConfig(t)
	c.MDNS.Enabled = true
	c.MDNS.Interfaces = []string{"no-such-iface0"}
	assertInvalid(t, c, "mdns.interfaces[0]", "找不到网卡")
}

func TestValidateMDNSDisabledSkipsChecks(t *testing.T) {
	c := baseConfig(t)
	c.MDNS.Enabled = false
	c.MDNS.Interfaces = []string{"no-such-iface0"}
	if err := Validate(c); err != nil {
		t.Fatalf("mdns 关闭时不应校验其子项，实际: %v", err)
	}
}

func TestValidateTimeMachineAdvertiseNeedsShare(t *testing.T) {
	c := baseConfig(t)
	c.MDNS.Enabled = true
	c.MDNS.Apple.Enabled = boolPtr(true)
	c.MDNS.Apple.AdvertiseTimeMachine = true
	assertInvalid(t, c, "mdns.apple.advertise_time_machine", "time_machine: true")

	c.Shares[0].TimeMachine = true
	if err := Validate(c); err != nil {
		t.Fatalf("有 Time Machine 共享后应当合法，实际: %v", err)
	}
}

func TestValidateTimeMachineAdvertiseNeedsApple(t *testing.T) {
	c := baseConfig(t)
	c.MDNS.Enabled = true
	c.MDNS.Apple.Enabled = boolPtr(false)
	c.MDNS.Apple.AdvertiseTimeMachine = true
	c.Shares[0].TimeMachine = true
	assertInvalid(t, c, "mdns.apple.advertise_time_machine", "mdns.apple.enabled")
}

func TestValidateLog(t *testing.T) {
	c := baseConfig(t)
	c.Log.Level = "verbose"
	assertInvalid(t, c, "log.level", "debug, info, warn, error")

	c = baseConfig(t)
	c.Log.Format = "xml"
	assertInvalid(t, c, "log.format", "text, json")

	c = baseConfig(t)
	c.Log.File = "relative.log"
	assertInvalid(t, c, "log.file", "绝对路径")
}

// 多处错误应当一次性全部报出来，而不是报一个就停。
func TestValidateReportsAllErrors(t *testing.T) {
	c := baseConfig(t)
	c.Listen.Port = 0
	c.Log.Level = "nope"
	c.Shares[0].Name = "IPC$"

	err := Validate(c)
	if err == nil {
		t.Fatal("期望校验失败")
	}
	ve, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("错误类型应为 ValidationErrors，实际 %T", err)
	}
	if len(ve) < 3 {
		t.Errorf("期望至少报出 3 处问题，实际 %d: %v", len(ve), err)
	}
	if !strings.Contains(err.Error(), "共 ") {
		t.Errorf("多错误时应汇总条数，实际: %v", err)
	}
}

func TestWarnings(t *testing.T) {
	c := baseConfig(t)
	c.Auth.AllowGuest = true
	c.Listen.Port = 445

	ws := strings.Join(Warnings(c), "\n")
	if !strings.Contains(ws, "allow_guest") {
		t.Errorf("开启 guest 必须告警，实际:\n%s", ws)
	}
	if !strings.Contains(ws, "明文口令") {
		t.Errorf("明文口令应告警，实际:\n%s", ws)
	}
	if os.Geteuid() != 0 && !strings.Contains(ws, "cap_net_bind_service") {
		t.Errorf("非 root 下特权端口应告警，实际:\n%s", ws)
	}
}

func TestWarningsGuestOKWithoutAllowGuest(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].GuestOK = true
	c.Auth.AllowGuest = false
	if !strings.Contains(strings.Join(Warnings(c), "\n"), "guest_ok") {
		t.Error("guest_ok 与 allow_guest 冲突时应告警")
	}
}

// TestWarningsDemoAccount 钉住镜像内置演示账号的告警：公开凭据等于匿名，
// 用户必须在启动日志里第一眼看到（configs/docker.yaml 的 stupidsamba/stupidsamba）。
func TestWarningsDemoAccount(t *testing.T) {
	c := baseConfig(t)
	// 基准配置里是 alice/changeme，不应触发演示账号告警。
	if ws := strings.Join(Warnings(c), "\n"); strings.Contains(ws, "演示账号") {
		t.Errorf("非演示账号不应触发告警，实际:\n%s", ws)
	}
	c.Auth.Users[0] = User{Name: "stupidsamba", Password: "stupidsamba"}
	ws := strings.Join(Warnings(c), "\n")
	if !strings.Contains(ws, "演示账号") || !strings.Contains(ws, "公开凭据") {
		t.Errorf("内置演示账号必须告警，实际:\n%s", ws)
	}
	// 只换掉口令就不再是那个公开凭据，不应误伤同名用户。
	c.Auth.Users[0].Password = "my-own-password"
	if ws := strings.Join(Warnings(c), "\n"); strings.Contains(ws, "公开凭据") {
		t.Errorf("同名但口令已改的用户不应触发演示账号告警，实际:\n%s", ws)
	}
}

func TestDialectNamesIsCopy(t *testing.T) {
	a := DialectNames()
	a[0] = "tampered"
	if DialectNames()[0] != "2.0.2" {
		t.Error("DialectNames 必须返回副本，避免外部改坏内部状态")
	}
}
