package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// TestFilesystemModeDefaultsToAuto：不写这一项时必须落到 auto。
func TestFilesystemModeDefaultsToAuto(t *testing.T) {
	c := &Config{}
	ApplyDefaults(c)
	if c.FilesystemMode != "auto" {
		t.Errorf("默认 filesystem_mode = %q，应为 auto", c.FilesystemMode)
	}
}

// TestFilesystemModeDefaultDoesNotOverride：显式设置不能被默认值覆盖。
//
// 这是 ApplyDefaults 那条"只在零值时填"约定的负向对照 —— 少了它，
// 一个写反的 if 会让所有人的 portable 配置被静默改回 auto，
// 而症状是"配了 portable 却还是走了 native"，几乎不可能从日志看出来。
func TestFilesystemModeDefaultDoesNotOverride(t *testing.T) {
	for _, m := range oscap.ModeNames() {
		c := &Config{FilesystemMode: m}
		ApplyDefaults(c)
		if c.FilesystemMode != m {
			t.Errorf("显式 %q 被改成了 %q", m, c.FilesystemMode)
		}
	}
}

// TestFilesystemModeAcceptsAllModes：oscap 认的取值，配置层必须全都放行。
//
// 遍历 oscap.ModeNames() 而不是手写三个字面量：将来 oscap 新增一态，
// 这条测试会自动覆盖到；手写的话新态会静默地没人测。
func TestFilesystemModeAcceptsAllModes(t *testing.T) {
	for _, m := range oscap.ModeNames() {
		c := baseConfig(t)
		c.FilesystemMode = m
		if err := Validate(c); err != nil {
			t.Errorf("filesystem_mode=%q 应当合法，实际报错: %v", m, err)
		}
	}
}

// TestFilesystemModeRejectsInvalid 是"拒绝"那条路径。
//
// 大小写与空白变体是重点：ParseMode 刻意不做 ToLower / trim，
// 所以 "Auto" 必须被**拒绝**而不是被悄悄纠正 —— 用户以为设的是 A、
// 服务在按 B 跑，是本项目明令禁止的静默行为。
func TestFilesystemModeRejectsInvalid(t *testing.T) {
	bad := []string{
		"",           // ApplyDefaults 没跑过：报错，不替它猜
		"Auto",       // 大小写不纠正
		"AUTO",       //
		" auto",      // 不 trim
		"auto ",      //
		"builtin",    // 像那么回事但不是取值（builtin 是 adapter 名，不是模式名）
		"native",     // v0.5 已移除的档：必须报错，不能被当成别的档放行
		"native ",    //
		"portable\n", //
		"true",       // YAML 里手滑写成布尔
		"none",
	}
	for _, v := range bad {
		c := baseConfig(t)
		c.FilesystemMode = v
		assertInvalid(t, c, "filesystem_mode")
	}
}

// TestFilesystemModeNativeRejectedWithRemovalNotice：写已移除的 "native"
// 必须报错，且报错要带上移除说明与替代取值 —— 只说「非法取值」会让人去
// 检查拼写，意识不到这一档已经没了、配置必须改。
func TestFilesystemModeNativeRejectedWithRemovalNotice(t *testing.T) {
	c := baseConfig(t)
	c.FilesystemMode = "native"
	err := Validate(c)
	if err == nil {
		t.Fatal("filesystem_mode: native（v0.5 已移除）应当校验失败")
	}
	msg := err.Error()
	for _, want := range []string{"auto", "portable", "v0.5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %q:\n%s", want, msg)
		}
	}
}

// TestFilesystemModeErrorListsAllChoices：报错必须把可选值列全。
//
// AGENTS.md §6 要求校验给出"人话"错误。只说"非法取值"而不告诉用户该写什么，
// 等于逼人去翻源码。
func TestFilesystemModeErrorListsAllChoices(t *testing.T) {
	c := baseConfig(t)
	c.FilesystemMode = "nonsense"
	err := Validate(c)
	if err == nil {
		t.Fatal("期望校验失败")
	}
	msg := err.Error()
	for _, m := range oscap.ModeNames() {
		if !strings.Contains(msg, m) {
			t.Errorf("错误信息没有列出可选值 %q:\n%s", m, msg)
		}
	}
}

