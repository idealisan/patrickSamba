package vfs

// path_perf_test.go —— 路径解析在大目录上的**耗时增长曲线**。
//
// 判据必须可证伪，所以这里不测「比以前快」（没有基线就没法证伪），
// 而是测**耗时随目录规模的增长率**：
//
//	同一个操作分别在 1k / 10k / 50k 条目的目录里跑，
//	O(1) 的实现耗时应当基本持平，O(n) 的实现应当近似线性增长。
//
// 增长率是无量纲的，换机器、换文件系统都不影响结论，
// 而「12 µs 还是 30 µs」这种绝对值换台机器就作废。
//
// 跑法：
//
//	go test -run 'TestPathLookupScaling' -v ./internal/vfs/
//
// -short 下跳过（要建 6.1 万个文件）。

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeFlatDir 建一个含 n 个空文件的目录，模拟 .sparsebundle/bands/。
// 名字用 band 的实际形态（16 进制、无扩展名）。
func makeFlatDir(tb testing.TB, n int) string {
	tb.Helper()
	dir := tb.TempDir()
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%x", i))
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			tb.Fatalf("建第 %d 个文件: %v", i, err)
		}
		_ = f.Close()
	}
	return dir
}

// timeOp 跑 iters 次取平均，返回单次耗时。
func timeOp(iters int, fn func(i int)) time.Duration {
	start := time.Now()
	for i := 0; i < iters; i++ {
		fn(i)
	}
	return time.Since(start) / time.Duration(iters)
}

// pathScaleSizes 是三档目录规模。50k 已经能把 O(n) 和 O(1) 拉开两个数量级。
var pathScaleSizes = []int{1000, 10000, 50000}

func TestPathLookupScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("要建 6.1 万个文件，-short 下跳过")
	}

	// 三种调用形态，分别对应真实的 SMB 操作：
	//   existing —— 末级名字**存在且大小写完全一致**：Remove / Rename / SetInfo
	//   missing  —— 末级名字**不存在**：Mkdir / FILE_CREATE 新 band
	//   resolve  —— Resolve 走 resolveComponents 的末级分支：Open/Create
	type row struct {
		existing time.Duration
		missing  time.Duration
		resolve  time.Duration
	}
	got := make(map[int]row, len(pathScaleSizes))

	for _, n := range pathScaleSizes {
		dir := makeFlatDir(t, n)
		r, err := NewResolver(dir, true)
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		// 取一个位于目录**末尾**的已存在名字：全扫描是顺序的，
		// 取靠前的名字会让 O(n) 实现看起来很快，那是自欺欺人。
		last := fmt.Sprintf("%x", n-1)

		const iters = 20
		got[n] = row{
			existing: timeOp(iters, func(int) {
				if _, name, err := r.ResolveParent(last); err != nil || name != last {
					t.Fatalf("ResolveParent(%q) = %q, %v", last, name, err)
				}
			}),
			missing: timeOp(iters, func(i int) {
				want := fmt.Sprintf("newband-%d", i)
				if _, name, err := r.ResolveParent(want); err != nil || name != want {
					t.Fatalf("ResolveParent(%q) = %q, %v", want, name, err)
				}
			}),
			resolve: timeOp(iters, func(i int) {
				want := fmt.Sprintf("newband-%d", i)
				if _, err := r.Resolve(want); err != nil {
					t.Fatalf("Resolve(%q): %v", want, err)
				}
			}),
		}
	}

	t.Logf("%-8s %12s %12s %12s", "条目数", "已存在名字", "新名字", "Resolve新名")
	for _, n := range pathScaleSizes {
		g := got[n]
		t.Logf("%-8d %12v %12v %12v", n, g.existing, g.missing, g.resolve)
	}

	// ---- 判据 ----
	//
	// 「已存在且大小写一致」是绝对多数的调用形态，它必须与目录规模无关。
	// 阈值取 4×：50 倍的规模差如果只带来 4 倍以内的波动，就不可能是线性的
	// （线性应当是 ~50×）。留 4 倍余量是给计时噪声和页缓存冷热的。
	base := got[pathScaleSizes[0]].existing
	worst := got[pathScaleSizes[len(pathScaleSizes)-1]].existing
	if base <= 0 {
		t.Fatalf("基准耗时为 0，计时不可信")
	}
	if ratio := float64(worst) / float64(base); ratio > 4 {
		t.Errorf("ResolveParent 命中已存在名字时耗时随目录规模增长 %.1f 倍"+
			"（1k=%v → 50k=%v），说明还在做全目录扫描", ratio, base, worst)
	}
}
