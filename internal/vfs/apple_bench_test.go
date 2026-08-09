package vfs

// apple_bench_test.go —— AAPL readdir_attr 场景下的每条目开销测量。
//
// 动机：commit 9b7185b 测过「纯枚举」的成本并得出「无需优化」的结论，
// 但那时 AAPL readdir_attr 还没实现。协商成功后**每一条目录项**都要
// 额外拿到 FinderInfo 与资源派生大小，成本模型完全变了，必须重测
// （AGENTS.md §9：先量再决定）。
//
// 跑法：
//
//	go test -run XXX -bench 'BenchmarkReadDirAAPL|BenchmarkAppleInfo' \
//	    -benchtime 1x -benchmem ./internal/vfs/
//
// # 实测（AMD EPYC 9K65, linux/amd64, 10k 条 band 目录, 每页 100 条, 三次取中位数）
//
//	纯枚举（无 readdir_attr）        39 ms    基线
//	+ AppleInfo（按路径回查）       177 ms    4.5×
//	+ AppleInfoAt（目录句柄内）      52 ms    1.35×
//
// 单次调用（-benchtime 2000x）：按路径 6.6 µs / 18 allocs，
// 目录句柄内 2.8 µs / 8 allocs。
//
// # 结论
//
//  1. **上层必须用 DirAppleMetadata.AppleInfoAt，不要用 FileSystem.AppleInfo。**
//     后者每条目都要把 "bands/<name>" 从共享根重新解析一遍（逐级 lstat
//     + 软链校验），在十万级目录上就是十万次重复解析，成本比它要取的
//     元数据本身还高。
//  2. 快照期间顺手记下 ._ 旁路文件的存在性（localHandle.dotUnder）之后，
//     每个条目省掉一次注定 ENOENT 的 open。Time Machine 的 bands 目录
//     一个 ._ 都没有，这条优化对该场景是 100% 命中：
//     优化前 by-handle 是基线的 3.1×，优化后 1.35×。
//  3. 剩下的 ~35% 开销里，**syscall 部分确实是固有的**：getxattr
//     每条目一次、无法批量（POSIX 没有批量 xattr 接口）。
//     但它周围的**分配**可以消掉，见下。
//
// # 后续（BenchmarkAppleInfoBatch，2000 条 band，同一进程内对比）
//
//	逐条 AppleInfoAt      3.27 µs/条   581 B   4.6 allocs
//	AppleInfoAtBatch      1.74 µs/条   222 B   3.6 allocs   ← 快 47%，省 62% 内存
//
// 两处改动叠加得来：
//
//   - readMetaXattrFast（xattr_unix.go）：metadata blob 的长度是规范固定的
//     402 字节，不必像通用的 XattrAccessor.Get 那样「先问长度再分配」，
//     一趟读完。**这一条逐条调用也受益。**
//   - AppleInfoAtBatch：那 402 字节的读缓冲整批复用一个，
//     外加每条一次的锁获取变成整批一次。
//
// 到此为止不再继续优化：剩下的就是 getxattr 本身与路径字符串，
// 加缓存要处理失效，不划算。readdir_attr 的收益本来就是「省掉客户端对
// 每个文件额外开两次流」——一次 getxattr 换两次网络往返仍然大赚。

import (
	"path/filepath"
	"testing"
)

// benchAppleEnumerate 模拟 readdir_attr：枚举 + 逐条取 Apple 元数据。
func benchAppleEnumerate(b *testing.B, n int, viaHandle bool) {
	root := buildBandDir(b, n)
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
		for {
			batch, err := h.ReadDir("*", false, 100)
			for _, e := range batch {
				if e.Name == "." || e.Name == ".." {
					continue
				}
				if viaHandle {
					if _, _, err := h.(DirAppleMetadata).AppleInfoAt(e.Name); err != nil {
						b.Fatalf("AppleInfoAt(%s): %v", e.Name, err)
					}
				} else {
					p := filepath.ToSlash(filepath.Join("bands", e.Name))
					if _, _, err := fs.AppleInfo(p); err != nil {
						b.Fatalf("AppleInfo(%s): %v", p, err)
					}
				}
			}
			if err != nil || len(batch) == 0 {
				break
			}
		}
		_ = h.Close()
	}
}

// BenchmarkReadDirAAPLByPath 是「上层拿到 DirEntry 后按路径回查」的朴素做法。
func BenchmarkReadDirAAPLByPath1k(b *testing.B)  { benchAppleEnumerate(b, 1_000, false) }
func BenchmarkReadDirAAPLByPath10k(b *testing.B) { benchAppleEnumerate(b, 10_000, false) }

// BenchmarkReadDirAAPLByHandle 是目录句柄内直查（省掉整条路径重解析）。
func BenchmarkReadDirAAPLByHandle1k(b *testing.B)  { benchAppleEnumerate(b, 1_000, true) }
func BenchmarkReadDirAAPLByHandle10k(b *testing.B) { benchAppleEnumerate(b, 10_000, true) }

// BenchmarkAppleInfoSingle 拆出单次调用的成本，便于和 lstat 对比。
func BenchmarkAppleInfoSingle(b *testing.B) {
	root := buildBandDir(b, 16)
	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = fs.Close() }()

	b.Run("ByPath", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, _, err := fs.AppleInfo("bands/a"); err != nil {
				b.Fatal(err)
			}
		}
	})

	h, _, err := fs.Open(&OpenRequest{
		Path: "bands", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	b.Run("ByHandle", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, _, err := h.(DirAppleMetadata).AppleInfoAt("a"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
