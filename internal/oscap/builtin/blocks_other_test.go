//go:build !unix

package builtin

// blocks_other_test.go —— 非 unix 平台测不出实际占用块数。
//
// Windows 没有 st_blocks：没有「文件占了多少块」这个口径（稀疏性要看
// GetFileInformationByHandleEx 的 FILE_ALLOCATED_RANGE_BUFFER，那是另一套东西，
// 不值得为了一条测试断言去实现）。这里返回 ok=false，用例跳过即可 ——
// 跳过的只是「省没省空间」那一半，可观测语义那一半照测不误。

func fileBlocks(string) (int64, bool) { return 0, false }
