package builtin

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

func TestPortablePunchHoleObservableSemantics(t *testing.T) {
	e := newEnv(t)
	const blocks = 4
	ref := e.file("band", bytesRepeat(0xAA, blocks*zeroBlockSize))

	// 打洞前：整个窗口都是已分配。
	got, err := e.set.Sparse.AllocatedRanges(ref, 0, blocks*zeroBlockSize)
	if err != nil {
		t.Fatalf("AllocatedRanges 失败: %v", err)
	}
	want := []oscap.Range{{Offset: 0, Length: blocks * zeroBlockSize}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("打洞前应整段已分配: 得 %v 期望 %v", got, want)
	}

	// 挖掉第 2 块。
	if err := e.set.Sparse.PunchHole(ref, zeroBlockSize, zeroBlockSize); err != nil {
		t.Fatalf("PunchHole 失败: %v", err)
	}

	// 语义一：读回来是零，且文件逻辑长度不变。
	data := e.read(ref)
	if len(data) != blocks*zeroBlockSize {
		t.Fatalf("打洞改变了文件长度: %d", len(data))
	}
	if !bytes.Equal(data[zeroBlockSize:2*zeroBlockSize], make([]byte, zeroBlockSize)) {
		t.Fatal("打洞区间读回来不是零")
	}
	if !bytes.Equal(data[:zeroBlockSize], bytesRepeat(0xAA, zeroBlockSize)) {
		t.Fatal("打洞波及了洞外的数据")
	}

	// 语义二：AllocatedRanges 不再报这一段。
	got, err = e.set.Sparse.AllocatedRanges(ref, 0, blocks*zeroBlockSize)
	if err != nil {
		t.Fatalf("AllocatedRanges 失败: %v", err)
	}
	want = []oscap.Range{
		{Offset: 0, Length: zeroBlockSize},
		{Offset: 2 * zeroBlockSize, Length: 2 * zeroBlockSize},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("打洞后区间不对: 得 %v 期望 %v", got, want)
	}
}

