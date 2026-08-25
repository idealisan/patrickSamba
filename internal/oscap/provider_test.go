package oscap

import (
	"errors"
	"strings"
	"testing"
)

// factoryOf 把一套固定实现包成 Factory，并记调用次数。
func factoryOf(s Set, calls *int) Factory {
	return func(Options) (Set, error) {
		if calls != nil {
			*calls++
		}
		return s, nil
	}
}

// assertAllAccessorsNonNil 兑现 Provider 的核心承诺：六个访问器都非 nil。
func assertAllAccessorsNonNil(t *testing.T, p Provider) {
	t.Helper()
	if p.Xattr() == nil {
		t.Error("Provider.Xattr() 为 nil")
	}
	if p.Sparse() == nil {
		t.Error("Provider.Sparse() 为 nil")
	}
	if p.Streams() == nil {
		t.Error("Provider.Streams() 为 nil")
	}
	if p.IDs() == nil {
		t.Error("Provider.IDs() 为 nil")
	}
	if p.Times() == nil {
		t.Error("Provider.Times() 为 nil")
	}
	if p.DOS() == nil {
		t.Error("Provider.DOS() 为 nil")
	}
}

// TestNewPicksPerCapability 是组装环节的核心断言：
// 矩阵说哪一侧，就必须真的用哪一侧的**那个实例**。
//
// 光看 Matrix() 报的 kind 是不够的 —— 那只是一份声明。这里用 stub 的 id
// 去认实例，确保「声称走 builtin」和「真的走 builtin」是同一件事。
// 本项目栽过太多次「成功回显 ≠ 事情真的发生」。
func TestNewPicksPerCapability(t *testing.T) {
	o := testOptions(t)
	m := NewMatrix(ModeAuto, map[Capability]Kind{
		CapXattr:       KindNative,
		CapNamedStream: KindNative,
	})

	p, err := New(m, o, factoryOf(fullSet("native"), nil), factoryOf(fullSet("builtin"), nil))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer func() { _ = p.Close() }()

	assertAllAccessorsNonNil(t, p)

	if got := stubID(p.Xattr()); got != "native" {
		t.Errorf("xattr 应挑中 native 实例，实际 %q", got)
	}
	if got := stubID(p.Streams()); got != "native" {
		t.Errorf("named_stream 应挑中 native 实例，实际 %q", got)
	}
	for name, got := range map[string]string{
		"sparse_file":    stubID(p.Sparse()),
		"stable_file_id": stubID(p.IDs()),
		"creation_time":  stubID(p.Times()),
		"dos_attributes": stubID(p.DOS()),
	} {
		if got != "builtin" {
			t.Errorf("%s 应挑中 builtin 实例，实际 %q", name, got)
		}
	}
}

// TestPortableIgnoresNativeEvenWhenAvailable 是 portable 的可证伪判据。
//
// 关键点：native factory **提供了全部六项**，而且完全可用。
// portable 仍然必须一项都不用它 —— 否则"我配了 portable，跑的却是 native"
// 这种最难发现的假象就成立了。顺带断言 native factory 压根没被调用。
func TestPortableIgnoresNativeEvenWhenAvailable(t *testing.T) {
	nativeCalls := 0
	m := NewMatrix(ModePortable, nil)

	p, err := New(m, testOptions(t),
		factoryOf(fullSet("native"), &nativeCalls),
		factoryOf(fullSet("builtin"), nil))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer func() { _ = p.Close() }()

	if nativeCalls != 0 {
		t.Errorf("portable 模式调用了 %d 次 native factory，应当一次都不调", nativeCalls)
	}
	for name, got := range map[string]string{
		"xattr":          stubID(p.Xattr()),
		"sparse_file":    stubID(p.Sparse()),
		"named_stream":   stubID(p.Streams()),
		"stable_file_id": stubID(p.IDs()),
		"creation_time":  stubID(p.Times()),
		"dos_attributes": stubID(p.DOS()),
	} {
		if got != "builtin" {
			t.Errorf("portable 模式下 %s 用的是 %q，应当全部为 builtin", name, got)
		}
	}
}

// TestAutoFallsBackWhenNativeLacksCapability：
// auto 模式下探测说支持、但 native adapter 实际没给这一项时，
// 必须落到 builtin，并且 Matrix() 要**如实**报 builtin。
//
// 报得不实比降级本身更危险：日志说 native、跑的是 builtin，
// 排查性能问题时会往完全错误的方向查。
func TestAutoFallsBackWhenNativeLacksCapability(t *testing.T) {
	partial := fullSet("native")
	partial.Times = nil // native 侧没实现创建时间

	m := NewMatrix(ModeAuto, map[Capability]Kind{
		CapXattr:        KindNative,
		CapCreationTime: KindNative,
	})

	p, err := New(m, testOptions(t), factoryOf(partial, nil), factoryOf(fullSet("builtin"), nil))
	if err != nil {
		t.Fatalf("auto 模式应当降级而不是报错: %v", err)
	}
	defer func() { _ = p.Close() }()

	if got := stubID(p.Times()); got != "builtin" {
		t.Errorf("creation_time 应降级到 builtin，实际 %q", got)
	}
	if got := p.Matrix().Kind(CapCreationTime); got != KindBuiltin {
		t.Errorf("Matrix() 应如实报 builtin，实际报 %s", got)
	}
	if got := p.Matrix().Kind(CapXattr); got != KindNative {
		t.Errorf("xattr 未受影响，应仍为 native，实际 %s", got)
	}
}

