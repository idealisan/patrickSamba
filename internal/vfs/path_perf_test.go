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
// 上假红过：事故（旧 CNB 流水线，编号见归档记录），`ff77acb`（绿）与 `c269b76`（红）之间
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

// scanCounts 是一档目录规模下测到的全扫次数。
//
// 单独抽成类型，是为了让判据本身能被**纯函数**表达（scanViolations），
// 从而可以喂人造数据做反向对照 —— 见 TestScanViolationsCatchesRegression。
type scanCounts struct {
	existingScans        int64 // 必须 == 0
	resolveExistingScans int64 // 必须 == 0
	missingScans         int64 // 只诊断（这条路径的 O(n) 热点尚未修）
	variantScans         int64 // 必须 > 0（反向对照）
}

// scanViolations 是「零全扫」判据的**纯函数**形式：给定一档规模下的全扫次数，
// 返回它违反了哪几条；返回空切片表示通过。
//
// 为什么要把三个 if 抽成函数而不是就地写在用例里：判据本身也需要被验证。
// 内联版本里，如果有人把某个 if 整块删掉，**没有任何东西会发现** ——
// 用例照样全绿，而门禁已经空了。抽成纯函数之后，
// TestScanViolationsCatchesRegression 可以喂它人造的违规数据，
// 断言它**确实报红**。理由见 memory/feedback_falsifiable_assertions.md：
// 一个从来没红过的判据，和没有判据是一回事。
//
// 注意 missingScans 在**大小写敏感**宿主上**故意不判**：那条路径（末级名字
// 不存在）目前仍是 O(n)，是 path.go 里记录在案的未修热点，不是本用例要守的
// 命题。在折叠宿主上它是反向对照（见下），那时才参与判定。
func scanViolations(n int, iters int, c scanCounts, caseSensitive bool) []string {
	var out []string

	// 「已存在且大小写一致」是绝对多数的调用形态（客户端用的是枚举里拿到的
	// 名字），它必须一次全扫都不做 —— 这正是 09c8927 承诺的事，也是
	// Time Machine 往 .sparsebundle/bands/ 灌十万个 band 时不退化成 O(n²)
	// 的前提。判据是**次数**，与机器快慢、runner 负载完全无关。
	if c.existingScans != 0 {
		out = append(out, fmt.Sprintf(
			"目录 %d 条目：ResolveParent 命中已存在且大小写一致的名字，"+
				"却做了 %d 次全目录扫描（共 %d 次调用），说明「精确匹配优先」失效了",
			n, c.existingScans, iters))
	}
	if c.resolveExistingScans != 0 {
		out = append(out, fmt.Sprintf(
			"目录 %d 条目：Resolve 命中已存在且大小写一致的名字，"+
				"却做了 %d 次全目录扫描（共 1 次调用），resolveComponents 的精确匹配失效了",
			n, c.resolveExistingScans))
	}
	// 反向对照：这一条挂了说明计数器根本没接在全扫那条路上 ——
	// 一个永远读到 0 的计数器会让上面两条断言变成永远通过的摆设。
	//
	// 用哪条路径当对照，取决于宿主折不折叠大小写：
	if caseSensitive {
		// 大小写敏感宿主：「只有大小写不同」的名字精确 stat 必 miss，
		// 一定走全扫（variantScans）。一次都不做 ⇒ 计数器没接上。
		if c.variantScans <= 0 {
			out = append(out, fmt.Sprintf(
				"目录 %d 条目：查大小写变体的名字竟然一次全扫都没做，"+
					"说明计数器没接在全扫路径上，上面两条零全扫断言不可信。"+
					"（若已改用折叠索引替代全目录扫描，请把计数点挪到新实现里）", n))
		}
	} else {
		// 折叠宿主（APFS/NTFS）：大小写变体会被内核精确命中，压根不走全扫，
		// 不能当反向对照。改用「目录里完全不存在的名字」——它必然走全扫
		// （missing 那条路径目前仍是 O(n)），同样能证明计数器接在全扫路径上。
		if c.missingScans <= 0 {
			out = append(out, fmt.Sprintf(
				"目录 %d 条目：查不存在的名字竟然一次全扫都没做，"+
					"说明计数器没接在全扫路径上，上面两条零全扫断言不可信", n))
		}
	}
	return out
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

	// 宿主折不折叠大小写，决定「只有大小写不同」的名字能不能当反向对照：
	// 折叠宿主（APFS/NTFS）上它会精确命中、压根不走全扫。
	// 两条分支的判据见 scanViolations。
	caseSensitive := !hostFoldsCase(t)

	// 四种调用形态，分别对应真实的 SMB 操作：
	//   existing —— 末级名字**存在且大小写完全一致**：Remove / Rename / SetInfo
	//   missing  —— 末级名字**不存在**：Mkdir / FILE_CREATE 新 band
	//   resolve  —— Resolve 走 resolveComponents 的末级分支：Open/Create
	//   variant  —— 末级名字存在但**大小写不同**：反向对照，必须走全扫
	type row struct {
		existing time.Duration
		missing  time.Duration
		resolve  time.Duration

		// 全扫次数。判据在 scanViolations 里，这里只负责测出来。
		scanCounts
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
		// 反向对照的前提只在大小写敏感的宿主上成立：折叠宿主上这个变体会
		// 被内核当成同一个名字精确命中，「落空 → 全扫」这条路走不到。
		if caseSensitive {
			if _, err := os.Lstat(filepath.Join(dir, variant)); !os.IsNotExist(err) {
				t.Fatalf("反向对照失效：%q 在磁盘上真实存在（err=%v），"+
					"精确 Lstat 会直接命中，压根不会走全扫", variant, err)
			}
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
		//
		// 折叠宿主上这条对照不成立（变体会被精确命中），照原样跑会得到
		// 「0 次全扫」并把判据判红。此时反向对照改由 missingScans 承担，
		// 所以这里整块跳过、variantScans 保持 0，由 scanViolations 走另一条分支。
		if caseSensitive {
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
		}

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
	// 判定逻辑在 scanViolations 里（纯函数，便于反向对照）。
	for _, n := range pathScaleSizes {
		for _, msg := range scanViolations(n, pathScaleIters, got[n].scanCounts, caseSensitive) {
			t.Error(msg)
		}
	}
}

// TestScanViolationsCatchesRegression 是**判据自身的反向对照**。
//
// 上面那个用例断言「全扫次数为 0」。但只要判定逻辑被误删或写反，它就会变成
// 一个永远通过的空判据 —— 而且从输出上完全看不出来（还是全绿，还是打那张表）。
// 本项目已经吃过同型的亏：加密曾用「能读到内容」判定，漏掉了明文旁路。
//
// 所以这里给 scanViolations 喂**人造的违规数据**，断言它确实报红。
// 这些用例不碰磁盘、不建文件、毫秒级跑完，也不受 -short 影响。
func TestScanViolationsCatchesRegression(t *testing.T) {
	const n, iters = 1000, 20

	// 先钉住「合规输入必须静默」。少了这一条，一个无脑 return 一堆错误的
	// 实现也能让下面所有用例通过 —— 那同样是个假判据。
	if v := scanViolations(n, iters, scanCounts{
		existingScans: 0, resolveExistingScans: 0, variantScans: 1,
	}, true); len(v) != 0 {
		t.Errorf("合规输入不该报错，却报了 %d 条：%v", len(v), v)
	}
	// 折叠宿主那一侧的合规输入同理：反向对照由 missingScans 承担，
	// 此时 variantScans 恒为 0 不该被当成违规。
	if v := scanViolations(n, iters, scanCounts{
		existingScans: 0, resolveExistingScans: 0, missingScans: 1,
	}, false); len(v) != 0 {
		t.Errorf("折叠宿主的合规输入不该报错，却报了 %d 条：%v", len(v), v)
	}

	// missingScans 在**大小写敏感**宿主上是纯诊断字段：无论多大都不该影响红绿。
	// 单独钉一条，防止将来有人顺手把那条未修的 O(n) 热点也加进判据 ——
	// 那会让用例在一个已知且**故意**未修的问题上长期红着，最后被整体禁用。
	// （折叠宿主上它是反向对照，为 0 反而要报红，见下面表格最后一条。）
	if v := scanViolations(n, iters, scanCounts{
		missingScans: 12345, variantScans: 1,
	}, true); len(v) != 0 {
		t.Errorf("missingScans 只做诊断，不该参与判定，却报了：%v", v)
	}

	for _, tc := range []struct {
		name          string
		in            scanCounts
		caseSensitive bool
		want          string // 期望出现在报错文本里的关键词
	}{
		{
			// 这就是要防的那个真回归：20 次调用全部走了全扫，
			// 也就是「精确匹配优先」被改没了，退回 O(目录条目数)。
			name:          "ResolveParent 每次调用都全扫（精确匹配优先失效）",
			in:            scanCounts{existingScans: iters, variantScans: 1},
			caseSensitive: true,
			want:          "精确匹配优先",
		},
		{
			name:          "只泄漏一次也要抓到（不是「大部分没全扫就算过」）",
			in:            scanCounts{existingScans: 1, variantScans: 1},
			caseSensitive: true,
			want:          "精确匹配优先",
		},
		{
			name:          "Resolve 的 resolveComponents 分支退回全扫",
			in:            scanCounts{resolveExistingScans: 1, variantScans: 1},
			caseSensitive: true,
			want:          "resolveComponents",
		},
		{
			// 计数器没接线 / 大小写回退被砍：此时上面两条零全扫断言
			// 会因为恒读到 0 而永远通过，必须由这一条兜住。
			name:          "计数器没接在全扫路径上（variantScans 恒为 0）",
			in:            scanCounts{variantScans: 0},
			caseSensitive: true,
			want:          "计数器没接在全扫路径上",
		},
		{
			// 折叠宿主那一侧的反向对照：缺了它，折叠分支的门禁就是空判据。
			name:          "折叠宿主：计数器没接在全扫路径上（missingScans 恒为 0）",
			in:            scanCounts{missingScans: 0},
			caseSensitive: false,
			want:          "计数器没接在全扫路径上",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := scanViolations(n, iters, tc.in, tc.caseSensitive)
			if len(v) == 0 {
				t.Fatalf("判据漏报：输入 %+v 明显违规，scanViolations 却返回空。"+
					"说明判定逻辑已失效，TestPathLookupScaling 的全绿不可信", tc.in)
			}
			if !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Errorf("报错文本里没有 %q，实际为：\n%s", tc.want, strings.Join(v, "\n"))
			}
		})
	}
}
