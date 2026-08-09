package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// warnMetadataIgnored 是"该字段在本平台会被忽略"那条 WARN 的特征子串。
// 它是 metadata_path 在非 Windows 平台上唯一的出口。
const warnMetadataIgnored = "仅在 Windows 上生效"

// hasIgnoredWarning 判断给定平台下是否产生了"会被忽略"的 WARN。
func hasIgnoredWarning(c *Config, hostOS string) bool {
	return strings.Contains(strings.Join(warningsOn(c, hostOS), "\n"), warnMetadataIgnored)
}

// TestMetadataPathPlatformMatrix 是 metadata_path 跨平台判定的表驱动测试。
//
// 覆盖 {运行平台 Windows / 非 Windows} × {Windows 绝对路径 / POSIX 绝对路径 /
// 相对路径 / 空} 共 8 格，每格同时断言"是否报错"与"是否产生 WARN"。
//
// 这张表要钉死的核心行为是：
//   - 非 Windows 平台**完全不校验** metadata_path，无论写成什么样都不能拦住启动，
//     唯一的反馈是一条 WARN（空值除外，空值本就无话可说）；
//   - Windows 平台维持严格校验，POSIX 绝对路径与相对路径都要报错，
//     且不产生"会被忽略"的 WARN（在 Windows 上它确实生效）。
//
// 平台通过 validateOn/warningsOn 的 hostOS 形参注入，因此这 8 格在**任意**
// 宿主平台上都会被真正执行 —— 若判定点直接读 runtime.GOOS，Windows 那 4 格
// 在 Linux CI 上永远跑不到，等于没测。
func TestMetadataPathPlatformMatrix(t *testing.T) {
	// Windows 绝对路径样本。在真 Windows 上跑测试时换成 TempDir 下的路径，
	// 因为 Windows 分支还会检查父目录是否存在，硬编码的 C:\ProgramData\...
	// 在测试机上不一定有。在 Linux 上 filepath.Dir 认不得反斜杠，
	// 结果是 "."（存在），父目录检查天然通过。
	winAbs := `C:\ProgramData\stupidsamba\meta.db`
	if runtime.GOOS == "windows" {
		winAbs = filepath.Join(t.TempDir(), "meta.db")
	}

	const (
		posixAbs = "/var/lib/stupidsamba/meta.db"
		relative = "meta.db"
	)

	tests := []struct {
		name     string
		hostOS   string
		path     string
		wantErr  bool
		wantWarn bool
	}{
		// —— 运行在 Windows：严格校验，不提示"会被忽略" ——
		{"windows/win-abs", "windows", winAbs, false, false},
		{"windows/posix-abs", "windows", posixAbs, true, false},
		{"windows/relative", "windows", relative, true, false},
		{"windows/empty", "windows", "", false, false},

		// —— 运行在非 Windows：一律放行，只给 WARN ——
		// 第一格就是本次要修的 bug：给 Windows 写的配置拿到 Linux 上必须能启动。
		{"linux/win-abs", "linux", winAbs, false, true},
		{"linux/posix-abs", "linux", posixAbs, false, true},
		{"linux/relative", "linux", relative, false, true},
		{"linux/empty", "linux", "", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig(t)
			c.Shares[0].MetadataPath = tc.path

			err := validateOn(c, tc.hostOS)
			if tc.wantErr && err == nil {
				t.Errorf("hostOS=%s metadata_path=%q: 期望校验失败，实际通过", tc.hostOS, tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("hostOS=%s metadata_path=%q: 期望校验通过，实际失败: %v", tc.hostOS, tc.path, err)
			}
			// 报错时必须指到具体字段，否则用户不知道改哪一行（AGENTS.md §6）。
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "shares[0].metadata_path") {
				t.Errorf("hostOS=%s: 错误信息未指明字段 shares[0].metadata_path，实际: %v", tc.hostOS, err)
			}

			if got := hasIgnoredWarning(c, tc.hostOS); got != tc.wantWarn {
				t.Errorf("hostOS=%s metadata_path=%q: WARN 期望 %v 实际 %v，全部 WARN:\n%s",
					tc.hostOS, tc.path, tc.wantWarn, got, strings.Join(warningsOn(c, tc.hostOS), "\n"))
			}
		})
	}
}

