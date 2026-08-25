//go:build !linux && !darwin && !windows

package native

// native_other.go —— 非目标平台的兜底（AGENTS.md C7 只要求
// linux / darwin / windows 三家，其余平台保证能编译过即可）。
//
// 返回**空**的 Set：一项原生能力都不提供，于是全部落到 builtin。
// 这与 probe_other.go 的「一律 false」是同一件事的两面，两边必须一致 ——
// 探测说没有、这里也确实没有，不会出现「探测报 true 而实现是 nil」
// 那种启动通过、运行时才降级的静默失效。
//
// 注意这不是「窄档分支」：走的仍然是同一条装配路径（§1.2 铁律 1）。

import "github.com/finalappstore/stupidsamba/internal/oscap"

func newSet(oscap.Options) (oscap.Set, error) {
	return oscap.Set{
		// 本平台一项原生能力都没有，Migration 同样没有「宿主对象」可依托；
		// 给无操作只是保持与其它平台一致的形状，语义上等价于留 nil。
		Migration: noopMigration{},
	}, nil
}
