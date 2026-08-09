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
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// probeAllTrue 是一个「什么都说支持」的探测器。
//
// 刻意这么设：portable 模式的承诺是**完全不看探测结果**。用一个全 true 的探测器，
// 如果实现哪天偷偷改成「探测说支持就走 native」，这个用例会立刻抓到 ——
// 用真实探测反而测不出这件事（构建机上探测本来就大多返回 true 或 false，
// 断言等于交给运气）。
func probeAllTrue(oscap.Capability, oscap.Options) bool { return true }

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

	p, err := oscap.OpenWithProbe(oscap.ModePortable, opts, probeAllTrue, nativeMustNotBeUsed(t), New)
	if err != nil {
		t.Fatalf("portable 模式装配失败: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close 失败: %v", err)
		}
	}()

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

// TestAutoModeFallsBackToBuiltin 覆盖另一条真实路径：auto 模式下探测说
// 「什么都不支持」，六项应当全部落到 builtin 且照样可用。
//
// 这条与上面那条不重复：portable 是**不看探测**，auto 是**看了探测之后降级**，
// 两者在 provider.go 里走的是不同分支。
func TestAutoModeFallsBackToBuiltin(t *testing.T) {
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

// TestBuiltinIsComplete 直接盯住那条铁律：New 返回的 Set 六项一个都不能缺。
//
// 编译期断言只能保证类型实现了接口，保证不了**字段被填进 Set**——
// 少写一行 `Times: a` 编译照样过，然后 oscap.New 在运行期抛 IncompleteBuiltinError。
func TestBuiltinIsComplete(t *testing.T) {
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

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(oscap.Options{}); !errors.Is(err, oscap.ErrInvalidArg) {
		t.Fatalf("空 Root 应得 ErrInvalidArg，实得 %v", err)
	}
}
