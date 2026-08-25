package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// warnMetadataIgnored 曾是"该字段在本平台会被忽略"那条 WARN 的特征子串。
//
// **这条 WARN 已被删除，因为 PR #159 之后它是假话**：metadata_path 现在还决定
// oscap builtin 那份旁路 bbolt 库落在哪（internal/vfs/oscap_xattr.go 的
// oscapMetadataDir），auto 与 portable 两档在 Linux 上实测都会真的在该目录里
// 建出 .stupidsamba-oscap-<hash>.db。
//
// 常量保留下来当**反向断言**用：断言这句话再也不出现。
// 直接删掉常量，"这句假话没有偷偷回来"就没有任何东西守着了 —— 而它原先恰恰是
// 被一条绿灯测试钉住的，属于"伪装成有覆盖"的最难自愈的形态。
const warnMetadataIgnored = "仅在 Windows 上生效"

// hasIgnoredWarning 判断给定平台下是否仍在产生那条已作废的"会被忽略" WARN。
// 正确答案在任何平台上都应当是 false。
func hasIgnoredWarning(c *Config, hostOS string) bool {
	return strings.Contains(strings.Join(warningsOn(c, hostOS), "\n"), warnMetadataIgnored)
}

// TestMetadataPathPlatformMatrix 是 metadata_path 跨平台判定的表驱动测试。
//
// 覆盖 {运行平台 Windows / 非 Windows} × {Windows 绝对路径 / POSIX 绝对路径 /
// 相对路径 / 空} 共 8 格，每格同时断言"是否报错"与"是否产生 WARN"。
//
// 这张表要钉死的核心行为是（**PR #159 之后已改**）：
//   - **所有平台**都校验 metadata_path。旧版本非 Windows 完全不校验，
//     前提是"该字段会被忽略"；#159 把 oscap 接进数据路径后这个前提没了，
//     该字段在所有平台都决定旁路 bbolt 库的位置。
//   - 绝对性按**运行平台**判定：写成另一个平台的绝对路径要报错，
//     且错误信息必须点明"这是另一个平台的绝对路径"而不是笼统说"不是绝对路径"。
//   - 那条"仅在 Windows 上生效"的 WARN **已删除**，任何平台都不该再出现。
//
// 平台通过 validateOn/warningsOn 的 hostOS 形参注入，因此这 8 格在**任意**
// 宿主平台上都会被真正执行 —— 若判定点直接读 runtime.GOOS，Windows 那 4 格
// 在 Linux CI 上永远跑不到，等于没测。
func TestMetadataPathPlatformMatrix(t *testing.T) {
	// Windows 绝对路径样本。在真 Windows 上跑测试时换成 TempDir 下的路径，
	// 因为同平台校验还会检查父目录是否存在，硬编码的 C:\ProgramData\...
	// 在测试机上不一定有。
	winAbs := `C:\ProgramData\stupidsamba\meta.db`
	if runtime.GOOS == "windows" {
		winAbs = filepath.Join(t.TempDir(), "meta.db")
	}

	// POSIX 绝对路径样本同理：在真 POSIX 上要指向一个**存在**的父目录，
	// 否则命中的是"目录不存在"而不是我们想验的语法判定。
	posixAbs := "/var/lib/stupidsamba/meta.db"
	if runtime.GOOS != "windows" {
		posixAbs = filepath.Join(t.TempDir(), "meta.db")
	}

	const relative = "meta.db"

	tests := []struct {
		name     string
		hostOS   string
		path     string
		wantErr  bool
		wantWarn bool
	}{
		// —— 运行在 Windows：严格校验 ——
		{"windows/win-abs", "windows", winAbs, false, false},
		{"windows/posix-abs", "windows", posixAbs, true, false},
		{"windows/relative", "windows", relative, true, false},
		{"windows/empty", "windows", "", false, false},

		// —— 运行在非 Windows：#159 之后同样严格校验 ——
		// win-abs 这格是**本次行为变化**：以前放行 + WARN，现在报错。
		// 理由见 validateShareMetadataPath 的注释：放行的话
		// filepath.Dir(`C:\...`) 在 Linux 上得到 "."，库会被静默建在 CWD。
		{"linux/win-abs", "linux", winAbs, true, false},
		{"linux/posix-abs", "linux", posixAbs, false, false},
		{"linux/relative", "linux", relative, true, false},
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

// TestMetadataPathWindowsConfigOnPosixNowFailsLoudly 取代了原先的
// TestMetadataPathWindowsConfigStartsOnLinux。**改的是期望，不是判据。**
//
// # 原用例断言什么，为什么它不再成立
//
// 原用例（PR #18 的回归测试）断言：一份给 Windows 写的配置
// （metadata_path: C:\ProgramData\...）拿到 Linux 上**必须能启动**，只给 WARN。
// 它当时的理由是——"一个声称被忽略的字段却能拦住启动，这是自相矛盾的"。
//
// PR #159 把该字段接进了所有平台的数据路径，**"被忽略"这个前提没了**，
// 于是那条理由连同它的结论一起失效。这正是本仓库反复登记的
// "结论对、理由过期"的镜像形态：**理由过期之后，原本正确的结论会变成错的。**
//
// # 为什么新期望是"报错"而不是"继续放行"
//
// 放行的代价现在比拦住大得多：Linux 上 filepath.Dir(`C:\ProgramData\x\meta.db`)
// 得到 "."，旁路 bbolt 库会被**静默**建在进程当前工作目录里 ——
// 用户既不知道它在哪，重启换个 CWD 还会换一份空库。宁可启动就报错。
//
// # 保留下来的两条判据（它们与平台无关，仍然有效）
//
//  1. 错误必须指到具体字段（AGENTS.md §6：人话错误信息）；
//  2. 错误必须点明"这是另一个平台的绝对路径"，否则用户看着自己写的
//     C:\ 开头的路径被说成"不是绝对路径"，只会以为程序坏了。
func TestMetadataPathWindowsConfigOnPosixNowFailsLoudly(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].MetadataPath = `C:\ProgramData\stupidsamba\meta.db`

	for _, hostOS := range []string{"linux", "darwin"} {
		err := validateOn(c, hostOS)
		if err == nil {
			t.Fatalf("hostOS=%s: Windows 路径在此平台必须报错（否则库会静默建在 CWD），实际通过", hostOS)
		}
		msg := err.Error()
		if !strings.Contains(msg, "shares[0].metadata_path") {
			t.Errorf("hostOS=%s: 错误未指明字段 shares[0].metadata_path，实际: %v", hostOS, err)
		}
		if !strings.Contains(msg, "另一个平台的绝对路径") {
			t.Errorf("hostOS=%s: 错误未点明这是另一个平台的绝对路径，用户会以为程序坏了，实际: %v", hostOS, err)
		}
		if !strings.Contains(msg, hostOS) {
			t.Errorf("hostOS=%s: 错误未写明当前运行平台名，实际: %v", hostOS, err)
		}
	}
}

// TestMetadataPathIgnoredWarningIsGone 是那句假话的**反向断言**。
//
// "metadata_path 仅在 Windows 上生效"这句话曾经被一条绿灯测试钉着 ——
// 想改对它的人得先让绿灯变红，而绿灯天然让人不敢动。这比单纯的文档腐烂
// 更难自愈，因为它伪装成"有测试覆盖"。
//
// 所以删掉那句 WARN 之后要**反过来钉一次**：任何平台都不许再出现它。
// 没有这一条，它随时可能被"好心"加回来而无人察觉。
func TestMetadataPathIgnoredWarningIsGone(t *testing.T) {
	c := baseConfig(t)
	c.Shares[0].MetadataPath = filepath.Join(t.TempDir(), "meta.db")
	if runtime.GOOS == "windows" {
		c.Shares[0].MetadataPath = filepath.Join(t.TempDir(), "meta.db")
	}

	for _, hostOS := range []string{"linux", "darwin", "windows"} {
		if hasIgnoredWarning(c, hostOS) {
			t.Errorf("hostOS=%s: 已作废的 WARN %q 又出现了，全部 WARN:\n%s",
				hostOS, warnMetadataIgnored, strings.Join(warningsOn(c, hostOS), "\n"))
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
