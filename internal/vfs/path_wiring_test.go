package vfs

// path_wiring_test.go —— 证明 ValidateComponent 真的**接线**到了
// validateWindowsName（winpath.go），而不只是「函数写好了没人调」。
//
// # 为什么单独一个文件、为什么不能只测 validateWindowsName
//
// winpath_test.go 已经把 validateWindowsName 本身测得很透（含 7/7 变异测试），
// 但那些用例**在接线之前也全绿**——被测函数是死代码时它照样自洽。这正是本
// 项目 §3「验收判据必须可证伪」反复强调的那类假阳性：断言的是「零件合格」，
// 而不是「零件装上了」。
//
// 本文件的每条断言都从**外部入口** ValidateComponent / SplitPath 进去，
// 且刻意挑「只有新表拒、旧表不拒」的输入，这样:
//
//	接线在  → 绿
//	接线断  → 红
//
// 反向实验（把 validateComponent 末尾那句 validateWindowsName 调用注释掉）
// 的实测结果记录在 PR 描述里。
//
// # 关于 hostTrimsTrailingDotSpace 的两侧
//
// 结尾点/空格规则只在 Windows 宿主生效，而 CI 与开发容器都只有 Linux。
// 所以凡是受该开关影响的断言，都走参数化的 validateComponent 把 true/false
// 两条路径都跑到；同时用导出的 ValidateComponent 断言「它传进去的确实是平台
// 常量而不是写死的字面量」。

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateComponentRejectsDevicesOnlyNewTableKnows 是本文件的核心。
//
// 这些设备名**旧表 reservedNames 一个都不认**（旧表只有 CON/PRN/AUX/NUL 与
// COM1~9/LPT1~9）。因此：只要 ValidateComponent 没有接到 validateWindowsName
// 上，下面每一条都会返回 nil，测试立刻变红。
//
// 换句话说，这一组用例的唯一职责就是发现「接线断了」。
func TestValidateComponentRejectsDevicesOnlyNewTableKnows(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"COM0", "旧表只有 COM1~9，COM0 是新表补的"},
		{"LPT0", "同上"},
		{"com0", "设备名大小写不敏感"},
		{"COM\u00b9", "上标 ¹ 变体，旧表完全没有"},
		{"COM\u00b2", "上标 ²"},
		{"COM\u00b3", "上标 ³"},
		{"LPT\u00b9", "上标 ¹（LPT 侧）"},
		{"CONIN$", "控制台输入句柄别名，旧表没有"},
		{"CONOUT$", "控制台输出句柄别名，旧表没有"},
		{"conout$.txt", "带扩展名 + 小写，仍是设备"},
		{"COM0.tar.gz", "只看第一个点之前的部分"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateComponent(c.name)
			if err == nil {
				t.Fatalf("ValidateComponent(%q) = nil，应当拒绝；理由：%s\n"+
					"（这条失败几乎一定意味着 ValidateComponent 没有调用 "+
					"validateWindowsName，即接线断了）", c.name, c.why)
			}
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("错误应当可 errors.Is 到 ErrInvalidPath，实得 %v", err)
			}
		})
	}
}

// TestValidateComponentStillRejectsLegacyDevices —— 旧表里的设备名一个都不能丢。
//
// 与 TestValidateWindowsNameSupersetOfLegacyReservedNames 的区别：那条比的是
// 两张**表**，这条走的是**真实入口**。表是超集但入口没接上，那条照样绿。
func TestValidateComponentStillRejectsLegacyDevices(t *testing.T) {
	for _, name := range legacyReservedNames {
		for _, variant := range []string{name, strings.ToLower(name), name + ".txt"} {
			t.Run(variant, func(t *testing.T) {
				if err := ValidateComponent(variant); err == nil {
					t.Fatalf("ValidateComponent(%q) = nil；接线后旧表的设备名被漏掉了", variant)
				}
			})
		}
	}
}

// TestSplitPathRejectsDevicesOnlyNewTableKnows —— 再往外一层。
//
// ValidateComponent 是导出的，但客户端路径实际走的是 SplitPath。这里确认
// 新规则确实作用在那条真实链路上（SplitPath → ValidateComponent →
// validateWindowsName），而不是只在一个没人走的导出函数上生效。
func TestSplitPathRejectsDevicesOnlyNewTableKnows(t *testing.T) {
	for _, p := range []string{
		"CONIN$",
		"dir/COM0",
		`dir\LPT0`, // 反斜杠同样是分隔符
		"a/b/COM\u00b2/c",
	} {
		t.Run(p, func(t *testing.T) {
			if _, err := SplitPath(p); err == nil {
				t.Fatalf("SplitPath(%q) = nil，应当拒绝：其中含 Windows 设备名分量", p)
			}
		})
	}
}

