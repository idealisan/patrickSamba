package oscap

// provider.go —— 把两侧适配器按矩阵组装成一个 Provider。
//
// 装配顺序（也是错误处理的优先级）：
//  1. 算矩阵（SelectMatrix）——「谁来做」在这一步就定死了；
//  2. 向两侧要实现（Factory）；
//  3. 逐项挑选并**校验完整性**：builtin 缺项一律硬失败。

import (
	"fmt"
	"strings"
)

// Set 是一套能力实现。native/ 与 builtin/ 各返回一个。
//
// 用结构体而不是接口：适配器只需要把自己做了的能力填进对应字段，
// 没做的留 nil，不必为「没做的那几项」写一堆返回 ErrNotSupported 的样板。
// 谁缺了什么由 New 统一判定并给出人话错误。
type Set struct {
	Xattr   Xattr
	Sparse  SparseFile
	Streams NamedStream
	IDs     StableFileID
	Times   CreationTime
	DOS     DOSAttributes

	// Migration 是 rename/remove 的旁路元数据迁移/清理实现。
	//
	// 与上面六项不同，它**不参与矩阵**（没有对应的 Capability，探测也不问它）：
	// 它不是「宿主有没有这个能力」的问题，而是「谁在按路径记账」的伴随义务。
	// builtin 必须提供真实现；native 的元数据长在宿主对象上，提供的是
	// 文档化的无操作。留 nil 时 Provider.Migration() 会回退到另一侧或 nil，
	// vfs 层对 nil 有防御。
	Migration MetadataMigration

	// Close 释放这套实现持有的资源（builtin 的旁路 KV 句柄等），可为 nil。
	Close func() error
}

// has 判断这套实现里有没有能力 c。
func (s Set) has(c Capability) bool {
	switch c {
	case CapXattr:
		return s.Xattr != nil
	case CapSparseFile:
		return s.Sparse != nil
	case CapNamedStream:
		return s.Streams != nil
	case CapStableFileID:
		return s.IDs != nil
	case CapCreationTime:
		return s.Times != nil
	case CapDOSAttributes:
		return s.DOS != nil
	default:
		return false
	}
}

// Factory 构造一套能力实现。native/ 与 builtin/ 各导出一个。
//
// 只有真正被矩阵用到的那一侧才会被调用：portable 模式下 native factory
// 一次都不会被调（连它的构造副作用都不该发生）。
type Factory func(Options) (Set, error)

// Provider 是组装完成的能力集合，供 vfs 层使用。
//
// 六个能力访问器**保证非 nil** —— 这是本包对上层的核心承诺，也是
// 「builtin 必须完整」那条铁律的兑现形式：拿到 Provider 之后调用方
// 不需要判 nil，也不需要类型断言，直接用。
// 缺项在 New 就已经硬失败了，绝不会漏到运行期变成一个空指针。
type Provider interface {
	Xattr() Xattr
	Sparse() SparseFile
	Streams() NamedStream
	IDs() StableFileID
	Times() CreationTime
	DOS() DOSAttributes

	// Migration 返回旁路元数据的迁移/清理实现，可能为 nil。
	//
	// 为什么允许 nil 而六个能力访问器不允许：Migration 不在矩阵里，
	// 「两侧都没给」在语义上是合法的（比如测试注入的最小 Provider），
	// 硬失败反而把一件「没有旁路账本就没有迁移义务」的事变成了错误。
	// vfs 层的调用点全部做了 nil 防御。
	Migration() MetadataMigration

	// Matrix 返回**实际生效**的矩阵。
	//
	// 注意它可能与 SelectMatrix 算出来的那个不同：auto 模式下探测说支持、
	// 但 native adapter 实际没提供该能力时，这里会如实报 builtin。
	// 日志与测试都该以这个为准。
	Matrix() Matrix

	// Close 释放两侧适配器持有的资源。
	Close() error
}

// IncompleteBuiltinError 表示 builtin 侧缺了能力实现。
//
// 这是**编程错误**而不是环境问题，所以措辞直接冲着实现者去：
// AGENTS.md §1.2 明文规定「每一项能力都必须有 builtin 实现，不许赊账」，
// 因为将来移植到未知系统时 builtin 是唯一底座，缺一项等于那个平台整个不可用。
type IncompleteBuiltinError struct {
	Caps []Capability
}

func (e *IncompleteBuiltinError) Error() string {
	return fmt.Sprintf(
		"oscap: builtin 适配器缺少能力实现: %s"+
			"（AGENTS.md §1.2：builtin 必须完整，每一项能力都要有 builtin 实现，不许赊账）",
		capList(e.Caps))
}

