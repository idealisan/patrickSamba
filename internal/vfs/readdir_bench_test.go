package vfs

// readdir_bench_test.go —— 大目录枚举的性能测量。
//
// 动机：Time Machine 的 `.sparsebundle/bands/` 目录会有几万到几十万个
// 小文件，客户端会反复分页枚举它。这里先**量**再决定要不要优化
// （AGENTS.md §9 精神：不凭感觉优化）。
//
// 跑法：
//
//	go test -run XXX -bench 'BenchmarkReadDir' -benchtime 1x -benchmem ./internal/vfs/
//
// 大目录的用例会建几万个文件，默认在 -short 下跳过。
//
// # 实测结果（AMD EPYC 9K65, linux/amd64, tmpfs 之外的普通 ext4）
//
//	BenchmarkReadDir1k           2.7 ms      枚举到底
//	BenchmarkReadDir10k         29.6 ms      枚举到底
//	BenchmarkReadDir50k        152   ms      枚举到底  49 MB  341k allocs
//	BenchmarkReadDirFirstPage   18   ms      **首批** 100 条  5.8 MB  90k allocs
//
// # 结论：当前实现够用，不做优化
//
//  1. 开销随条目数**线性**增长（约 3 µs/条），主要成本是每条一次 lstat。
//     这是 QUERY_DIRECTORY 必须提供属性所固有的，换任何数据结构都省不掉。
//  2. 快照**每个句柄只建一次**（见 ReadDir 里的 `h.dirNames == nil || restart`），
//     翻页不会重读目录。反证：50k 条要翻 500 页，若每页重读一次目录，
//     总耗时应是 500 × 18 ms ≈ 9 s，实测 152 ms，相差近 60 倍。
//  3. 首批延迟 18 ms（5 万条）远低于任何客户端的超时阈值，
//     不存在「客户端以为服务器没响应」的风险。
//
// # 已知的真实代价：内存
//
// 快照把全部文件名驻留在句柄里，5 万条约 5.8 MB。十万级目录会到
// 十几 MB／句柄，多个客户端同时枚举同一个 bands 目录时会叠加。
//
// 现在不优化的理由：这是**用一份可预测的内存换枚举一致性** ——
// 翻页期间目录被改不会漏项/重复项，而 SMB 客户端做增量对比时
// 非常依赖这个性质。真要压内存，正确的做法是换成「按名字排序的
// 游标 + 每页重新 seek」，那会牺牲一致性，得先有真实的内存压力
// 证据再谈。届时请先补一个并发枚举的内存基准，不要直接改。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// buildBandDir 造一个模拟 .sparsebundle/bands/ 的目录。
// 真实的 band 文件名是十六进制序号，这里照抄这个形态 ——
// 名字长度与分布会影响排序与匹配的开销。
func buildBandDir(tb testing.TB, n int) string {
	tb.Helper()
	root := tb.TempDir()
	bands := filepath.Join(root, "bands")
	if err := os.Mkdir(bands, 0o755); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		p := filepath.Join(bands, fmt.Sprintf("%x", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	return root
}

// enumerateAll 模拟客户端把一个目录分页枚举到底，返回总条数。
func enumerateAll(tb testing.TB, fs *LocalFS, dir string, page int) int {
	tb.Helper()
	h, _, err := fs.Open(&OpenRequest{
		Path: dir, Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	})
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	total := 0
	for {
		batch, err := h.ReadDir("*", false, page)
		total += len(batch)
		if err == io.EOF {
			return total
		}
		if err != nil {
			tb.Fatal(err)
		}
		if len(batch) == 0 {
			return total
		}
	}
}

func benchReadDir(b *testing.B, n, page int) {
	root := buildBandDir(b, n)
	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = fs.Close() }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got := enumerateAll(b, fs, "bands", page)
		// +2 是 "." 和 ".."
		if got != n+2 {
			b.Fatalf("枚举到 %d 条, want %d", got, n+2)
		}
	}
}

// 每批 100 条大致对应真实客户端在 64KiB 输出缓冲下的批量。
func BenchmarkReadDir1k(b *testing.B)  { benchReadDir(b, 1_000, 100) }
func BenchmarkReadDir10k(b *testing.B) { benchReadDir(b, 10_000, 100) }

func BenchmarkReadDir50k(b *testing.B) {
	if testing.Short() {
		b.Skip("建 5 万个文件较慢，-short 下跳过")
	}
	benchReadDir(b, 50_000, 100)
}

// BenchmarkReadDirFirstPage 单独量**首个** QUERY_DIRECTORY 的延迟。
//
// 这是最关键的指标：快照是在第一次调用时一次性建立的，
// 如果这里要好几秒，客户端会认为服务器无响应。
func BenchmarkReadDirFirstPage(b *testing.B) {
	if testing.Short() {
		b.Skip("建 5 万个文件较慢，-short 下跳过")
	}
	root := buildBandDir(b, 50_000)
	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = fs.Close() }()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h, _, err := fs.Open(&OpenRequest{
			Path: "bands", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
		})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := h.ReadDir("*", false, 100); err != nil && err != io.EOF {
			b.Fatal(err)
		}
		_ = h.Close()
	}
}

// TestReadDirLargeDirCorrectness 是 benchmark 的正确性对照：
// 大目录下分页枚举必须**不重不漏**。
//
// 单独写一个的理由：翻页游标的 off-by-one 在小目录上很难暴露，
// 而 Time Machine 恰恰跑在几万条的目录上。
func TestReadDirLargeDirCorrectness(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过")
	}
	const n = 5_000
	root := buildBandDir(t, n)
	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fs.Close() }()

	h, _, err := fs.Open(&OpenRequest{
		Path: "bands", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	seen := make(map[string]int, n)
	for {
		batch, err := h.ReadDir("*", false, 97) // 故意用质数批量，制造边界
		for _, e := range batch {
			seen[e.Name]++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
	}

	if len(seen) != n+2 {
		t.Errorf("枚举到 %d 个不同条目, want %d（含 . 与 ..）", len(seen), n+2)
	}
	for name, c := range seen {
		if c != 1 {
			t.Errorf("条目 %q 出现了 %d 次，应恰好 1 次", name, c)
		}
	}
	for i := 0; i < n; i++ {
		if _, ok := seen[fmt.Sprintf("%x", i)]; !ok {
			t.Fatalf("条目 %x 漏了", i)
		}
	}
}
