//go:build windows

package native

// stream_windows_test.go —— Windows 命名流降级路径的**真证伪**用例。
//
// # 为什么要这文件
//
// 命名流依赖 kernel32 的 FindFirstStreamW/FindNextStreamW/GetFileSizeEx，
// 而 LazyProc.Call 找不到导出会 panic。所以 newSetProcs 在探测缺失时
// **整项**把 Streams 判为不支持（留 nil），让 builtin 接管，而不是返回一个
// 「有时能有时 panic」的半吊子。这正是 capability.go 那条「半吊子必须整项
// 判为不支持」—— 上层才能做确定性决策。
//
// 只测「探测成功时能用」这一条路，是本项目在 encryption_required 上栽过的
// 同型坑（策略开关只测了允许路径，全绿，而拒绝路径压根没接线）。本文件用
// newSetProcs 的可注入参数**真的构造出「FindFirstStreamW 缺失」**，断言
// Streams 确实为 nil、且其它五项能力不被这次降级连坐。
//
// 本文件带 `//go:build windows`，由 test/ci/check-test-compile.sh 的
// windows/amd64 那一档负责编译（windows 在脚本的 KNOWN 平台清单里，
// 不需另行登记进 TAGS=）。

import (
	"testing"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// TestWindowsStreamProcsMissingYieldsNilStreams 验证：三项 kernel32 导出缺失时，
// newSetProcs 必须把 Streams 整项判为不支持（nil），而不是返回一个半吊子。
func TestWindowsStreamProcsMissingYieldsNilStreams(t *testing.T) {
	o := oscap.Options{Root: `C:\tmp`} // 仅用于构造 Set，不会被打开
	set, err := newSetProcs(o, false)
	if err != nil {
		t.Fatalf("newSetProcs(o, false) 不应返回错误: %v", err)
	}
	if set.Streams != nil {
		t.Fatalf("FindFirstStreamW 缺失时 Streams 必须为 nil，实际得到 %T", set.Streams)
	}
	// 其它五项能力不应被这次降级连坐：
	if set.Sparse == nil {
		t.Error("Sparse 不应因 Streams 缺失而变 nil")
	}
	if set.IDs == nil {
		t.Error("IDs 不应因 Streams 缺失而变 nil")
	}
	if set.Times == nil {
		t.Error("Times 不应因 Streams 缺失而变 nil")
	}
	if set.DOS == nil {
		t.Error("DOS 不应因 Streams 缺失而变 nil")
	}
	if set.Xattr != nil {
		t.Error("Windows 上 Xattr 应恒为 nil，与本次降级无关")
	}
}

// TestWindowsStreamProcsPresentKeepsStreams 反向对照：导出齐全时 Streams 必须非 nil。
// 少了这一条，一个把 Streams 永远设成 nil 的实现也能让上面的用例通过。
func TestWindowsStreamProcsPresentKeepsStreams(t *testing.T) {
	o := oscap.Options{Root: `C:\tmp`}
	set, err := newSetProcs(o, true)
	if err != nil {
		t.Fatalf("newSetProcs(o, true) 不应返回错误: %v", err)
	}
	if set.Streams == nil {
		t.Fatal("FindFirstStreamW 齐全时 Streams 必须为非 nil")
	}
}