// New 按矩阵组装 Provider。
//
// native 可以为 nil（表示这个构建里没有 native 侧）；builtin 为 nil 时，
// 只要有任何一项需要落到 builtin 就会失败。
func New(m Matrix, o Options, native, builtin Factory) (Provider, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}

	p := &provider{eff: Matrix{mode: m.mode}}

	// --- 1. 只在真正需要时才向 native 侧索取实现。
	var nativeSet Set
	if len(m.Caps(KindNative)) > 0 && native != nil {
		ns, err := native(o)
		if err != nil {
			// native 侧整体不可用，全部落到 builtin。不是静默吞掉 ——
			// 调用方能从 Provider.Matrix() 看到结果是 builtin，
			// 装配方应当把 err 记进启动日志（New 不做日志，见包分层）。
			ns = Set{}
		}
		nativeSet = ns
	}

	// --- 2. 逐项挑选，native 拿不到就退到 builtin。
	var wantBuiltin []Capability
	var builtinSet Set
	for c := Capability(0); c < capCount; c++ {
		if m.Kind(c) == KindNative && nativeSet.has(c) {
			p.eff.kind[c] = KindNative
			continue
		}
		p.eff.kind[c] = KindBuiltin
		wantBuiltin = append(wantBuiltin, c)
	}

	// --- 3. builtin 侧必须能补齐剩下的全部。
	if len(wantBuiltin) > 0 {
		if builtin == nil {
			return nil, &IncompleteBuiltinError{Caps: wantBuiltin}
		}
		bs, err := builtin(o)
		if err != nil {
			return nil, fmt.Errorf("oscap: 构造 builtin 适配器失败: %w", err)
		}
		builtinSet = bs
		var missing []Capability
		for _, c := range wantBuiltin {
			if !builtinSet.has(c) {
				missing = append(missing, c)
			}
		}
		if missing != nil {
			// 已经拿到手的资源要还回去，不能因为组装失败就泄漏。
			closeSet(builtinSet)
			return nil, &IncompleteBuiltinError{Caps: missing}
		}
		p.fill(builtinSet, KindBuiltin)
		p.closers = append(p.closers, builtinSet.Close)
	}
	p.fill(nativeSet, KindNative)
	p.closers = append(p.closers, nativeSet.Close)

	// Migration 不走矩阵：builtin 优先（账本在它手里），native 的无操作兜底。
	// 见 provider.Migration 的注释。
	p.mig = builtinSet.Migration
	if p.mig == nil {
		p.nativeMig = nativeSet.Migration
	}

	return p, nil
}

// Open 是「算矩阵 + 组装」的一步到位版本，装配层（cmd/）用这个。
func Open(mode Mode, o Options, native, builtin Factory) (Provider, error) {
	return OpenWithProbe(mode, o, nil, native, builtin)
}

// OpenWithProbe 与 Open 相同，但允许注入探测函数。
//
// 存在的理由是测试：真实探测的结果取决于构建机的文件系统，
// 拿它做断言等于把测试交给运气（AGENTS.md「验收判据必须可证伪」）。
func OpenWithProbe(mode Mode, o Options, probe Prober, native, builtin Factory) (Provider, error) {
	m, err := SelectMatrix(mode, o, probe)
	if err != nil {
		return nil, err
	}
	return New(m, o, native, builtin)
}

// provider 是 Provider 的唯一实现。
type provider struct {
	eff Matrix

	xattr   Xattr
	sparse  SparseFile
	streams NamedStream
	ids     StableFileID
	times   CreationTime
	dos     DOSAttributes

	mig       MetadataMigration
	nativeMig MetadataMigration

	closers []func() error
}

// fill 把 s 里那些**本次生效方是 k** 的能力填进 provider。
//
// 只填 eff 指名的那一侧：native set 里可能带着一堆能力，但矩阵只让它做两项，
// 剩下的必须由 builtin 提供，不能被顺手覆盖掉 —— 否则 portable/降级
// 的语义就是假的（"我配了 builtin，跑的却是 native"）。
func (p *provider) fill(s Set, k Kind) {
	if s.Xattr != nil && p.eff.kind[CapXattr] == k {
		p.xattr = s.Xattr
	}
	if s.Sparse != nil && p.eff.kind[CapSparseFile] == k {
		p.sparse = s.Sparse
	}
	if s.Streams != nil && p.eff.kind[CapNamedStream] == k {
		p.streams = s.Streams
	}
	if s.IDs != nil && p.eff.kind[CapStableFileID] == k {
		p.ids = s.IDs
	}
	if s.Times != nil && p.eff.kind[CapCreationTime] == k {
		p.times = s.Times
	}
	if s.DOS != nil && p.eff.kind[CapDOSAttributes] == k {
		p.dos = s.DOS
	}
}

func (p *provider) Xattr() Xattr         { return p.xattr }
func (p *provider) Sparse() SparseFile   { return p.sparse }
func (p *provider) Streams() NamedStream { return p.streams }
func (p *provider) IDs() StableFileID    { return p.ids }
func (p *provider) Times() CreationTime  { return p.times }
func (p *provider) DOS() DOSAttributes   { return p.dos }
func (p *provider) Matrix() Matrix       { return p.eff }

// Migration 返回迁移/清理实现。
//
// **builtin 优先**：旁路账本只有 builtin 有，native 的实现是无操作。
// 混合矩阵下选 builtin 的不会多做事也不会少做事 —— native 那侧本来
// 就没有账可迁。builtin 没给（理论上不可能，缺项在 New 已硬失败）
// 才回退到 native 的无操作，再不行就是 nil，由调用方防御。
func (p *provider) Migration() MetadataMigration {
	if p.mig != nil {
		return p.mig
	}
	return p.nativeMig
}

// Close 关闭两侧适配器。
//
// **不在第一个错误处提前返回**：另一侧的资源同样要还。
// 多个错误全部汇总上报，别让第二个泄漏被第一个错误盖住。
func (p *provider) Close() error {
	var msgs []string
	for _, c := range p.closers {
		if c == nil {
			continue
		}
		if err := c(); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("oscap: 关闭适配器失败: %s", strings.Join(msgs, "; "))
}

// closeSet 在组装中途失败时把已构造的一套实现还回去。
func closeSet(s Set) {
	if s.Close != nil {
		_ = s.Close()
	}
}
