package builtin

// portable_test.go —— **portable 模式在 CI 里真跑一遍**（AGENTS.md §1.2 前置要求）。
//
// 原文：「`portable` 模式必须在 CI 里真跑一遍，不能只是配置项里多一个取值。
// 一条在 CI 里从未被执行过的路径，到需要它的那天一定是坏的。
// 没有 CI 覆盖的 builtin 就是一份薛定谔的实现，写了等于没写。」
//
// 所以本文件不测 builtin 的内部函数，而是**从 oscap 的公开入口进去**：
// OpenWithProbe(ModePortable, …) → 六个访问器全非 nil → 六项能力逐个往返。
// 这条链路等价于生产装配（cmd/ 里那一句），任何一环断掉都会在这里红。

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// probeAllFalse 说「宿主什么可选能力都没有」。
//
// 缺项校验的用例用它而不是全 true：全 true 时矩阵会把能力指给 native，
// 于是「builtin 缺了这项」根本不会被问到，用例会因为走错分支而假绿。
func probeAllFalse(oscap.Capability, oscap.Options) bool { return false }

// countingProbe 返回一个「什么都说支持」的探测器，外加它被调用了几次。
//
// 全 true 是刻意的：portable 模式的承诺是**完全不看探测结果**。如果实现哪天偷偷
// 改成「探测说支持就走 native」，全 true 会让它一头撞上 nativeMustNotBeUsed。
// 用真实探测反而测不出这件事（构建机上探测本来就大多返回同一个值，
// 断言等于交给运气 —— AGENTS.md「验收判据必须可证伪」）。
//
// 计数器是比「结果矩阵全 builtin」更强的判据：矩阵只说**结论**对，
// 计数器说**过程**也对 —— portable 连问都不该问。这个区别不是洁癖：
// 真实探测会去碰宿主文件系统（建临时文件、试 xattr、试 fallocate），
// 而 portable 的存在意义正是「在不能碰这些东西的环境里也能跑」。
// 一个偷偷探测了的 portable，会在只读根 / 古怪 NAS 固件上当场炸掉，
// 而所有只看矩阵的测试都会显示绿灯。
func countingProbe(n *int) oscap.Prober {
	return func(oscap.Capability, oscap.Options) bool {
		*n++
		return true
	}
}

// nativeMustNotBeUsed 是一个一旦被调用就让用例失败的 native 工厂。
//
// portable 模式下 native 工厂**连构造都不该发生**（provider.go 明文写了这条），
// 这是比「结果矩阵全是 builtin」更强的判据：它连副作用都不允许。
func nativeMustNotBeUsed(t *testing.T) oscap.Factory {
	return func(oscap.Options) (oscap.Set, error) {
		t.Errorf("portable 模式下不该构造 native 适配器")
		return oscap.Set{}, errors.New("native 不该被调用")
	}
}