// TestPortableAllocatedRangesSelfCorrectsAfterOverwrite 是本文件里最要紧的一条。
//
// 打洞记录存在旁路库里，而客户端把数据写回同一段是走**普通 IO** 的，
// oscap 根本看不见。只信记账的实现会在这里少报一段真实数据 ——
// ports.go 警告过这个方向：「少报会让客户端以为数据丢了」。
// 这条用例就是那个可证伪的探针：如果哪天有人把回读校验优化掉，它会立刻红。
func TestPortableAllocatedRangesSelfCorrectsAfterOverwrite(t *testing.T) {
	e := newEnv(t)
	const blocks = 3
	ref := e.file("band", bytesRepeat(0xAA, blocks*zeroBlockSize))

	if err := e.set.Sparse.PunchHole(ref, zeroBlockSize, zeroBlockSize); err != nil {
		t.Fatalf("PunchHole 失败: %v", err)
	}

	// 绕过 oscap，像真实客户端那样直接写回数据。
	f, err := os.OpenFile(ref.Path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if _, err := f.WriteAt(bytesRepeat(0xBB, zeroBlockSize), zeroBlockSize); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	got, err := e.set.Sparse.AllocatedRanges(ref, 0, blocks*zeroBlockSize)
	if err != nil {
		t.Fatalf("AllocatedRanges 失败: %v", err)
	}
	want := []oscap.Range{{Offset: 0, Length: blocks * zeroBlockSize}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("被覆写的空洞必须回到已分配（少报真实数据是数据丢失级别的 bug）: 得 %v 期望 %v", got, want)
	}
}

func TestPortablePunchHoleClipsToEOF(t *testing.T) {
	e := newEnv(t)
	ref := e.file("small", bytesRepeat(0xCC, 100))

	if err := e.set.Sparse.PunchHole(ref, 50, 1<<20); err != nil {
		t.Fatalf("PunchHole 失败: %v", err)
	}
	if n := len(e.read(ref)); n != 100 {
		t.Fatalf("越过 EOF 的打洞不得改变文件长度，实得 %d", n)
	}

	// 查询窗口越过 EOF 时结果必须裁剪到 EOF。
	got, err := e.set.Sparse.AllocatedRanges(ref, 0, 1<<20)
	if err != nil {
		t.Fatalf("AllocatedRanges 失败: %v", err)
	}
	want := []oscap.Range{{Offset: 0, Length: 50}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("窗口未裁剪到 EOF: 得 %v 期望 %v", got, want)
	}

	// 完全落在 EOF 之外的窗口返回空结果。
	if got, err := e.set.Sparse.AllocatedRanges(ref, 1000, 100); err != nil || got != nil {
		t.Fatalf("EOF 之外的窗口应得 (nil, nil)，实得 (%v, %v)", got, err)
	}
}

func TestPortableSparseArgumentValidation(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.bin", bytesRepeat(1, 10))

	cases := []struct {
		name       string
		off, n     int64
		wantInvArg bool
	}{
		{"负 offset", -1, 10, true},
		{"负 length", 0, -1, true},
		{"大但不溢出", 1 << 61, 1 << 61, false}, // 合法，只是整段落在 EOF 之外
		{"相加溢出 int64", math.MaxInt64 - 5, 10, true},
		{"零长度", 0, 0, false},
	}
	for _, c := range cases {
		err := e.set.Sparse.PunchHole(ref, c.off, c.n)
		if got := errors.Is(err, oscap.ErrInvalidArg); got != c.wantInvArg {
			t.Errorf("PunchHole(%s): ErrInvalidArg=%v 期望 %v (err=%v)", c.name, got, c.wantInvArg, err)
		}
		_, err = e.set.Sparse.AllocatedRanges(ref, c.off, c.n)
		if got := errors.Is(err, oscap.ErrInvalidArg); got != c.wantInvArg {
			t.Errorf("AllocatedRanges(%s): ErrInvalidArg=%v 期望 %v (err=%v)", c.name, got, c.wantInvArg, err)
		}
		err = e.set.Sparse.Preallocate(ref, c.off, c.n)
		if got := errors.Is(err, oscap.ErrInvalidArg); got != c.wantInvArg {
			t.Errorf("Preallocate(%s): ErrInvalidArg=%v 期望 %v (err=%v)", c.name, got, c.wantInvArg, err)
		}
	}
}

// TestPortableSetSparseAsymmetry 钉住 ports.go 那条刻意的不对称。
func TestPortableSetSparseAsymmetry(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.bin", nil)
	if err := e.set.Sparse.SetSparse(ref, true); err != nil {
		t.Fatalf("SetSparse(true) 应成功，实得 %v", err)
	}
	if err := e.set.Sparse.SetSparse(ref, false); !errors.Is(err, oscap.ErrNotSupported) {
		t.Fatalf("SetSparse(false) 应得 ErrNotSupported（不能假装做到），实得 %v", err)
	}
}

func TestPortableSparseMissingObject(t *testing.T) {
	e := newEnv(t)
	ref := oscap.Ref{Path: filepath.Join(e.root, "nope")}
	if err := e.set.Sparse.PunchHole(ref, 0, 10); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("对不存在的对象打洞应得 ErrNotFound，实得 %v", err)
	}
	if err := e.set.Sparse.Preallocate(ref, 0, 10); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("对不存在的对象预分配应得 ErrNotFound，实得 %v", err)
	}
	if _, err := e.set.Sparse.AllocatedRanges(ref, 0, 10); !errors.Is(err, oscap.ErrNotFound) {
		t.Fatalf("对不存在的对象查区间应得 ErrNotFound，实得 %v", err)
	}
}

func TestPortablePreallocateKeepsLogicalSize(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.bin", bytesRepeat(7, 16))
	if err := e.set.Sparse.Preallocate(ref, 0, 1<<20); err != nil {
		t.Fatalf("Preallocate 失败: %v", err)
	}
	if n := len(e.read(ref)); n != 16 {
		t.Fatalf("Preallocate 不得改变逻辑长度，实得 %d", n)
	}
}

