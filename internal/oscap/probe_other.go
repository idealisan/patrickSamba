//go:build !linux && !darwin && !windows

package oscap

// probe_other.go —— 非目标平台的兜底（AGENTS.md C7 只要求
// linux / darwin / windows 三家，其余平台保证能编译过即可）。
//
// 一律返回 false，于是**全部能力落到 builtin**。这正是 builtin 存在的意义：
// 一个我们没见过的系统（嵌入式、NAS 固件、未来的某个 GOOS）上，
// 服务照样能起来、照样正确 —— 只是慢一点。
// 注意这不是「窄档分支」：走的仍然是同一条装配路径，只是矩阵里每一项都指向
// builtin，和 filesystem_mode: portable 的结果完全一致（§1.2 铁律 1）。

func probeNative(Capability, Options) bool { return false }
