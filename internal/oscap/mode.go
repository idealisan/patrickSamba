package oscap

// mode.go —— 配置项 filesystem_mode 的三态（AGENTS.md §1.2）。

import "fmt"

// Mode 是 filesystem_mode 的取值。
type Mode uint8

const (
	// ModeAuto 逐项探测宿主能力，能 native 就 native，不能就落到 builtin。
	// 零值即默认值。
	ModeAuto Mode = iota

	// ModeNative 强制全部走 native；探测到某项不支持就**启动即报错**，
	// 不静默降级。
	//
	// 为什么必须报错而不是降级：它的用途是**在测试里钉死走的是哪条路**。
	// 一个会偷偷降级的 native 等于没有 —— 这个亏本项目已经吃过：
	// 某个策略开关只测了「允许」那条路径，全绿，而「拒绝」那条路径压根
	// 没接线，测试从头到尾都在验证同一条路。
	ModeNative

	// ModePortable 强制全部走 builtin，完全不碰 OS 的可选能力。
	// 可移植性/可预测性最高，性能最低。
	ModePortable
)

// DefaultMode 是未配置时的取值。
const DefaultMode = ModeAuto

// modeNames 是三态的稳定字符串名，与 YAML 里写的值一一对应。
var modeNames = map[Mode]string{
	ModeAuto:     "auto",
	ModeNative:   "native",
	ModePortable: "portable",
}

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
		modeNames[ModeNative],
		modeNames[ModePortable],
	}
}

// ParseMode 解析配置里的字符串。
//
// **刻意严格**：不做 ToLower、不 trim 空白、不认空串。
// 配置项拼错却被静默"纠正"是运维灾难（用户以为设的是 A，服务在按 B 跑），
// 与 config.Load 的严格模式（未知字段直接报错）保持同一种态度。
func ParseMode(s string) (Mode, error) {
	for m, name := range modeNames {
		if name == s {
			return m, nil
		}
	}
	return DefaultMode, fmt.Errorf("oscap: 非法 filesystem_mode %q: %w", s, ErrInvalidArg)
}