func TestPortableHolesSurviveReopen(t *testing.T) {
	e := newEnv(t)
	ref := e.file("band", bytesRepeat(0xAA, 2*zeroBlockSize))
	if err := e.set.Sparse.PunchHole(ref, 0, zeroBlockSize); err != nil {
		t.Fatalf("PunchHole 失败: %v", err)
	}
	e.reopen(false)
	got, err := e.set.Sparse.AllocatedRanges(ref, 0, 2*zeroBlockSize)
	if err != nil {
		t.Fatalf("AllocatedRanges 失败: %v", err)
	}
	want := []oscap.Range{{Offset: zeroBlockSize, Length: zeroBlockSize}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("重开后空洞记录丢了: 得 %v 期望 %v", got, want)
	}
}

// TestPortableRangeMath 是区间集合运算的纯函数用例（无 IO，跑得飞快）。
func TestPortableRangeMath(t *testing.T) {
	t.Run("normalize", func(t *testing.T) {
		in := []oscap.Range{
			{Offset: 30, Length: 10},
			{Offset: 0, Length: 10},
			{Offset: 5, Length: 10},  // 与前一条重叠
			{Offset: 40, Length: 5},  // 与 30..40 相邻，应合并
			{Offset: 100, Length: 0}, // 空区间应被丢弃
		}
		want := []oscap.Range{{Offset: 0, Length: 15}, {Offset: 30, Length: 15}}
		if got := normalizeRanges(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("得 %v 期望 %v", got, want)
		}
		if got := normalizeRanges(nil); got != nil {
			t.Fatalf("空输入应得 nil，实得 %v", got)
		}
	})

	t.Run("clip", func(t *testing.T) {
		in := []oscap.Range{{Offset: 0, Length: 100}, {Offset: 200, Length: 100}}
		w := oscap.Range{Offset: 50, Length: 200} // [50, 250)
		want := []oscap.Range{{Offset: 50, Length: 50}, {Offset: 200, Length: 50}}
		if got := clipRanges(in, w); !reflect.DeepEqual(got, want) {
			t.Fatalf("得 %v 期望 %v", got, want)
		}
	})

	t.Run("subtract", func(t *testing.T) {
		w := oscap.Range{Offset: 0, Length: 100}
		holes := []oscap.Range{{Offset: 0, Length: 10}, {Offset: 50, Length: 10}}
		want := []oscap.Range{{Offset: 10, Length: 40}, {Offset: 60, Length: 40}}
		if got := subtractRanges(w, holes); !reflect.DeepEqual(got, want) {
			t.Fatalf("得 %v 期望 %v", got, want)
		}
		// 整个窗口都是洞。
		if got := subtractRanges(w, []oscap.Range{{Offset: 0, Length: 100}}); got != nil {
			t.Fatalf("全洞时应得 nil，实得 %v", got)
		}
	})
}

// TestPortableAppendZeroBlocksAlignsToFileOffset 盯的是分块对齐：块边界必须按**文件绝对
// 偏移**算，否则相邻两次读会错位，同一物理块被劈成两半分别判定。
func TestPortableAppendZeroBlocksAlignsToFileOffset(t *testing.T) {
	data := make([]byte, zeroBlockSize)
	data[len(data)-1] = 1 // 尾字节非零，绝对偏移落在第二个块里

	// 从块中间（绝对偏移 2048）开始：切出 [2048,4096) 与 [4096,6144) 两段，
	// 前者全零算空洞，后者含那个非零字节，整段判为已分配。
	// 若边界按 buf 起点算，切法会变成 [2048,6144) 一整块，结果就没有空洞了。
	got := normalizeRanges(appendZeroBlocks(nil, data, zeroBlockSize/2))
	want := []oscap.Range{{Offset: zeroBlockSize / 2, Length: zeroBlockSize / 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("得 %v 期望 %v", got, want)
	}
}
