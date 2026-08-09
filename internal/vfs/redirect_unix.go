//go:build !windows

package vfs

// redirect_unix.go —— 「这一级会不会把名字重定向到别处」的 POSIX 侧判定。
//
// 本文件是**纯粹的现状固化**：两个函数就是 path.go 原先内联写着的那两句，
// 一个字没改。抽成平台钩子只是为了让 Windows 侧能给出不同的答案
// （见 redirect_windows.go：Go 在 Windows 上认不出 junction）。
//
// Linux / macOS 上行为零变化，现有用例原样回归。

import (
	"os"
	"path/filepath"
)

// hostIsRedirect 报告 fi 描述的对象是否是「会把名字指向别处」的东西。
//
// POSIX 上就是符号链接，没有第二种。参数 path 在这里用不到，
// 留着是因为 Windows 侧需要它去取重解析标记。
func hostIsRedirect(_ string, fi os.FileInfo) bool {
	return fi.Mode()&os.ModeSymlink != 0
}

// hostEvalRedirect 求出 link 最终指向的宿主路径，形式与 Resolver.root 一致
// （即普通绝对路径），以便直接交给 Resolver.contains 比较。
//
// 目标不存在（悬空软链）时返回的 error 满足 os.IsNotExist，
// 调用方据此退化为词法判断 —— 这是 path.go 原有的语义，此处保持不变。
func hostEvalRedirect(link string) (string, error) {
	return filepath.EvalSymlinks(link)
}
