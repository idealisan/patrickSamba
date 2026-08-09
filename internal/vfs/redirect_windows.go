//go:build windows

package vfs

// redirect_windows.go —— 「这一级会不会把名字重定向到别处」的 Windows 侧判定。
//
// 存在的理由（详细论证见 winreparse.go 顶部）：Go 1.23+ 在 Windows 上
// **只**给 IO_REPARSE_TAG_SYMLINK 点亮 ModeSymlink，junction 与 WSL 符号链接
// 拿到的是 ModeIrregular。于是 POSIX 那套 `Mode()&ModeSymlink` 判据在这里
// 漏得很彻底，`filepath.EvalSymlinks` 同样不解析 junction
// （它内部用的是同一个判据）。
//
// ⚠️ 本文件依赖的两个取值函数（hostReparseInfo / hostFinalPath）
// **未在真 Windows 上执行过**，见 winreparse_windows.go 顶部的说明。
// 判定规则本身在 Linux 上有表驱动测试（winreparse_test.go）。

import "os"

// hostIsRedirect 报告 path 是否是会把名字指向别处的重解析点。
//
// 判定分两步，且**保留 POSIX 那一路**：
//
//  1. fi 已经说是符号链接 → 直接算数，连系统调用都省了
//     （IO_REPARSE_TAG_SYMLINK 这条 Go 认得）；
//  2. 否则只有在 fi 是 ModeIrregular 时才值得再问一次内核。
//     Go 把「重解析点但不是 symlink/socket/dedup」一律标成 ModeIrregular，
//     所以 junction 必定落在这一档。用它当前置过滤，可以让**绝大多数普通
//     文件和目录一次系统调用都不多花** —— 逐级解析是热路径，
//     每个分量都去 FindFirstFileW 一次是不可接受的。
//
// 注意第 2 步是「必要条件」不是「充分条件」：ModeIrregular 里混着重删、
// OneDrive 占位、容器分层文件这些**不重定向**的重解析点，把它们拒掉会让整卷
// 不可共享。所以还要靠 winIsRedirect 按 name surrogate 位精确区分。
//
// 取标记失败时返回 false：此时 fi 已经表明它不是符号链接，我们只是没能进一步
// 确认它是不是 junction。这里不能 fail closed 成 true —— 那会把取值失败
// （例如并发删除导致的 FindFirstFileW 失败）变成「拒绝访问」，
// 制造出比缺口更常见的误拒。真正的兜底在打开之后的 final path 校验。
func hostIsRedirect(path string, fi os.FileInfo) bool {
	if fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	if fi.Mode()&os.ModeIrregular == 0 {
		return false
	}
	attrs, tag, err := hostReparseInfo(path)
	if err != nil {
		return false
	}
	return winIsRedirect(attrs, tag)
}

// hostEvalRedirect 求出 link 最终指向的宿主路径。
//
// 不能用 filepath.EvalSymlinks：它在 Windows 上不解析 junction（见文件头）。
// 改为问内核要 final path —— 内核会把整条路径上的所有重解析点一次解析完，
// 不管是哪一种机制、出现在第几级。
//
// 返回前把 `\\?\` 形式还原成普通形式，好让调用方直接拿去和 Resolver.root
// （EvalSymlinks 产出的普通形式）比较。还原不了的形式原样返回，
// 于是在前缀比较里匹配不上、判为共享外 —— 失败方向是拒绝。
func hostEvalRedirect(link string) (string, error) {
	p, err := hostFinalPath(link)
	if err != nil {
		return "", err
	}
	return winStripLongPathPrefix(p), nil
}