// TestDefaultFilesystemModeMatchesOscap 是防漂移锁。
//
// config 里的默认值是 const 字面量（见 config.go 该常量注释），
// 与 oscap.DefaultMode 是两处独立的定义。这条测试是它们之间唯一的绑定 ——
// 少了它，改了一边而另一边没跟上，症状是"默认行为和文档说的不一样"，
// 且不会有任何编译错误。
func TestDefaultFilesystemModeMatchesOscap(t *testing.T) {
	if DefaultFilesystemMode != oscap.DefaultMode.String() {
		t.Errorf("config.DefaultFilesystemMode=%q 与 oscap.DefaultMode=%q 不一致",
			DefaultFilesystemMode, oscap.DefaultMode)
	}
	// 默认值本身必须是合法取值（否则默认配置就起不来）。
	if _, err := oscap.ParseMode(DefaultFilesystemMode); err != nil {
		t.Errorf("默认值 %q 不被 oscap.ParseMode 接受: %v", DefaultFilesystemMode, err)
	}
}

// TestFilesystemModeYAMLRoundTrip：YAML 键名走一遍真实的解析路径。
//
// 字段是加在 Config 根上的，键名写错（比如加在 server: 下面）单测断言
// 结构体字段是发现不了的 —— 只有真的喂一份 YAML 才能验出来。
func TestFilesystemModeYAMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, m := range oscap.ModeNames() {
		src := fmt.Sprintf(`
filesystem_mode: %s
shares:
  - name: public
    path: %s
auth:
  users:
    - name: alice
      password: changeme
`, m, dir)
		cfg, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("filesystem_mode: %s 解析失败: %v", m, err)
		}
		if cfg.FilesystemMode != m {
			t.Errorf("YAML 里写 %q，解析出 %q", m, cfg.FilesystemMode)
		}
	}
}

// TestFilesystemModeYAMLOmittedGetsDefault：YAML 里不写这一项，走完
// Parse（含 ApplyDefaults）后应当是 auto。
func TestFilesystemModeYAMLOmittedGetsDefault(t *testing.T) {
	dir := t.TempDir()
	src := fmt.Sprintf(`
shares:
  - name: public
    path: %s
auth:
  users:
    - name: alice
      password: changeme
`, dir)
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.FilesystemMode != DefaultFilesystemMode {
		t.Errorf("省略时得到 %q，应为 %q", cfg.FilesystemMode, DefaultFilesystemMode)
	}
}

// TestFilesystemModeYAMLTypoRejected：键名拼错必须报错，不能被静默忽略。
//
// Decode 是严格模式（未知字段报错）。这条测试盯的是那个严格性对新字段
// 依然生效 —— 否则 `filesystem-mode: portable`（中划线）会被无声吞掉，
// 用户以为开了 portable，服务在按 auto 跑。
func TestFilesystemModeYAMLTypoRejected(t *testing.T) {
	dir := t.TempDir()
	src := fmt.Sprintf(`
filesystem-mode: portable
shares:
  - name: public
    path: %s
auth:
  users:
    - name: alice
      password: changeme
`, dir)
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("键名拼成 filesystem-mode 应当报错，实际被静默忽略")
	}
}

// TestFilesystemModeYAMLInvalidValueRejected：值非法时 Parse 必须失败，
// 且错误里带得上字段名。
func TestFilesystemModeYAMLInvalidValueRejected(t *testing.T) {
	dir := t.TempDir()
	src := fmt.Sprintf(`
filesystem_mode: PORTABLE
shares:
  - name: public
    path: %s
auth:
  users:
    - name: alice
      password: changeme
`, dir)
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("filesystem_mode: PORTABLE 应当被拒绝（取值大小写敏感）")
	}
	if !strings.Contains(err.Error(), "filesystem_mode") {
		t.Errorf("错误信息里应指明是哪个字段:\n%v", err)
	}
}
