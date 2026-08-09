package vfs

// winpath.go —— Windows 路径**词法**规则。
//
// 本文件没有 build tag，三个平台都编译进来，但**里面的规则分成两类**，
// 生效范围不同，这个区分是本文件最重要的设计，改动前请先读懂：
//
//	┌ 设备名（winReservedNames）……… 三平台一律拒绝
//	└ 结尾的点与空格 ……………………… 只在会做归一的宿主上拒绝
//	                                （hostNormalizesTrailingDotSpace）
//
// # 为什么设备名三平台一致
//
// 同一份共享内容要能在平台间搬迁：如果 Linux 上能建 `CON`、搬到 Windows
// 宿主上就变成控制台设备，那是平台相关的惊吓。代价是宿主机上真实存在的、
// 名为 `aux` 的 POSIX 文件从此不可访问，这是可接受的取舍——Windows 客户端
// 本来也建不出来，误伤概率极低，没人把文件叫 `CONIN$`。
//
// # 为什么结尾的点与空格**不能**三平台一致
//
// Win32 的路径规范化会裁掉每个分量结尾的点和空格（MS Docs "File path
// formats on Windows systems" → Normalization → "Trimming characters"）。
// 于是在 Windows 宿主上：
//
//	"secret.txt "  与  "secret.txt"   是同一个文件
//	"secret.txt."  与  "secret.txt"   是同一个文件
//	"CON .txt"     与  "CON.txt"      都是控制台设备
//
// 一个对象有多个名字，意味着任何**按名字做的判定**都能被绕过：`._` 前缀的
// AppleDouble 隐藏、设备名黑名单、以及上层未来可能加的任何名字级策略。
//
// 而且这个裁剪**不是无条件的**：Go 对超过 248 字符的绝对路径会自动加上
// `\\?\` 前缀（os 包的 fixLongPath），`\\?\` 会**关闭**规范化。于是同一个
// 名字在浅目录里被裁、在深目录里不被裁——**判定结果随路径深度漂移**。
// 一条随调用位置改变结论的安全检查是不可推理的，所以在词法层直接拒绝。
//
// 但**这个别名根本不存在于 POSIX 宿主上**：Linux/macOS 的内核不裁任何东西，
// `"报告.txt "` 就是一个与 `"报告.txt"` 不同的合法文件。若三平台一律拒绝，
// 现有 Linux 共享里这类文件会**从此不可访问**——这是拿真实回归去换一个在
// 该平台上并不存在的威胁。所以防御只在会别名的宿主上生效。
//
// 代价（仅 Windows 宿主）：宿主机上真实存在的、名字以点或空格结尾的文件
// 不可访问。这与上面设备名那条同类，是已知且接受的取舍——Windows 客户端
// 本来也造不出这种名字。

import (
	"fmt"
	"strings"
)

// winTrimmedChars 是 Win32 会从分量结尾裁掉的字符。
//
// 只有 ASCII 空格 (0x20) 和点——不含 TAB 等其它空白，
// 那些属于控制字符，已被 ValidateComponent 的 c < 0x20 挡在前面。
const winTrimmedChars = ". "

// winReservedNames 是 Windows 的设备名，是本包唯一的一张设备名表。
//
// 它取代了 path.go 里那张只有 CON/PRN/AUX/NUL + COM1~9/LPT1~9 的旧表
// （已随接线一并删除），相对旧表补齐三类（出处：MS Docs "Naming Files,
// Paths, and Namespaces" → "Naming Conventions"，以及 .NET runtime 的
// PathInternal.Windows.cs）：
//
//  1. COM0 / LPT0 —— 较新的 Windows 文档已把 0 纳入保留集合。
//  2. COM¹ COM² COM³ / LPT¹ LPT² LPT³ —— 上标数字（U+00B9 / U+00B2 / U+00B3）
//     变体同样被识别为设备。这条极易漏，且正因为长得像普通文件名而危险。
//  3. CONIN$ / CONOUT$ —— 控制台输入/输出句柄的别名。
//
// 旧表的内容全部包含在本表内，winpath_test.go 的 superset 用例把这一点钉死。
var winReservedNames = func() map[string]struct{} {
	m := map[string]struct{}{
		"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
		"CONIN$": {}, "CONOUT$": {},
	}
	// COM/LPT 的 0~9 与三个上标数字变体。
	for _, dev := range []string{"COM", "LPT"} {
		for _, d := range []string{
			"0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
			"\u00b9", "\u00b2", "\u00b3", // ¹ ² ³
		} {
			m[dev+d] = struct{}{}
		}
	}
	return m
}()

// validateWindowsName 施加 Windows 特有的**名字级**规则。
//
// 只做词法判断，不碰文件系统。预期调用方是 ValidateComponent，
// 在字符集校验**之后**调用（那一步已排除控制字符与 invalidNameChars）。
//
// hostTrimsTrailingDotSpace 表示**宿主**是否会裁掉分量结尾的点和空格。
// 它由调用方传入平台常量 hostNormalizesTrailingDotSpace，之所以做成参数
// 而不是直接读常量，是为了让表驱动测试能在 Linux 上把 true / false 两条
// 路径都跑到：一个只在 Windows 上生效、而我们又没有 Windows 机器的检查，
// 如果不能在本地被翻转验证，那就等于没写（AGENTS.md §3「验收判据必须可
// 证伪」；本项目在 encryption_required 上吃过只测默认路径的假阳性）。
// 做成参数还顺带避免了包级可变状态，测试可并行、无顺序依赖。
//
// 输入约定：name 非空，且**不是** "." 或 ".."。
// 那两个由 SplitPath 的 switch 单独处理（path.go :139~:145），轮不到这里
// ——否则「结尾是点」的规则会把它们一并拒掉。
func validateWindowsName(name string, hostTrimsTrailingDotSpace bool) error {
	// 1) 结尾的点与空格。只在会归一的宿主上拒绝，理由见文件头。
	//
	// 这条同时覆盖了「整串都是点/空格」的情况（"..."、".. "、"   "），
	// 因为那些必然以点或空格结尾。
	if hostTrimsTrailingDotSpace {
		if last := name[len(name)-1]; strings.IndexByte(winTrimmedChars, last) >= 0 {
			return fmt.Errorf("%w: 路径分量 %q 以点或空格结尾（Windows 宿主会裁掉它们，造成同一对象有多个名字）",
				ErrInvalidPath, name)
		}
	}

	// 2) 设备名。取第一个 '.' 之前的部分——Windows 下 "CON.txt" 同样是设备。
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
		// 会归一的宿主上还要再裁掉这一段结尾的点和空格："CON .txt" 打开的
		// 也是控制台设备（.NET runtime 的 PathInternal.IsReservedDeviceName
		// 同样是先裁再比）。POSIX 宿主上 "CON .txt" 只是个普通文件，不裁。
		//
		// 只裁「'.' 之前的部分」而不是整个 name：整串结尾的点和空格已被第 1
		// 条拒掉，对整串再裁一次是够不到的死代码。这一点是被 winpath_test.go
		// 的变异测试逼出来的——最初写成先裁整串，把那行删掉后没有任何用例
		// 变红，说明它根本没在起作用。
		if hostTrimsTrailingDotSpace {
			base = strings.TrimRight(base, winTrimmedChars)
		}
	}
	if _, bad := winReservedNames[strings.ToUpper(base)]; bad {
		return fmt.Errorf("%w: %q 是 Windows 保留设备名", ErrInvalidPath, name)
	}
	return nil
}
