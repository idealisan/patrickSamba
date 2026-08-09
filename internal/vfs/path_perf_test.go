package vfs

// path_perf_test.go —— 「精确匹配优先」这个优化到底有没有生效。
//
// ## 判据是全目录扫描的**次数**，不是耗时
//
// 优化承诺的原话是：末级名字**在磁盘上原样存在**时，不再无条件全扫父目录
// （path.go 的 ResolveParent，提交 09c8927）。所以要断言的命题就一句话：
//
//	命中已存在且大小写完全一致的名字 ⇒ 全目录扫描次数 == 0，与目录规模无关。
//
// 这里用包内计数器 pathFullScans 直接数，**不再从耗时去反推**。
//
// ### 为什么不能用耗时反推（血泪，别改回去）
//
// 本用例的上一版判据是「50k 的耗时 / 1k 的耗时 > 4× 就判红」。它在共享 runner
// 上假红过：事故 sn=`cnb-v7o-1jviua2rj`，`ff77acb`（绿）与 `c269b76`（红）之间
// **只差一个 THIRD_PARTY.md 文档改动，产品代码逐字节相同**，同一份代码一红一绿；
// 同一个用例的 push 构建红、**同 sha 的 pull_request 构建绿**；两条流水线相隔
// 9 秒在同一台 runner（10.235.0.14）上重叠跑，互相抢 CPU。
//
// 根因是量级：基准量只有 ~1.5 µs，而 iters 只有 20 次取**平均**。这个尺度上
// 一次 GC、一次调度抢占、一次缺页就能把平均值抬高十几倍。当时那张表本身就在
// 反证「不是回归」——1k=1.557µs → 10k=1.703µs 只涨 1.09 倍，真在全扫的话
// 10 倍规模应当是 ~10 倍耗时；50k 那个 21.7µs 是离群点，不是曲线。
//
// **墙钟比值在共享 runner 上不是可信判据。** 修法不是把阈值从 4× 放大到 20×
// （那是把门禁缴械：跑红了就调大数字，正是门禁要防的事本身），
// 也不是加 build tag / t.Skip 藏起来（本项目明令禁止的止血）。
// 修法是换一个**确定性**的观测点，也就是计数器。
//
// 耗时那张表**保留**为诊断输出（t.Logf），因为它一眼能看出 missing 那条路径
// 仍然是 O(n)（124µs → 1.12ms → 5.67ms，path.go 里记的未修复热点），
// 但它**不参与红绿判定**。
//
// ### 反向对照是硬要求
//
// 一个永远读到 0 的计数器和没有计数器是一回事。所以除了「已存在名字 ⇒ 0 次」，
// 必须同时断言「只有大小写不同的名字 ⇒ > 0 次」，证明计数器真的接在那条路上、
// 也证明大小写回退本身没被砍掉。
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
	"strings"
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
//
// ⚠️ 返回值只用于 t.Logf 诊断，**不要拿它做判据**（理由见文件头）。
func timeOp(iters int, fn func(i int)) time.Duration {
	start := time.Now()
	for i := 0; i < iters; i++ {
		fn(i)
	}
	return time.Since(start) / time.Duration(iters)
}

// fullScansDuring 返回 fn 执行期间发生的全目录扫描次数。
//
// 取差值而不是读绝对值，这样 `-count=N` 连跑、以及同包里其他用例先前造成的
// 扫描都不会污染结论。
//
// 计数器是进程全局的，所以本文件的用例**不得** t.Parallel（本包目前一个
// t.Parallel 都没有）。若将来引入并行用例，这里要改成按 Resolver 实例计数。
func fullScansDuring(fn func()) int64 {
	before := pathFullScans.Load()
	fn()
	return pathFullScans.Load() - before
}

// pathScaleSizes 是三档目录规模。50k 已经足够让「一次全扫」在诊断耗时里
// 显形（毫秒级），也足够让「零全扫」这个判据有说服力。
var pathScaleSizes = []int{1000, 10000, 50000}

// pathScaleIters 是每种形态重复调用的次数。它只影响诊断耗时的平滑程度，
// 以及「零全扫」判据覆盖的调用次数，不影响判据本身的确定性。
const pathScaleIters = 20

func TestPathLookupScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("要建 6.1 万个文件，-short 下跳过")
	}

	// 四种调用形态，分别对应真实的 SMB 操作：
	//   existing —— 末级名字**存在且大小写完全一致**：Remove / Rename / SetInfo
	//   missing  —— 末级名字**不存在**：Mkdir / FILE_CREATE 新 band
	//   resolve  —— Resolve 走 resolveComponents 的末级分支：Open/Create
	//   variant  —— 末级名字存在但**大小写不同**：反向对照，必须走全扫
	type row struct {
		existing time.Duration
		missing  time.Duration
		resolve  time.Duration

		// 全扫次数。前两个是主判据，后两个是诊断 + 反向对照。
		existingScans        int64 // 必须 == 0
		resolveExistingScans int64 // 必须 == 0
		missingScans         int64 // 只诊断（这条路径的 O(n) 热点尚未修）
		variantScans         int64 // 必须 > 0（反向对照）
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

		// 反向对照用的大小写变体。%x 是 16 进制，当前三档规模的 n-1 落在
		// 3e7 / 270f / c34f 上，都含字母，ToUpper 必然与原名不同。
		// 这里显式断言一次：万一将来改了 pathScaleSizes 取到纯数字名字
		// （例如 n=17 → "10"），反向对照会**静默退化成正向用例**，
		// 那比没有对照更糟 —— 判据看着还在，实际一次都没被触发。
		variant := strings.ToUpper(last)
		if variant == last {
			t.Fatalf("规模 %d 的末尾名字 %q 不含字母，大小写反向对照会失效；"+
				"请调整 pathScaleSizes 让 %%x 的结果带字母", n, last)
		}
		if _, err := os.Lstat(filepath.Join(dir, variant)); !os.IsNotExist(err) {
			t.Fatalf("反向对照失效：%q 在磁盘上真实存在（err=%v），"+
				"精确 Lstat 会直接命中，压根不会走全扫", variant, err)
		}

		var g row

		g.existingScans = fullScansDuring(func() {
			g.existing = timeOp(pathScaleIters, func(int) {
				if _, name, err := r.ResolveParent(last); err != nil || name != last {
					t.Fatalf("ResolveParent(%q) = %q, %v", last, name, err)
				}
			})
		})
		g.missingScans = fullScansDuring(func() {
			g.missing = timeOp(pathScaleIters, func(i int) {
				want := fmt.Sprintf("newband-%d", i)
				if _, name, err := r.ResolveParent(want); err != nil || name != want {
					t.Fatalf("ResolveParent(%q) = %q, %v", want, name, err)
				}
			})
		})
		g.resolve = timeOp(pathScaleIters, func(i int) {
			want := fmt.Sprintf("newband-%d", i)
			if _, err := r.Resolve(want); err != nil {
				t.Fatalf("Resolve(%q): %v", want, err)
			}
		})
		// Resolve 走的是 resolveComponents（与 ResolveParent 不同的调用点，
		// 各自有一处全扫回退），命中已存在且大小写一致的名字时同样必须零全扫。
		g.resolveExistingScans = fullScansDuring(func() {
			host, err := r.Resolve(last)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", last, err)
			}
			if filepath.Base(host) != last {
				t.Fatalf("Resolve(%q) = %q，末级名字被改写了", last, host)
			}
		})
		// 反向对照：只有大小写不同 ⇒ 精确 Lstat 必 miss ⇒ 必须回退到全扫，
		// 且必须真的把名字折叠回磁盘上的那一个。
		g.variantScans = fullScansDuring(func() {
			_, name, err := r.ResolveParent(variant)
			if err != nil {
				t.Fatalf("ResolveParent(%q): %v", variant, err)
			}
			if name != last {
				t.Errorf("ResolveParent(%q) = %q，期望折叠回 %q —— 大小写回退没生效",
					variant, name, last)
			}
		})

		got[n] = g
	}

	// ---- 诊断输出（不是判据）----
	//
	// 耗时列留着是因为它能一眼看出 missing 那条路径仍是 O(n)（path.go 里
	// 记的未修复热点）。**任何时候都不要把这几个数字变回红绿判据**，理由见文件头。
	t.Logf("%-8s %12s %12s %12s | 全扫次数 %s",
		"条目数", "已存在名字", "新名字", "Resolve新名",
		"(已存在/Resolve已存在/新名字/大小写变体)")
	for _, n := range pathScaleSizes {
		g := got[n]
		t.Logf("%-8d %12v %12v %12v | %13d / %d / %d / %d",
			n, g.existing, g.missing, g.resolve,
			g.existingScans, g.resolveExistingScans, g.missingScans, g.variantScans)
	}

	// ---- 判据 ----
	//
	// 「已存在且大小写一致」是绝对多数的调用形态（客户端用的是枚举里拿到的
	// 名字），它必须一次全扫都不做 —— 这正是 09c8927 承诺的事，也是
	// Time Machine 往 .sparsebundle/bands/ 灌十万个 band 时不退化成 O(n²)
	// 的前提。判据是**次数**，与机器快慢、runner 负载完全无关。
	for _, n := range pathScaleSizes {
		g := got[n]
		if g.existingScans != 0 {
			t.Errorf("目录 %d 条目：ResolveParent 命中已存在且大小写一致的名字，"+
				"却做了 %d 次全目录扫描（共 %d 次调用），说明「精确匹配优先」失效了",
				n, g.existingScans, pathScaleIters)
		}
		if g.resolveExistingScans != 0 {
			t.Errorf("目录 %d 条目：Resolve 命中已存在且大小写一致的名字，"+
				"却做了 %d 次全目录扫描（共 1 次调用），resolveComponents 的精确匹配失效了",
				n, g.resolveExistingScans)
		}
		// 反向对照：这一条挂了说明计数器根本没接在全扫那条路上
		// （或者大小写回退被砍了）—— 一个永远读到 0 的计数器
		// 会让上面两条断言变成永远通过的摆设。
		if g.variantScans <= 0 {
			t.Errorf("目录 %d 条目：查大小写变体的名字竟然一次全扫都没做，"+
				"说明计数器没接在全扫路径上，上面两条零全扫断言不可信。"+
				"（若已改用折叠索引替代全目录扫描，请把计数点挪到新实现里）", n)
		}
	}
}
