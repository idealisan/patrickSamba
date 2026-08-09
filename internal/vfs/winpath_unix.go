//go:build !windows

// 注意：文件名后缀 `_unix` 在 Go 里**没有**隐含的构建约束（不像 `_windows`
// / `_linux` / `_darwin`），生效的是上面这行显式 tag。
//
// 用 `!windows` 而不是 Go 1.19 起支持的 `unix`，是因为 `unix` 不覆盖
// js/wasm、plan9 等目标，那些平台会因缺少常量而编译失败。本文件的语义本来
// 就是「非 Windows 宿主」，用取反表达最贴切，也不会随 GOOS 列表增长而漏。

package vfs

// hostNormalizesTrailingDotSpace 表示宿主的路径层会裁掉每个分量结尾的
// 点和空格，从而让一个对象拥有多个名字。
//
// POSIX：false。Linux/macOS 的内核不裁任何字符，`"报告.txt "` 是一个与
// `"报告.txt"` 不同的、完全合法的文件。若在这里也拒绝，等于让现有共享里
// 这类文件从此不可访问——用真实回归去换一个本平台上并不存在的威胁。
// 完整论证见 winpath.go 文件头。
const hostNormalizesTrailingDotSpace = false
