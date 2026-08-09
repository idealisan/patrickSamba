package native

// provider_fallback_test.go —— 端到端证伪「auto 模式下 native 缺项真的落到 builtin」。
//
// # 为什么要这文件
//
// native 适配器的契约只有一句话：做不到的能力把 Set 对应字段留 nil。
// 但「留 nil」必须真的导致 provider 去选 builtin，否则上层会拿到 nil 句柄
// 在运行期炸。这正是本项目在 encryption_required 上栽过的同型坑
// （策略开关只测了「允许」路径，全绿，而「拒绝」路径压根没接线）。
//
// 本文件不依赖 builtin 适配器的真实完成度：用两个**桩工厂**喂给真实的
// oscap.New，模拟「native 把 Streams 留 nil（Windows 上 FindFirstStreamW
// 缺失时的真实输出）」，断言 provider 真的把命名流这项落到 builtin，且其它
// 项因为 native 真提供了而保持 native。
//
// 它放在 internal/oscap/native 包里（不碰 port 层 shared 文件），且不使用
// 任何平台专属符号，所以 linux/darwin/windows 三个 CI 行都会编译执行。

import (
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// discardCap 是一个**哑**能力实现，仅供本文件的 provider 接线测试当桩用。
// 它的方法永远返回 ErrNotSupported —— 这些桩从来不会被真的调用，它们只是
// 为了让 native/builtin 工厂能填满 Set 的字段，好让 provider 的逐项挑选
// 逻辑被真跑一遍。
type discardCap struct{}

func (discardCap) GetXattr(oscap.Ref, string) ([]byte, error) { return nil, oscap.ErrNotSupported }
func (discardCap) SetXattr(oscap.Ref, string, []byte) error   { return oscap.ErrNotSupported }
func (discardCap) RemoveXattr(oscap.Ref, string) error        { return oscap.ErrNotSupported }
func (discardCap) ListXattr(oscap.Ref) ([]string, error)      { return nil, oscap.ErrNotSupported }

func (discardCap) PunchHole(oscap.Ref, int64, int64) error { return oscap.ErrNotSupported }
func (discardCap) Preallocate(oscap.Ref, int64, int64) error {
	return oscap.ErrNotSupported
}
func (discardCap) AllocatedRanges(oscap.Ref, int64, int64) ([]oscap.Range, error) {
	return nil, oscap.ErrNotSupported
}
func (discardCap) SetSparse(oscap.Ref, bool) error { return oscap.ErrNotSupported }

func (discardCap) ListStreams(oscap.Ref) ([]oscap.StreamInfo, error) {
	return nil, oscap.ErrNotSupported
}
func (discardCap) OpenStream(oscap.Ref, string, oscap.StreamFlags) (oscap.StreamHandle, error) {
	return nil, oscap.ErrNotSupported
}
func (discardCap) RemoveStream(oscap.Ref, string) error { return oscap.ErrNotSupported }

func (discardCap) FileID(oscap.Ref) (uint64, error) { return 0, oscap.ErrNotSupported }

func (discardCap) CreationTime(oscap.Ref) (time.Time, error) {
	return time.Time{}, oscap.ErrNotSupported
}
func (discardCap) SetCreationTime(oscap.Ref, time.Time) error { return oscap.ErrNotSupported }

func (discardCap) DOSAttributes(oscap.Ref) (uint32, error)  { return 0, oscap.ErrNotSupported }
func (discardCap) SetDOSAttributes(oscap.Ref, uint32) error { return oscap.ErrNotSupported }

var (
	_ oscap.Xattr         = discardCap{}
	_ oscap.SparseFile    = discardCap{}
	_ oscap.NamedStream   = discardCap{}
	_ oscap.StableFileID  = discardCap{}
	_ oscap.CreationTime  = discardCap{}
	_ oscap.DOSAttributes = discardCap{}
)

// builtinStream 是 builtin 侧提供的命名流桩，带一个可识别的实例，
// 用来断言 provider 真的选了它而不是 native 的 nil。
type builtinStream struct{ discardCap }

// TestProviderAutoFallsBackWhenNativeStreamsNil 模拟 Windows 上
// FindFirstStreamW 缺失时 native 的真实输出（Streams 留 nil），验证 auto 模式
// 下 provider 真的把命名流这项落到 builtin。
func TestProviderAutoFallsBackWhenNativeStreamsNil(t *testing.T) {
	o := oscap.Options{Root: t.TempDir()}

	// native 桩：除 Streams 外都给出（discardCap），Streams 故意留 nil ——
	// 这正是 newSetProcs(o, false) 会产生的真实输出。
	nativeStub := func(o oscap.Options) (oscap.Set, error) {
		return oscap.Set{
			Xattr:   discardCap{},
			Sparse:  discardCap{},
			IDs:     discardCap{},
			Times:   discardCap{},
			DOS:     discardCap{},
			Streams: nil,
		}, nil
	}
	bs := &builtinStream{}
	builtinStub := func(o oscap.Options) (oscap.Set, error) {
		return oscap.Set{
			Xattr:   discardCap{},
			Sparse:  discardCap{},
			IDs:     discardCap{},
			Times:   discardCap{},
			DOS:     discardCap{},
			Streams: bs,
		}, nil
	}

	// 探针说全部能力都「native 可用」：于是矩阵把六项都判给 native，
	// 留给 provider 去发现 native 的 Streams 其实是 nil 并落到 builtin。
	probe := oscap.Prober(func(c oscap.Capability, _ oscap.Options) bool { return true })

	p, err := oscap.OpenWithProbe(oscap.ModeAuto, o, probe, nativeStub, builtinStub)
	if err != nil {
		t.Fatalf("组装 Provider 失败: %v", err)
	}
	if got := p.Streams(); got != oscap.NamedStream(bs) {
		t.Fatalf("auto 模式没有把缺失的 Streams 落到 builtin：得到 %v，期望 %v", got, bs)
	}
	if k := p.Matrix().Kind(oscap.CapNamedStream); k != oscap.KindBuiltin {
		t.Fatalf("Matrix 没有把 Streams 记为 builtin，得到 %v", k)
	}
	// 反向对照：其它项既然 native 真的提供了，就应当是 native ——
	// 少了这条，一个「无脑全返回 builtin」的退化实现也能让上面的断言通过。
	if k := p.Matrix().Kind(oscap.CapXattr); k != oscap.KindNative {
		t.Fatalf("Matrix 把本可由 native 提供的 Xattr 错记成 %v", k)
	}
}
