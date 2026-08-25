package oscap

// mode.go —— 配置项 filesystem_mode 的两态（AGENTS.md §1.2）。

import "fmt"

// Mode 是 filesystem_mode 的取值。
type Mode uint8

const (
	// ModeAuto 逐项探测宿主能力，能 native 就 native，不能就落到 builtin。
	// 零值即默认值。
	ModeAuto Mode = iota

	// ModePortable 强制全部走 builtin，完全不碰 OS 的可选能力。
	// 可移植性/可预测性最高，性能最低。
	ModePortable
)

// DefaultMode 是未配置时的取值。
const DefaultMode = ModeAuto

// modeNames 是两态的稳定字符串名，与 YAML 里写的值一一对应。
var modeNames = map[Mode]string{
	ModeAuto:     "auto",
	ModePortable: "portable",
}

// errNativeRemoved 是 ParseMode("native") 的专门错误。
//
// 它必须与普通非法值的报错分开：写 native 的用户多半是从旧版本文档抄来的，
// 报一句「非法取值」会让人去检查拼写，而不是意识到这一档已经没了、必须改配置。
//
// 历史档案："native" 档在 v0.4 及以前与 auto/portable 并列，
// 契约是「强制全部走 native、缺一项启动即报错」。它已于 v0.5 开发版移除：
// 每个平台都至少有一项能力没有原生实现，该契约在任何平台上都无法满足，
// 三平台恒定启动失败，从未有过可用场景。
var errNativeRemoved = fmt.Errorf(
	"oscap: filesystem_mode \"native\" 已在 v0.5 开发版移除：" +
		"原契约要求全部能力走原生实现，但每个平台都至少有一项能力没有原生实现，" +
		"该契约在任何平台上都无法满足；" +
		"请改用 auto（逐项探测，原生优先、缺失自动落到 builtin）或 portable（全部 builtin）")

func (m Mode) String() string {
	if s, ok := modeNames[m]; ok {
		return s
	}
	return fmt.Sprintf("mode(%d)", uint8(m))
}

// ModeNames 返回全部合法取值（顺序稳定，供配置报错时列给用户看）。
func ModeNames() []string {
	return []string{
		modeNames[ModeAuto],
		modeNames[ModePortable],
	}
}

// ParseMode 解析配置里的字符串。
//
// **刻意严格**：不做 ToLower、不 trim 空白、不认空串。
// 配置项拼错却被静默"纠正"是运维灾难（用户以为设的是 A，服务在按 B 跑），
// 与 config.Load 的严格模式（未知字段直接报错）保持同一种态度。
//
// "native" 是已移除的取值（v0.5），返回专门的移除提示而不是普通非法值报错，
// 见 errNativeRemoved。
func ParseMode(s string) (Mode, error) {
	if s == "native" {
		return DefaultMode, errNativeRemoved
	}
	for m, name := range modeNames {
		if name == s {
			return m, nil
		}
	}
	return DefaultMode, fmt.Errorf("oscap: 非法 filesystem_mode %q: %w", s, ErrInvalidArg)
}