// TestBuiltinMustBeComplete 兑现 AGENTS.md §1.2 的第二条铁律。
//
// 为什么要在**组装期**硬失败而不是运行期返回 ErrNotSupported：
// builtin 是移植到未知系统时的唯一底座，缺一项等于那个平台整个不可用。
// 让它在启动时就炸，比等到现场用到那个功能才发现强得多。
func TestBuiltinMustBeComplete(t *testing.T) {
	incomplete := fullSet("builtin")
	incomplete.DOS = nil
	incomplete.Sparse = nil

	closed := false
	bf := func(Options) (Set, error) {
		s := incomplete
		s.Close = func() error { closed = true; return nil }
		return s, nil
	}

	_, err := New(NewMatrix(ModePortable, nil), testOptions(t), nil, bf)
	if err == nil {
		t.Fatal("builtin 缺项却组装成功了")
	}
	var ie *IncompleteBuiltinError
	if !errors.As(err, &ie) {
		t.Fatalf("错误类型应为 *IncompleteBuiltinError，实际 %T: %v", err, err)
	}
	want := []Capability{CapSparseFile, CapDOSAttributes}
	if len(ie.Caps) != len(want) {
		t.Fatalf("IncompleteBuiltinError.Caps = %v, 期望 %v", ie.Caps, want)
	}
	for i := range want {
		if ie.Caps[i] != want[i] {
			t.Fatalf("IncompleteBuiltinError.Caps = %v, 期望 %v", ie.Caps, want)
		}
	}
	// 组装中途失败也要把已经拿到手的资源还回去。
	if !closed {
		t.Error("组装失败时没有关闭已构造的 builtin set —— 资源泄漏")
	}
}

func TestBuiltinFactoryNilIsIncomplete(t *testing.T) {
	_, err := New(NewMatrix(ModePortable, nil), testOptions(t), factoryOf(fullSet("native"), nil), nil)
	var ie *IncompleteBuiltinError
	if !errors.As(err, &ie) {
		t.Fatalf("builtin factory 为 nil 时应报 *IncompleteBuiltinError，实际 %T: %v", err, err)
	}
	if len(ie.Caps) != int(capCount) {
		t.Errorf("应当报出全部 %d 项缺失，实际 %v", capCount, ie.Caps)
	}
}

// TestAutoSurvivesNativeFactoryError：auto 模式下 native 侧整体构造失败，
// 服务仍应起得来（全部落到 builtin），因为 builtin 是完整的。
func TestAutoSurvivesNativeFactoryError(t *testing.T) {
	nf := func(Options) (Set, error) { return Set{}, errors.New("模拟 native 初始化失败") }

	p, err := New(NewMatrix(ModeAuto, map[Capability]Kind{CapXattr: KindNative}),
		testOptions(t), nf, factoryOf(fullSet("builtin"), nil))
	if err != nil {
		t.Fatalf("auto 模式下 native 构造失败应降级而不是起不来: %v", err)
	}
	defer func() { _ = p.Close() }()

	if got := stubID(p.Xattr()); got != "builtin" {
		t.Errorf("应全部落到 builtin，xattr 实际 %q", got)
	}
	if got := p.Matrix().Kind(CapXattr); got != KindBuiltin {
		t.Errorf("Matrix() 应如实报 builtin，实际 %s", got)
	}
}

// TestCloseClosesBothSidesAndAggregates：一侧关闭失败不得掩盖另一侧。
func TestCloseClosesBothSidesAndAggregates(t *testing.T) {
	nativeClosed, builtinClosed := false, false

	ns := fullSet("native")
	ns.Close = func() error { nativeClosed = true; return errors.New("native 关闭失败") }
	bs := fullSet("builtin")
	bs.Close = func() error { builtinClosed = true; return errors.New("builtin 关闭失败") }

	p, err := New(NewMatrix(ModeAuto, map[Capability]Kind{CapXattr: KindNative}),
		testOptions(t), factoryOf(ns, nil), factoryOf(bs, nil))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	cerr := p.Close()
	if cerr == nil {
		t.Fatal("两侧都关闭失败，Close 却返回 nil")
	}
	if !nativeClosed || !builtinClosed {
		t.Errorf("两侧都要关：native=%v builtin=%v（不能在第一个错误处提前返回）",
			nativeClosed, builtinClosed)
	}
	if !strings.Contains(cerr.Error(), "native 关闭失败") ||
		!strings.Contains(cerr.Error(), "builtin 关闭失败") {
		t.Errorf("两条失败都要出现在汇总错误里: %v", cerr)
	}
}

func TestCloseNilClosersIsSafe(t *testing.T) {
	p, err := New(NewMatrix(ModePortable, nil), testOptions(t), nil, factoryOf(fullSet("builtin"), nil))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Set.Close 为 nil 时 Close() 应返回 nil，实际 %v", err)
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(NewMatrix(ModePortable, nil), Options{}, nil, factoryOf(fullSet("b"), nil)); err == nil {
		t.Fatal("Root 为空应当报错")
	}
}

// TestOpenWithProbeEndToEnd 走一遍「算矩阵 + 组装」的完整入口。
func TestOpenWithProbeEndToEnd(t *testing.T) {
	pr := &countingProbe{native: map[Capability]bool{CapXattr: true, CapNamedStream: true}}

	p, err := OpenWithProbe(ModeAuto, testOptions(t), pr.probe,
		factoryOf(fullSet("native"), nil), factoryOf(fullSet("builtin"), nil))
	if err != nil {
		t.Fatalf("OpenWithProbe 失败: %v", err)
	}
	defer func() { _ = p.Close() }()

	const want = "auto: xattr=native sparse_file=builtin named_stream=native " +
		"stable_file_id=builtin creation_time=builtin dos_attributes=builtin"
	if got := p.Matrix().String(); got != want {
		t.Errorf("生效矩阵\n实际: %s\n期望: %s", got, want)
	}
}