func TestPortableModeAllCapabilities(t *testing.T) {
	root := t.TempDir()
	opts := oscap.Options{
		Root:         root,
		MetadataPath: filepath.Join(t.TempDir(), "oscap.db"),
	}

	probeCalls := 0
	p, err := oscap.OpenWithProbe(oscap.ModePortable, opts,
		countingProbe(&probeCalls), nativeMustNotBeUsed(t), New)
	if err != nil {
		t.Fatalf("portable 模式装配失败: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close 失败: %v", err)
		}
	}()

	// --- 0. portable 模式下探测器一次都不该被调用。
	// 见 countingProbe 的注释：这是「过程」层面的断言，矩阵断言替代不了。
	if probeCalls != 0 {
		t.Errorf("portable 模式下探测器被调用了 %d 次，应为 0 次", probeCalls)
	}

	// --- 1. 六个访问器保证非 nil（provider.go 对上层的核心承诺）。
	accessors := map[string]any{
		"Xattr":   p.Xattr(),
		"Sparse":  p.Sparse(),
		"Streams": p.Streams(),
		"IDs":     p.IDs(),
		"Times":   p.Times(),
		"DOS":     p.DOS(),
	}
	for name, v := range accessors {
		if v == nil {
			t.Fatalf("%s() 返回 nil —— builtin 缺项本该在 New 就硬失败", name)
		}
	}

	// --- 2. 生效矩阵必须逐项都是 builtin。
	m := p.Matrix()
	if m.Mode() != oscap.ModePortable {
		t.Fatalf("模式不对: %v", m.Mode())
	}
	for _, c := range oscap.Capabilities() {
		if k := m.Kind(c); k != oscap.KindBuiltin {
			t.Errorf("portable 模式下 %s 走了 %s", c, k)
		}
	}
	if n := len(m.Caps(oscap.KindNative)); n != 0 {
		t.Errorf("portable 模式下不该有任何 native 项，实得 %d 项", n)
	}

	// --- 3. 六项能力逐个往返（这才是「真跑一遍」）。
	ref := mkfile(t, root, "band")

	t.Run("xattr", func(t *testing.T) {
		if err := p.Xattr().SetXattr(ref, "com.apple.FinderInfo", []byte("finder")); err != nil {
			t.Fatalf("SetXattr: %v", err)
		}
		v, err := p.Xattr().GetXattr(ref, "com.apple.FinderInfo")
		if err != nil || !bytes.Equal(v, []byte("finder")) {
			t.Fatalf("GetXattr: (%q, %v)", v, err)
		}
		names, err := p.Xattr().ListXattr(ref)
		if err != nil || len(names) != 1 || names[0] != "com.apple.FinderInfo" {
			t.Fatalf("ListXattr: (%v, %v)", names, err)
		}
		if err := p.Xattr().RemoveXattr(ref, "com.apple.FinderInfo"); err != nil {
			t.Fatalf("RemoveXattr: %v", err)
		}
	})

	t.Run("sparse", func(t *testing.T) {
		if err := p.Sparse().SetSparse(ref, true); err != nil {
			t.Fatalf("SetSparse: %v", err)
		}
		if err := p.Sparse().Preallocate(ref, 0, zeroBlockSize); err != nil {
			t.Fatalf("Preallocate: %v", err)
		}
		if err := p.Sparse().PunchHole(ref, 0, zeroBlockSize); err != nil {
			t.Fatalf("PunchHole: %v", err)
		}
		got, err := p.Sparse().AllocatedRanges(ref, 0, 2*zeroBlockSize)
		if err != nil {
			t.Fatalf("AllocatedRanges: %v", err)
		}
		if len(got) != 1 || got[0] != (oscap.Range{Offset: zeroBlockSize, Length: zeroBlockSize}) {
			t.Fatalf("AllocatedRanges 结果不对: %v", got)
		}
	})

	t.Run("streams", func(t *testing.T) {
		h, err := p.Streams().OpenStream(ref, "AFP_Resource",
			oscap.StreamRead|oscap.StreamWrite|oscap.StreamCreate)
		if err != nil {
			t.Fatalf("OpenStream: %v", err)
		}
		if _, err := h.WriteAt([]byte("rsrc"), 0); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := h.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		list, err := p.Streams().ListStreams(ref)
		if err != nil || len(list) != 1 || list[0].Name != "AFP_Resource" || list[0].Size != 4 {
			t.Fatalf("ListStreams: (%v, %v)", list, err)
		}
		if err := p.Streams().RemoveStream(ref, "AFP_Resource"); err != nil {
			t.Fatalf("RemoveStream: %v", err)
		}
	})

	t.Run("file_id", func(t *testing.T) {
		id, err := p.IDs().FileID(ref)
		if err != nil {
			t.Fatalf("FileID: %v", err)
		}
		if id == 0 {
			t.Fatal("FileID 不该是 0")
		}
		other, err := p.IDs().FileID(mkfile(t, root, "other"))
		if err != nil {
			t.Fatalf("FileID: %v", err)
		}
		if other == id {
			t.Fatalf("两个对象拿到同一个 FileID %d", id)
		}
	})

	t.Run("creation_time", func(t *testing.T) {
		want := time.Date(2020, 1, 2, 3, 4, 5, 600000000, time.UTC)
		if err := p.Times().SetCreationTime(ref, want); err != nil {
			t.Fatalf("SetCreationTime: %v", err)
		}
		got, err := p.Times().CreationTime(ref)
		if err != nil || !got.Equal(want) {
			t.Fatalf("CreationTime: (%v, %v)", got, err)
		}
	})

	t.Run("dos_attributes", func(t *testing.T) {
		if err := p.DOS().SetDOSAttributes(ref, 0x22); err != nil {
			t.Fatalf("SetDOSAttributes: %v", err)
		}
		got, err := p.DOS().DOSAttributes(ref)
		if err != nil || got != 0x22 {
			t.Fatalf("DOSAttributes: (%#x, %v)", got, err)
		}
	})
}

