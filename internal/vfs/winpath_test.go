package vfs

// winpath_test.go —— Windows 名字规则的表驱动测试。
//
// 这些用例**全部在 Linux 上跑**，这是刻意的：规则是纯词法的，不需要真
// Windows 就能完整验证，而本项目的 CI 与开发容器都只有 Linux。
//
// validateWindowsName 的 hostTrimsTrailingDotSpace 做成参数（而不是直接读
// 平台常量）就是为了这里：只在 Windows 上生效、我们又没有 Windows 机器的
// 检查，如果不能在本地被翻转验证，那就等于没写。所以凡是受该开关影响的
// 规则，下面都**两侧都断言**——true 侧拒绝、false 侧放行——而不是只测一边。

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// bothHosts 用于给受宿主归一行为影响的用例做双向断言。
var bothHosts = []struct {
	name      string
	hostTrims bool
}{
	{"Windows宿主", true},
	{"POSIX宿主", false},
}

// TestHostNormalizesTrailingDotSpaceMatchesGOOS 把平台常量钉死在 GOOS 上。
//
// 常量本身由 build tag 选择，写反了（两个文件都写 true、或 tag 写错）
// 编译与 vet 都不会报错，只有这个断言能发现。它在任何 GOOS 上都成立，
// 所以将来真在 Windows 上跑测试时同样有效。
func TestHostNormalizesTrailingDotSpaceMatchesGOOS(t *testing.T) {
	want := runtime.GOOS == "windows"
	if hostNormalizesTrailingDotSpace != want {
		t.Fatalf("GOOS=%s 时 hostNormalizesTrailingDotSpace = %v，应为 %v（build tag 写反了？）",
			runtime.GOOS, hostNormalizesTrailingDotSpace, want)
	}
}

// TestValidateWindowsNameReservedNames —— 设备名三平台一律拒绝。
//
// 这一类**不受**宿主归一开关影响：目的是保证共享内容能在平台间搬迁，
// 不能出现「Linux 上建得出 CON、搬到 Windows 就变成控制台设备」。
func TestValidateWindowsNameReservedNames(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"CON", "控制台设备"},
		{"con", "设备名大小写不敏感"},
		{"NUL", "空设备"},
		{"AUX", "辅助设备"},
		{"PRN", "打印机设备"},
		{"COM1", "串口"},
		{"COM0", "较新的 Windows 也把 0 纳入保留集合"},
		{"LPT0", "同上"},
		{"LPT9", "并口"},
		{"COM\u00b9", "上标 ¹ 变体同样是设备，最容易漏的一类"},
		{"COM\u00b2", "上标 ²"},
		{"LPT\u00b3", "上标 ³"},
		{"CONIN$", "控制台输入句柄别名"},
		{"CONOUT$", "控制台输出句柄别名"},
		{"CON.txt", "扩展名不能让设备名变成普通文件"},
		{"nul.tar.gz", "只看第一个点之前的部分"},
	}
	for _, h := range bothHosts {
		for _, c := range cases {
			t.Run(h.name+"/"+c.name, func(t *testing.T) {
				err := validateWindowsName(c.name, h.hostTrims)
				if err == nil {
					t.Fatalf("validateWindowsName(%q, %v) = nil，应当拒绝；理由：%s",
						c.name, h.hostTrims, c.why)
				}
				if !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("错误应当可 errors.Is 到 ErrInvalidPath，实得 %v", err)
				}
			})
		}
	}
}

// TestValidateWindowsNameTrailingDotSpaceIsHostConditional —— 结尾点/空格。
//
// 这是本文件的核心断言，两侧都要成立：
//   - Windows 宿主：拒绝。Win32 裁掉结尾点空格 → 一个对象多个名字 →
//     任何按名字做的判定都能被绕过。
//   - POSIX 宿主：放行。内核不裁任何东西，`"报告.txt "` 是一个合法且与
//     `"报告.txt"` 不同的文件；拒绝它等于让现有共享里这类文件不可访问。
//
// 只写上半段（早先的版本就是）会造成真实的 Linux 回归，且这个回归不会被
// 任何用例发现——下半段就是防这个的。
func TestValidateWindowsNameTrailingDotSpaceIsHostConditional(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"secret.txt ", "裁掉尾空格后等于 secret.txt，同一文件两个名字"},
		{"secret.txt.", "裁掉尾点后等于 secret.txt"},
		{"secret.txt...", "多个尾点同样被裁"},
		{"secret.txt . ", "点与空格混合结尾"},
		{"web.config ", "经典的过滤器绕过写法"},
		{".. ", "裁剪后是 ..；能否真逃到父目录未在真机验证，但 `\\\\?\\` 会关闭规范化、判定随路径深度漂移，仅此就足以拒绝"},
		{"...", "整串都是点"},
		{". ", "点加空格"},
		{"   ", "整串空格，裁剪后是空名字"},
		{"a.", "最短的尾点用例"},
		{"a ", "最短的尾空格用例"},
		{"报告.txt ", "非 ASCII 也一样"},
	}
	for _, c := range cases {
		t.Run("Windows宿主/"+c.name, func(t *testing.T) {
			err := validateWindowsName(c.name, true)
			if err == nil {
				t.Fatalf("validateWindowsName(%q, true) = nil，应当拒绝；理由：%s", c.name, c.why)
			}
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("错误应当可 errors.Is 到 ErrInvalidPath，实得 %v", err)
			}
		})
		t.Run("POSIX宿主/"+c.name, func(t *testing.T) {
			if err := validateWindowsName(c.name, false); err != nil {
				t.Fatalf("validateWindowsName(%q, false) = %v；POSIX 宿主不裁结尾字符，"+
					"这是个合法且独立的文件名，拒绝它会让现有共享里的文件不可访问", c.name, err)
			}
		})
	}
}

