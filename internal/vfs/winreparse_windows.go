//go:build windows

package vfs

// winreparse_windows.go —— winreparse.go 那些纯判定规则的**系统调用取值侧**。
//
// ⚠️ 本文件的每一个函数都**没有在真 Windows 上执行过**。本项目没有 Windows
// 机器（AGENTS.md §10.3），能做的静态保证只有：交叉编译通过、
// `GOOS=windows go vet` 通过、以及所有能剥出来的判断都已经剥进 winreparse.go
// 并在 Linux 上做了表驱动测试。判定规则是验过的，**这里的取值不是**。
//
// 所以本文件刻意写得极薄：每个函数只做「调一次 Win32、把结果原样递出去」，
// 不掺任何判断。薄到肉眼可查，是我们目前唯一能给的保证。

import (
	"os"

	"golang.org/x/sys/windows"
)

// GetFinalPathNameByHandleW 的 flags（fileapi.h）。
// x/sys@v0.47.0 没有导出这几个常量，按 winbase.h 的定义手写。
const (
	// FILE_NAME_NORMALIZED = 0x0：返回规范化的路径（解析掉 8.3 短名、
	// 大小写取磁盘上的真实形式）。这正是 winPathContains 能做精确比较的前提。
	winFileNameNormalized uint32 = 0x0
	// VOLUME_NAME_DOS = 0x0：卷部分用盘符形式（`\\?\C:\...`）而不是
	// 卷 GUID。root 与目标必须用**同一个** flag 取，否则两边形式不同、
	// 前缀比较必然失败。
	winVolumeNameDOS uint32 = 0x0
)

// hostReparseInfo 取出 path **自身**（不跟随重解析点）的文件属性与重解析标记。
//
// 用 FindFirstFileW 而不是 GetFileAttributesEx，因为只有前者会在
// `WIN32_FIND_DATA.dwReserved0` 里给出重解析标记 —— 这也正是 Go 标准库
// 自己的取法（go1.25 `src/os/types_windows.go:144`，`fs.ReparseTag = d.Reserved0`）。
// GetFileAttributesEx 根本不返回 tag，拿它没法区分 junction 和 OneDrive 占位文件。
//
// FindFirstFileW 不跟随重解析点，返回的就是这一级对象自身的信息，符合我们
// 「逐级检查每一个分量」的需要。
//
// 返回的 tag **只有在 attrs 带 FILE_ATTRIBUTE_REPARSE_POINT 时才有意义**，
// 其余情况下 dwReserved0 存的是别的东西。这个前提由 winIsRedirect 负责把关，
// 本函数原样返回、不做判断。
func hostReparseInfo(path string) (attrs, tag uint32, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, ErrInvalidPath
	}
	var data windows.Win32finddata
	h, err := windows.FindFirstFile(p, &data)
	if err != nil {
		return 0, 0, err
	}
	windows.FindClose(h)
	return data.FileAttributes, data.Reserved0, nil
}

// hostFinalPath 返回 path 实际指向的对象的**规范绝对路径**，形如
// `\\?\C:\srv\share\a.txt`。
//
// 它是本方案里最有价值的一个原语：**内核替我们把整条路径上的所有重解析点
// 都解析完了**，无论 junction 出现在第几级、是哪一种重定向机制。我们自己
// 逐级走链条永远会漏掉新出现的机制，问内核不会。
//
// 三个刻意的选择：
//
//   - dwDesiredAccess = 0。查路径不需要任何访问权，给 0 可以避免在
//     「存在但我们无权读」的对象上白白吃一个 ACCESS_DENIED。
//   - 带 FILE_FLAG_BACKUP_SEMANTICS：不带它**打不开目录句柄**。
//   - **不带** FILE_FLAG_OPEN_REPARSE_POINT：这里的目的恰恰是要跟随重解析点，
//     看看它最终落在哪。这与 winOpenParamsFor 里永远带该 flag 的取向相反，
//     不是笔误。
//
// ShareMode 给全（含 DELETE），否则我们这次纯查询会短暂挡住别人的正常操作。
func hostFinalPath(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", ErrInvalidPath
	}
	h, err := windows.CreateFile(
		p,
		0, // 只查路径，不要任何访问权
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return finalPathOf(h)
}

// 现状说明（重要，别误读成「第二层已经做了」）
// ---------------------------------------------------------------------------
// hostFinalPath 是**拿一个路径自己去开一个句柄**再反查，它不是
// 「打开之后对已打开的句柄反查」那一层。二者的区别决定了 TOCTOU 窗口在不在：
//
//   - 现在的做法：CreateFile(path) → GetFinalPathNameByHandleW → CloseHandle，
//     查完之后调用方还要**再开一次**真正的句柄。中间那道缝就是 TOCTOU 窗口：
//     攻击者可以在这两次打开之间把路径换成 junction，而我们拿到的结论已经过期。
//   - 第二层要的做法：对**本次真正要用的那个句柄**反查，查完不合规则立刻关掉。
//     因为查的就是要用的那个，中间没有缝。
//
// 也就是说：本文件提供了第二层需要的**原语**（finalPathOf），但第二层本身
// **尚未接线** —— open 路径目前没有在拿到句柄之后回过头来反查它。
// 接线缺的是 open 路径的配合（要把句柄交给这一层、并在判定失败时关掉它），
// 以及可移植的部分怎么在 Linux 上做注入断言（真竞态在 Windows 上才跑得出来）。
// 跟踪见仓库 Issue #110。
//
// 两层不能互相替代，别想着二选一（理由见 winreparse.go 顶部）：
//   - 只有第一层挡不住 TOCTOU 竞态；
//   - 只有第二层也不行 —— 带「创建/清空」标志打开时，破坏在反查之前就已经发生。

// finalPathOf 对一个已经打开的句柄取规范路径。
//
// 单独拆出来是因为「打开后再校验」那一层（见 winreparse.go 顶部说明）需要
// 直接作用在**已有句柄**上 —— 在已打开的对象上求值才是免疫 TOCTOU 的关键，
// 拿路径重新打开一次就又出现了竞态窗口。
func finalPathOf(h windows.Handle) (string, error) {
	const flags = winFileNameNormalized | winVolumeNameDOS

	// 缓冲区协议（MS Docs "GetFinalPathNameByHandleW" 返回值一节）：
	//   - 成功：返回写入的 WCHAR 数，**不含**结尾 NUL，因此必然 < len(buf)；
	//   - 缓冲区太小：返回所需 WCHAR 数，**含**结尾 NUL，且必然 >= len(buf)。
	// 两种情况都不返回错误码（x/sys 的包装只在返回 0 时置 err），
	// 所以判据是比长度而不是看 err。
	//
	// 据此扩容重试一次即可，不写成无限循环：所需长度由内核直接给出，
	// 按它扩容后还不够就是真出错了，循环下去只会掩盖问题。
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
	if err != nil {
		return "", err
	}
	if n >= uint32(len(buf)) {
		buf = make([]uint16, n+1)
		n, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
		if err != nil {
			return "", err
		}
		if n >= uint32(len(buf)) {
			return "", ErrInvalidPath
		}
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// finalPathOfFile 是 finalPathOf 对 *os.File 的包装。
func finalPathOfFile(f *os.File) (string, error) {
	return finalPathOf(windows.Handle(f.Fd()))
}