// TestValidateComponentTrailingDotSpaceIsHostConditional —— 接线的另一半。
//
// 设备名那组在两种宿主下结论相同，所以它证明了「有接线」，却证明不了
// 「传进去的开关是对的」。这一组补上：同一个输入在两侧必须给出相反结论。
//
//   - hostTrims=true（Windows 宿主）：拒。Win32 裁掉结尾点/空格 → 一个对象
//     多个名字 → 按名字做的判定都能被绕过。
//   - hostTrims=false（POSIX 宿主）：放行。内核不裁，`"报告.txt "` 是合法且
//     独立的文件；拒掉它等于让现有 Linux 共享里这类文件不可访问。
//
// 这是**只能**通过参数化的 validateComponent 验证的部分——我们没有 Windows
// 机器，若不把开关做成参数，Windows 侧这半边逻辑在合并前不可能被跑到一次。
func TestValidateComponentTrailingDotSpaceIsHostConditional(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"secret.txt ", "裁掉尾空格后等于 secret.txt"},
		{"secret.txt.", "裁掉尾点后等于 secret.txt"},
		{"web.config ", "经典的过滤器绕过写法"},
		{"报告.txt ", "非 ASCII 也一样"},
		{"CON .x", "裁剪之后 base 是 CON，打开的是控制台设备"},
	}
	for _, c := range cases {
		t.Run("Windows宿主/"+c.name, func(t *testing.T) {
			err := validateComponent(c.name, true)
			if err == nil {
				t.Fatalf("validateComponent(%q, true) = nil，应当拒绝；理由：%s\n"+
					"（Windows 宿主上的结尾点/空格规则没有经 ValidateComponent 接线）",
					c.name, c.why)
			}
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("错误应当可 errors.Is 到 ErrInvalidPath，实得 %v", err)
			}
		})
		t.Run("POSIX宿主/"+c.name, func(t *testing.T) {
			if err := validateComponent(c.name, false); err != nil {
				t.Fatalf("validateComponent(%q, false) = %v；POSIX 宿主不裁结尾字符，"+
					"这是个合法且独立的文件名，拒绝它会让现有共享里的文件不可访问", c.name, err)
			}
		})
	}
}

// TestValidateComponentPassesPlatformConstant 钉死「导出入口传的是平台常量」。
//
// 上一条测的是 validateComponent 这个内部函数；这条测的是 ValidateComponent
// 这层薄包装到底传了什么。写死成 true 会让本条在 Linux 上立刻变红
// （"a " 这类合法 POSIX 名字会被误拒）。
//
// 写死成 false 则要到 Windows 上才红——那是没有 Windows 机器就无法在本地
// 消除的盲区，用 TestHostNormalizesTrailingDotSpaceMatchesGOOS（winpath_test.go）
// 把常量本身钉在 GOOS 上，尽可能缩小它。
func TestValidateComponentPassesPlatformConstant(t *testing.T) {
	for _, name := range []string{"a ", "a.", "报告.txt ", "CON .x"} {
		t.Run(name, func(t *testing.T) {
			gotErr := ValidateComponent(name) != nil
			wantErr := validateComponent(name, hostNormalizesTrailingDotSpace) != nil
			if gotErr != wantErr {
				t.Fatalf("ValidateComponent(%q) 出错=%v，但按平台常量 (%v) 应当为 %v；"+
					"导出入口大概率传了写死的字面量而不是 hostNormalizesTrailingDotSpace",
					name, gotErr, hostNormalizesTrailingDotSpace, wantErr)
			}
		})
	}
}

// TestValidateComponentKeepsDotAndDotDot —— 契约没被接线改坏。
//
// ValidateComponent 的文档明确写着它**故意放行** "." 与 ".."（SplitPath 在
// switch 里先行处理，validateChildName 另行拦截）。而 validateWindowsName 的
// 第 1 条规则看的是结尾字符，会把这两个连坐拒掉——所以 validateComponent 里
// 有一句先行返回。
//
// 那句先行返回在 Linux 上删掉也没有任何可见变化（开关是 false），正是
// AGENTS.md 说的「不可证伪就等于没写」。用 hostTrims=true 直接断言，
// 删掉那句立刻变红。
func TestValidateComponentKeepsDotAndDotDot(t *testing.T) {
	for _, name := range []string{".", ".."} {
		for _, hostTrims := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				if err := validateComponent(name, hostTrims); err != nil {
					t.Fatalf("validateComponent(%q, %v) = %v；本函数按契约放行 "+
						"\".\" 与 \"..\"，三平台结论必须一致", name, hostTrims, err)
				}
			})
		}
	}
}

// TestValidateComponentAccepts —— 反向对照。
//
// 少了这一组，把 ValidateComponent 写成「一律返回错误」也能让上面所有拒绝
// 用例全绿。接线是一次**收紧**，收紧最容易顺手误伤正常名字。
func TestValidateComponentAccepts(t *testing.T) {
	for _, name := range []string{
		"a", "file.txt", "Report.TXT",
		".hidden", "..hidden",
		"CONSOLE", "CON2", "COMx", "COM10", "COM1x", "XCOM1",
		"NULL", "AUXILIARY", "CONIN", "CONOUT$x",
		"C0N", "PROGRA~1",
		"bands", "0000a1f3", // Time Machine sparsebundle 里的真实名字
		"._foo", // AppleDouble 旁路文件
		"报告.txt",
		strings.Repeat("x", MaxComponentLen),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateComponent(name); err != nil {
				t.Fatalf("ValidateComponent(%q) = %v，应当放行", name, err)
			}
		})
	}
}

// TestValidateComponentPreExistingRulesSurvive —— 接线不能挤掉原有规则。
//
// validateWindowsName 是加在字符集校验**之后**的，这几条走的是它前面的分支。
func TestValidateComponentPreExistingRulesSurvive(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"", "空分量"},
		{strings.Repeat("x", MaxComponentLen+1), "超长"},
		{"a\x00b", "NUL 属于控制字符"},
		{"a\x1fb", "控制字符"},
		{"a:b", "':' 是流分隔符，到这里不允许出现"},
		{"a*b", "通配符不是合法文件名字符"},
		{"a|b", "MS-FSCC §2.1.5 非法字符"},
		{`a\b`, "反斜杠"},
		{"a/b", "斜杠"},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			if err := ValidateComponent(c.name); err == nil {
				t.Fatalf("ValidateComponent(%q) = nil，应当拒绝；理由：%s", c.name, c.why)
			}
		})
	}
}