// TestValidateWindowsNameDeviceCheckTrimsBeforeDot —— 设备名判定前的裁剪。
//
// "CON .x" 不以点或空格结尾，逃得过第 1 条；第 2 条取第一个 '.' 之前得到
// "CON "，Windows 宿主上必须再裁一次才认得出它是设备。
//
// 用例本身是变异测试的产物：最初写的输入是 "CON. x"，把裁剪那行删掉之后
// 它照样通过（"CON. x" 取到的就是 "CON"，压根没触发裁剪），也就是说那一版
// 断言是假的。换成 "CON .x" 之后，删掉裁剪立刻变红。
func TestValidateWindowsNameDeviceCheckTrimsBeforeDot(t *testing.T) {
	const name = "CON .x"
	if err := validateWindowsName(name, true); err == nil {
		t.Fatalf("validateWindowsName(%q, true) = nil；Windows 宿主上它打开的是控制台设备", name)
	}
	if err := validateWindowsName(name, false); err != nil {
		t.Fatalf("validateWindowsName(%q, false) = %v；POSIX 宿主上这只是个普通文件名", name, err)
	}
}

// TestValidateWindowsNameAccepts —— 反向对照。
//
// 少了这一组，把函数写成「一律返回错误」也能让上面所有拒绝用例全绿。
// 这些名字在两种宿主下都必须放行。
func TestValidateWindowsNameAccepts(t *testing.T) {
	cases := []string{
		"a", "file.txt", "Report.TXT",
		".hidden",      // 点开头合法，只有点结尾才可能不合法
		"..hidden",     // 同上
		"CONSOLE",      // 设备名的前缀不是设备名
		"CON2", "COMx", // 形似但不在表内
		"COM10",          // 只有 COM0~COM9 是设备
		"COM1x", "XCOM1", // 不是精确匹配就不该拒
		"NULL", "AUXILIARY", // 前缀相同但不是设备
		"CONIN", "CONOUT$x", // 差一个字符
		"a.b.c",
		"bands", "0000a1f3", // Time Machine sparsebundle 里的真实名字
		"._foo",                              // AppleDouble 旁路文件
		"C0N",                                // 数字零不是字母 O
		"PROGRA~1",                           // 8.3 短名是合法文件名，不该拒
		"报告.txt",                             // 非 ASCII
		strings.Repeat("x", MaxComponentLen), // 长度由 ValidateComponent 管，本函数不管
	}
	for _, h := range bothHosts {
		for _, c := range cases {
			t.Run(h.name+"/"+c, func(t *testing.T) {
				if err := validateWindowsName(c, h.hostTrims); err != nil {
					t.Fatalf("validateWindowsName(%q, %v) = %v，应当放行", c, h.hostTrims, err)
				}
			})
		}
	}
}

// legacyReservedNames 是 path.go 里那张已删除的旧设备名表的**冻结快照**。
//
// 接线（ValidateComponent → validateWindowsName）时旧表被删掉了，但「新表
// 不能比旧表少认一个设备名」这条约束不能跟着一起消失，所以在这里留一份字面
// 量。它是历史事实，**不要**随 winReservedNames 一起增补——那样这条断言就
// 变成自己跟自己比，永远为真。
var legacyReservedNames = []string{
	"CON", "PRN", "AUX", "NUL",
	"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
	"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
}

// TestValidateWindowsNameSupersetOfLegacyReservedNames 确认新表是旧表的
// **超集**：换用 validateWindowsName 之后不能有任何设备名被漏掉。
func TestValidateWindowsNameSupersetOfLegacyReservedNames(t *testing.T) {
	for _, old := range legacyReservedNames {
		if _, ok := winReservedNames[old]; !ok {
			t.Errorf("旧表 reservedNames 里有 %q，winReservedNames 却没有；"+
				"接线后这个设备名会被漏掉", old)
		}
	}
}