// TestMetadataPathWindowsConfigStartsOnLinux 是这次 bug 的回归测试。
//
// 症状：一份给 Windows 写的配置（metadata_path: C:\ProgramData\...）拿到
// Linux 上，POSIX 版 filepath.IsAbs 判它不是绝对路径 → 硬报错 → 服务起不来，
// 而"该字段会被忽略"的 WARN 永远走不到。一个声称被忽略的字段却能拦住启动。
func TestMetadataPathWindowsConfigStartsOnLinux(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].MetadataPath = `C:\ProgramData\stupidsamba\meta.db`

	for _, hostOS := range []string{"linux", "darwin"} {
		if err := validateOn(c, hostOS); err != nil {
			t.Fatalf("hostOS=%s: Windows 配置必须能在此平台启动，实际被拦: %v", hostOS, err)
		}
		if !hasIgnoredWarning(c, hostOS) {
			t.Errorf("hostOS=%s: 缺少 %q 的 WARN —— 用户会以为该字段生效了", hostOS, warnMetadataIgnored)
		}
		// WARN 里要写出真实平台名，否则用户不知道是哪个平台忽略了它。
		if !strings.Contains(strings.Join(warningsOn(c, hostOS), "\n"), hostOS) {
			t.Errorf("hostOS=%s: WARN 未写明平台名", hostOS)
		}
	}
}

// TestIsAbsPathOn 覆盖平台化绝对路径判定本身。
//
// 重点是那些"看起来像绝对路径但不是"的形式，它们是误判的高发区。
func TestIsAbsPathOn(t *testing.T) {
	tests := []struct {
		path      string
		onWindows bool
		onPosix   bool
	}{
		{`C:\ProgramData\x.db`, true, false}, // 盘符 + 反斜杠
		{`C:/ProgramData/x.db`, true, false}, // Windows 上正斜杠同样是分隔符
		{`z:\x`, true, false},                // 小写盘符
		{`C:x.db`, false, false},             // 盘符**相对**路径，不是绝对
		{`C:`, false, false},                 // 只有盘符
		{`\\server\share\x.db`, true, false}, // UNC
		{`\\?\C:\x.db`, true, false},         // 扩展长度前缀
		{`\x.db`, false, false},              // 有根无卷：Go 的 IsAbs 也判 false
		{`/var/lib/x.db`, false, true},       // POSIX 绝对
		{`var/lib/x.db`, false, false},       // 相对
		{`./x.db`, false, false},             // 显式相对
		{``, false, false},                   // 空
		{`1:\x`, false, false},               // 盘符必须是字母
		{`//server/share/x`, true, true},     // 双正斜杠：Windows 认 UNC，POSIX 也以 / 开头
	}

	for _, tc := range tests {
		if got := isAbsPathOn(tc.path, true); got != tc.onWindows {
			t.Errorf("isAbsPathOn(%q, windows=true) = %v，期望 %v", tc.path, got, tc.onWindows)
		}
		if got := isAbsPathOn(tc.path, false); got != tc.onPosix {
			t.Errorf("isAbsPathOn(%q, windows=false) = %v，期望 %v", tc.path, got, tc.onPosix)
		}
	}
}

// TestIsAbsPathOnMatchesStdlibOnHost 用标准库给本平台那一半判定做交叉验证。
//
// isAbsPathOn 是手写的，手写就可能与 Go 的真实语义漂移。这里拿
// filepath.IsAbs（编译进来的是**本平台**版本）做对照，至少保证
// "当前平台这一半"没写错。另一半由上面的固定用例表覆盖。
func TestIsAbsPathOnMatchesStdlibOnHost(t *testing.T) {
	hostIsWindows := runtime.GOOS == "windows"
	paths := []string{
		`/var/lib/x.db`, `var/lib/x.db`, `./x.db`, ``,
		`C:\ProgramData\x.db`, `C:/x.db`, `C:x.db`, `\\server\share\x`, `\x.db`,
	}
	for _, p := range paths {
		want := filepath.IsAbs(p)
		if got := isAbsPathOn(p, hostIsWindows); got != want {
			t.Errorf("isAbsPathOn(%q, windows=%v) = %v，但本平台 filepath.IsAbs = %v",
				p, hostIsWindows, got, want)
		}
	}
}
