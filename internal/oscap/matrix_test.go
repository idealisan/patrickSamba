package oscap

import (
	"testing"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{Root: t.TempDir()}
}

// TestSelectMatrixPortableNeverProbes 钉住 portable 的承诺：
// 「完全不碰 OS 的可选能力」—— 连探测都不该碰。
//
// 探测本身会在共享根下建临时文件、往根目录写一个临时扩展属性。
// 一个号称 portable 的模式如果偷偷做了这些，在只读根/古怪文件系统上
// 就会以最难排查的方式出问题。所以这里直接断言探测调用次数为 0。
func TestSelectMatrixPortableNeverProbes(t *testing.T) {
	p := &countingProbe{native: allCaps()}

	m, err := SelectMatrix(ModePortable, testOptions(t), p.probe)
	if err != nil {
		t.Fatalf("SelectMatrix(portable) 失败: %v", err)
	}
	if p.calls != 0 {
		t.Errorf("portable 模式调用了 %d 次探测，应当一次都不调", p.calls)
	}
	for _, c := range Capabilities() {
		if got := m.Kind(c); got != KindBuiltin {
			t.Errorf("portable 模式下 %s = %s，应当全部为 builtin", c, got)
		}
	}
}

// TestSelectMatrixAutoIsPerCapability 是本包最核心的一条：
// **逐能力**降级，不是整体二选一（AGENTS.md §1.2 铁律 1）。
//
// 用的正是 AGENTS.md 举的那个例子：ext4 有 xattr 但拿不到可靠创建时间，
// 于是命名流走 native、创建时间走 builtin —— 同一次运行里两侧并存。
func TestSelectMatrixAutoIsPerCapability(t *testing.T) {
	p := &countingProbe{native: map[Capability]bool{
		CapXattr:       true,
		CapNamedStream: true,
		// creation_time / sparse_file / stable_file_id / dos_attributes 不支持
	}}

	m, err := SelectMatrix(ModeAuto, testOptions(t), p.probe)
	if err != nil {
		t.Fatalf("SelectMatrix(auto) 失败: %v", err)
	}
	if p.calls != int(capCount) {
		t.Errorf("auto 模式探测了 %d 次，应当每项能力各一次（共 %d）", p.calls, capCount)
	}

	for _, c := range Capabilities() {
		want := KindBuiltin
		if p.native[c] {
			want = KindNative
		}
		if got := m.Kind(c); got != want {
			t.Errorf("auto 模式下 %s = %s, 期望 %s", c, got, want)
		}
	}

	// 反向对照：如果两侧混合没生效（全 native 或全 builtin），
	// 上面的逐项断言可能因为"恰好都对"而失去意义，这里显式确认是混合的。
	if len(m.Caps(KindNative)) == 0 || len(m.Caps(KindBuiltin)) == 0 {
		t.Fatalf("这条用例要验的就是两侧并存，实际矩阵却是单侧的: %s", m)
	}
}

// TestNoModeForcesAllNative 钉住 v0.5 移除 native 档后的不变量：
// **不存在任何模式**会因「某项能力没有原生实现」而拒绝启动，
// 也不存在任何模式能构造出「全部能力强制走原生」的语义。
//
// native 档曾承诺「缺一项就启动报错」，但每个平台都至少有一项能力
// 被源码硬编码为不支持，该契约在任何平台上都无法满足（三平台恒定
// 启动失败），因此被整体移除。这条用例防止它以任何形式回来：
// 若将来有人重新引入一个「全原生强制」档，这里对全部合法取值做的
// 「探测全 false 也必须成功且全落 builtin」断言会当场变红。
func TestNoModeForcesAllNative(t *testing.T) {
	never := func(Capability, Options) bool { return false }
	for _, name := range ModeNames() {
		mode, err := ParseMode(name)
		if err != nil {
			t.Fatalf("ParseMode(%q) 失败: %v", name, err)
		}
		m, err := SelectMatrix(mode, testOptions(t), never)
		if err != nil {
			t.Fatalf("mode %s 在六项能力全部无原生实现时报错: %v", name, err)
		}
		for _, c := range Capabilities() {
			if got := m.Kind(c); got != KindBuiltin {
				t.Errorf("mode %s 下探测全失败时 %s = %s，应当落 builtin（安全侧）", name, c, got)
			}
		}
	}
}

func TestSelectMatrixRejectsEmptyRoot(t *testing.T) {
	if _, err := SelectMatrix(ModeAuto, Options{}, func(Capability, Options) bool { return true }); err == nil {
		t.Fatal("Root 为空应当报错")
	}
}

func TestSelectMatrixRejectsUnknownMode(t *testing.T) {
	if _, err := SelectMatrix(Mode(200), testOptions(t), nil); err == nil {
		t.Fatal("未知模式应当报错，而不是当成 auto 悄悄放行")
	}
}

// TestMatrixStringGolden 钉住摘要格式。
//
// 这一行会进启动日志，排查现场问题时运维往往只贴得出它；
// 格式变来变去就没法跨版本比对了。
func TestMatrixStringGolden(t *testing.T) {
	m := NewMatrix(ModeAuto, map[Capability]Kind{
		CapXattr:        KindNative,
		CapNamedStream:  KindNative,
		CapStableFileID: KindNative,
	})
	const want = "auto: xattr=native sparse_file=builtin named_stream=native " +
		"stable_file_id=native creation_time=builtin dos_attributes=builtin"
	if got := m.String(); got != want {
		t.Errorf("Matrix.String()\n实际: %s\n期望: %s", got, want)
	}

	empty := Matrix{}
	const wantEmpty = "auto: xattr=builtin sparse_file=builtin named_stream=builtin " +
		"stable_file_id=builtin creation_time=builtin dos_attributes=builtin"
	if got := empty.String(); got != wantEmpty {
		t.Errorf("Matrix 零值\n实际: %s\n期望: %s（零值必须落在 builtin 这一侧）", got, wantEmpty)
	}
}

func TestMatrixKindRejectsInvalidCapability(t *testing.T) {
	m := NewMatrix(ModeAuto, map[Capability]Kind{CapXattr: KindNative})
	if got := m.Kind(Capability(-1)); got != KindBuiltin {
		t.Errorf("非法 Capability 应返回 builtin（安全侧），实际 %s", got)
	}
	if got := m.Kind(capCount); got != KindBuiltin {
		t.Errorf("越界 Capability 应返回 builtin（安全侧），实际 %s", got)
	}
	if m.Mode() != ModeAuto {
		t.Errorf("Matrix.Mode() = %v, 期望 auto", m.Mode())
	}
}

func TestCapabilityNamesAreUniqueAndComplete(t *testing.T) {
	caps := Capabilities()
	if len(caps) != int(capCount) {
		t.Fatalf("Capabilities() 返回 %d 项，期望 %d", len(caps), capCount)
	}
	seen := make(map[string]Capability, len(caps))
	for _, c := range caps {
		n := c.String()
		if n == "" || n == "capability(?)" {
			t.Errorf("能力 %d 没有登记名字（capabilityNames 漏了一项）", int(c))
		}
		if prev, dup := seen[n]; dup {
			t.Errorf("能力名 %q 重复：%d 与 %d", n, int(prev), int(c))
		}
		seen[n] = c
		if !c.Valid() {
			t.Errorf("Capabilities() 返回了非法能力 %d", int(c))
		}
	}
	if got := Capability(capCount).String(); got != "capability(?)" {
		t.Errorf("越界能力的 String() = %q", got)
	}
}
