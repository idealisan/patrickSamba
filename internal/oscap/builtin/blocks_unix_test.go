//go:build unix

package builtin

// blocks_unix_test.go —— 测试用的「实际占用块数」读取。
//
// 为什么要有它：本次修复的两条收益（不填实已有的洞、尾部真回收）都是**省空间**，
// 可观测语义层面完全看不出来 —— 读回来都是零、AllocatedRanges 也都不报。
// 唯一能证明「没变胖 / 真的瘦了」的判据就是 st_blocks，所以测试必须能读到它。
//
// 分开两个文件是因为 Windows 没有 st_blocks 这个概念（那一侧返回 ok=false，
// 用例跳过而不是拿假数字做断言）。

import "syscall"

// fileBlocks 返回文件实际占用的 512 字节块数（st_blocks）。
//
// ok=false 表示本平台测不出来，调用方**必须跳过**相关断言。
func fileBlocks(path string) (int64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return st.Blocks, true
}