// TestPortableAutoModeFallsBackToBuiltin 覆盖另一条真实路径：auto 模式下探测说
// 「什么都不支持」，六项应当全部落到 builtin 且照样可用。
//
// 这条与上面那条不重复：portable 是**不看探测**，auto 是**看了探测之后降级**，
// 两者在 provider.go 里走的是不同分支。
func TestPortableAutoModeFallsBackToBuiltin(t *testing.T) {
	opts := oscap.Options{
		Root:         t.TempDir(),
		MetadataPath: filepath.Join(t.TempDir(), "oscap.db"),
	}
	probeNone := func(oscap.Capability, oscap.Options) bool { return false }

	p, err := oscap.OpenWithProbe(oscap.ModeAuto, opts, probeNone, nativeMustNotBeUsed(t), New)
	if err != nil {
		t.Fatalf("auto 模式装配失败: %v", err)
	}
	defer func() { _ = p.Close() }()

	for _, c := range oscap.Capabilities() {
		if k := p.Matrix().Kind(c); k != oscap.KindBuiltin {
			t.Errorf("探测全否时 %s 应落到 builtin，实得 %s", c, k)
		}
	}
	ref := mkfile(t, opts.Root, "a.bin")
	if err := p.Xattr().SetXattr(ref, "k", []byte("v")); err != nil {
		t.Fatalf("降级后的实现不可用: %v", err)
	}
}

