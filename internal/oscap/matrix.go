package oscap

// matrix.go —— 能力矩阵：**逐项**决定每一项能力走 native 还是 builtin。
//
// 这里是 AGENTS.md §1.2 那条铁律的落点：
// 「逐能力矩阵降级，不是整体二选一」。所以本文件里没有、也不许出现
// 「if 窄平台 { 走另一套 }」这种全局开关 —— 那会变成第二份永远没人测的实现。

import (
	"fmt"
	"sort"
	"strings"
)

// Matrix 记录每一项能力最终由哪一侧提供，以及它是在哪种 Mode 下算出来的。
//
// 值类型，可自由复制。零值是「auto 模式 + 全部 builtin」——
// 又一次让默认值落在安全侧。
type Matrix struct {
	mode Mode
	kind [capCount]Kind
}

// NewMatrix 直接构造一个矩阵，主要给测试与显式装配用。
func NewMatrix(mode Mode, kinds map[Capability]Kind) Matrix {
	m := Matrix{mode: mode}
	for c, k := range kinds {
		if c.Valid() {
			m.kind[c] = k
		}
	}
	return m
}

// Mode 返回算出本矩阵时使用的模式。
func (m Matrix) Mode() Mode { return m.mode }

// Kind 返回某项能力由哪一侧提供。非法 Capability 返回 KindBuiltin（安全侧）。
func (m Matrix) Kind(c Capability) Kind {
	if !c.Valid() {
		return KindBuiltin
	}
	return m.kind[c]
}

// Caps 返回由 k 提供的全部能力，按声明顺序。
func (m Matrix) Caps(k Kind) []Capability {
	var out []Capability
	for c := Capability(0); c < capCount; c++ {
		if m.kind[c] == k {
			out = append(out, c)
		}
	}
	return out
}

// String 是**稳定**的单行摘要，形如：
//
//	auto: xattr=native sparse_file=builtin named_stream=native \
//	stable_file_id=native creation_time=builtin dos_attributes=builtin
//
// 启动时会把它打进日志。格式被 matrix_test.go 的 golden 用例钉住：
// 排查现场问题时，运维贴过来的往往只有这一行，它变来变去就没法比对了。
func (m Matrix) String() string {
	var b strings.Builder
	b.WriteString(m.mode.String())
	b.WriteString(":")
	for c := Capability(0); c < capCount; c++ {
		fmt.Fprintf(&b, " %s=%s", c, m.kind[c])
	}
	return b.String()
}

// UnsupportedError 表示 filesystem_mode: native 下有能力拿不到原生实现。
//
// 这是**启动期**错误，必须让进程起不来。见 ModeNative 的注释：
// 会偷偷降级的 native 等于没有。
type UnsupportedError struct {
	Mode Mode
	Root string
	// Caps 是不支持的能力列表，按声明顺序，一次报全 ——
	// 用户改一项跑一次是最没必要的折磨。
	Caps []Capability
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf(
		"oscap: filesystem_mode: %s 要求全部能力走原生实现，但 %q 所在的文件系统不支持: %s"+
			"（改成 filesystem_mode: auto 可对这些项自动降级到 builtin，"+
			"改成 portable 则全部走 builtin）",
		e.Mode, e.Root, capList(e.Caps))
}

// Unwrap 让调用方能用 errors.Is(err, ErrNotSupported) 判定这一类失败。
func (e *UnsupportedError) Unwrap() error { return ErrNotSupported }

// Prober 探测某项能力在 o.Root 所在的宿主文件系统上是否被原生支持。
//
// 必须**不 panic**、不修改任何用户数据（临时探测文件要在返回前清理干净）。
// 做成函数类型而不是写死调用 ProbeNative，是为了让选择逻辑能被单测钉死：
// 真实探测的结果取决于构建机的文件系统，拿它做断言等于把测试交给运气。
type Prober func(c Capability, o Options) bool

// SelectMatrix 按模式与探测结果算出能力矩阵。
//
// probe 为 nil 时用 ProbeNative（真实探测）。
// 只有 ModeAuto / ModeNative 会调用 probe；ModePortable 一次都不调 ——
// portable 的承诺是「完全不碰 OS 的可选能力」，连探测都不该碰。
func SelectMatrix(mode Mode, o Options, probe Prober) (Matrix, error) {
	if err := o.Validate(); err != nil {
		return Matrix{}, err
	}
	if probe == nil {
		probe = ProbeNative
	}

	m := Matrix{mode: mode}

	switch mode {
	case ModePortable:
		// 全部 builtin：kind 数组的零值就是 KindBuiltin，无需赋值。
		return m, nil

	case ModeAuto:
		for c := Capability(0); c < capCount; c++ {
			if probe(c, o) {
				m.kind[c] = KindNative
			}
		}
		return m, nil

	case ModeNative:
		var missing []Capability
		for c := Capability(0); c < capCount; c++ {
			if probe(c, o) {
				m.kind[c] = KindNative
				continue
			}
			missing = append(missing, c)
		}
		if len(missing) > 0 {
			return Matrix{}, &UnsupportedError{Mode: mode, Root: o.Root, Caps: missing}
		}
		return m, nil

	default:
		return Matrix{}, fmt.Errorf("oscap: 未知 filesystem_mode %s: %w", mode, ErrInvalidArg)
	}
}

// capList 把能力列表格式化成 "a, b, c"，顺序稳定（按声明顺序）。
func capList(caps []Capability) string {
	if len(caps) == 0 {
		return "(无)"
	}
	sorted := make([]Capability, len(caps))
	copy(sorted, caps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	parts := make([]string, len(sorted))
	for i, c := range sorted {
		parts[i] = c.String()
	}
	return strings.Join(parts, ", ")
}