// TestPortableBuiltinIsComplete 直接盯住那条铁律：New 返回的 Set 六项一个都不能缺。
//
// 编译期断言只能保证类型实现了接口，保证不了**字段被填进 Set**——
// 少写一行 `Times: a` 编译照样过，然后 oscap.New 在运行期抛 IncompleteBuiltinError。
func TestPortableBuiltinIsComplete(t *testing.T) {
	set, err := New(oscap.Options{
		Root:         t.TempDir(),
		MetadataPath: filepath.Join(t.TempDir(), "oscap.db"),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer func() { _ = set.Close() }()

	missing := []string{}
	if set.Xattr == nil {
		missing = append(missing, "Xattr")
	}
	if set.Sparse == nil {
		missing = append(missing, "Sparse")
	}
	if set.Streams == nil {
		missing = append(missing, "Streams")
	}
	if set.IDs == nil {
		missing = append(missing, "IDs")
	}
	if set.Times == nil {
		missing = append(missing, "Times")
	}
	if set.DOS == nil {
		missing = append(missing, "DOS")
	}
	if set.Close == nil {
		missing = append(missing, "Close")
	}
	if len(missing) > 0 {
		t.Fatalf("builtin 缺项: %v（AGENTS.md §1.2：不许赊账）", missing)
	}
}

// TestPortableIncompleteBuiltinIsRejected 是 IncompleteBuiltinError 的**反向对照**。
//
// 上面那条 TestPortableBuiltinIsComplete 只证明「今天的 New 没缺项」。它证明不了
// 「真缺项时会被拦下来」—— 那条路径在整个测试套件里从未被执行过。
// AGENTS.md §1.2：「一个从来没红过的门禁，和没有门禁是一回事。」
//
// 这个区别是有实体的：provider.go 的完整性校验写在 `if len(wantBuiltin) > 0` 里面，
// 只要有人把那层条件改错、或者把 `missing != nil` 写成 `len(missing) > 0` 之外的
// 什么形态，缺项就会**静默漏过去**，然后在运行期变成 Provider 访问器返回 nil，
// 上层拿到一个空指针 —— 而 TestPortableBuiltinIsComplete 全程绿灯。
//
// 做法：拿真实的 New，逐项把一个字段抠成 nil 再喂给 OpenWithProbe。
// 六项各来一遍，确保**每一项**都在校验覆盖之内（只测一项等于假设六项走同一段
// 代码，而 Set.has 恰好是逐项 switch —— 少写一个 case 就漏一项）。
func TestPortableIncompleteBuiltinIsRejected(t *testing.T) {
	// 每项能力对应一个「把它抠掉」的手术刀。
	holes := []struct {
		cap   oscap.Capability
		punch func(*oscap.Set)
	}{
		{oscap.CapXattr, func(s *oscap.Set) { s.Xattr = nil }},
		{oscap.CapSparseFile, func(s *oscap.Set) { s.Sparse = nil }},
		{oscap.CapNamedStream, func(s *oscap.Set) { s.Streams = nil }},
		{oscap.CapStableFileID, func(s *oscap.Set) { s.IDs = nil }},
		{oscap.CapCreationTime, func(s *oscap.Set) { s.Times = nil }},
		{oscap.CapDOSAttributes, func(s *oscap.Set) { s.DOS = nil }},
	}

	for _, h := range holes {
		t.Run(h.cap.String(), func(t *testing.T) {
			// closed 记录残缺 Set 的 Close 有没有被调用。
			// provider.go 在组装失败时必须把已经拿到手的资源还回去
			// （builtin 持有 bbolt 句柄，泄漏会让后续开库直接卡住）。
			closed := false
			crippled := func(o oscap.Options) (oscap.Set, error) {
				s, err := New(o)
				if err != nil {
					return s, err
				}
				inner := s.Close
				s.Close = func() error {
					closed = true
					if inner != nil {
						return inner()
					}
					return nil
				}
				h.punch(&s)
				return s, nil
			}

			opts := oscap.Options{
				Root:         t.TempDir(),
				MetadataPath: filepath.Join(t.TempDir(), "oscap.db"),
			}
			p, err := oscap.OpenWithProbe(oscap.ModePortable, opts,
				probeAllFalse, nativeMustNotBeUsed(t), crippled)
			if err == nil {
				_ = p.Close()
				t.Fatalf("缺了 %s 却装配成功了 —— 完整性校验形同虚设", h.cap)
			}
			if p != nil {
				t.Errorf("装配失败时应返回 nil Provider，实得 %v", p)
			}

			// 必须是 IncompleteBuiltinError，而且**指名道姓**说缺的是哪一项。
			// 只判「err != nil」是不够的：任何一个无关错误（建库失败、
			// 参数校验）都会让那种断言变绿，而缺项校验可能根本没跑到。
			var ibe *oscap.IncompleteBuiltinError
			if !errors.As(err, &ibe) {
				t.Fatalf("缺 %s 应得 *IncompleteBuiltinError，实得 %T: %v", h.cap, err, err)
			}
			if len(ibe.Caps) != 1 || ibe.Caps[0] != h.cap {
				t.Errorf("报告的缺项不对: 得 %v，期望恰好 [%s]", ibe.Caps, h.cap)
			}
			if !strings.Contains(ibe.Error(), h.cap.String()) {
				t.Errorf("错误文本里没提到 %s，运维照着搜不到: %s", h.cap, ibe.Error())
			}
			if !closed {
				t.Errorf("组装失败时没有关闭已构造的 builtin Set —— bbolt 句柄泄漏")
			}
		})
	}
}

// TestPortableNilBuiltinFactoryIsRejected 覆盖同一条校验的另一个入口：
// builtin 工厂整个为 nil。这在 provider.go 里是**另一个 return 点**
// （`if builtin == nil` 那支），不与上面那条共用代码路径。
func TestPortableNilBuiltinFactoryIsRejected(t *testing.T) {
	opts := oscap.Options{
		Root:         t.TempDir(),
		MetadataPath: filepath.Join(t.TempDir(), "oscap.db"),
	}
	p, err := oscap.OpenWithProbe(oscap.ModePortable, opts,
		probeAllFalse, nativeMustNotBeUsed(t), nil)
	if err == nil {
		_ = p.Close()
		t.Fatal("builtin 工厂为 nil 却装配成功了")
	}
	var ibe *oscap.IncompleteBuiltinError
	if !errors.As(err, &ibe) {
		t.Fatalf("应得 *IncompleteBuiltinError，实得 %T: %v", err, err)
	}
	// 一项都没有时必须把六项全报出来，别只报第一项让人以为改一个就好了。
	if len(ibe.Caps) != len(oscap.Capabilities()) {
		t.Errorf("builtin 全缺时应报满 %d 项，实得 %d 项: %v",
			len(oscap.Capabilities()), len(ibe.Caps), ibe.Caps)
	}
}

func TestPortableNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(oscap.Options{}); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("空 Root 应得 ErrInvalidArg，实得 %v", err)
	}
}
