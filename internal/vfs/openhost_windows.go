//go:build windows

package vfs

import (
	"os"

	"golang.org/x/sys/windows"
)

// openHostFile 是「打开宿主文件」的统一接缝（openHostFile seam）。
// 总说明见 openhost_unix.go 顶部：为什么要把所有宿主文件打开收口到这里。
//
// 本文件是第二步：**自己调 CreateFileW**，而不是转调 os.OpenFile。
//
// 为什么必须自己调（详见 winopen.go 文件头）：Go 的 `syscall.Open`
// （go1.25 `syscall/syscall_windows.go:396`）用的 sharemode 是
// `FILE_SHARE_READ|FILE_SHARE_WRITE`，**缺 FILE_SHARE_DELETE** —— 于是只要
// 我们还开着一个句柄，**任何**别的句柄（包括我们自己）就改不了名、删不掉这个
// 文件。SMB 的共享冲突应当由 server 层按 MS-SMB2 的 ShareAccess 判定，而不是
// 借宿主句柄语义顺带实现 —— 借了会把协议上合法的改名/删除也一起挡掉。
//
// 顺带把 `FILE_FLAG_OPEN_REPARSE_POINT` 常开，补上 Windows 侧缺失的
// 「不跟随最后一跳」防护（即 sys_windows.go 里 `openNoFollow = 0` 那个缺口，
// 与 POSIX 的 O_NOFOLLOW 对齐）。
//
// 原先暂缓第二步的理由是「本项目没有 Windows runner，写下去等于赌一个没法验的
// 假设：os.NewFile(handle) 返回的 *os.File 能否正常支撑 Seek / ReadAt /
// WriteAt / Truncate」。现在 GitHub Actions 的 windows-latest 会真跑这条路径，
// 假设可验，故接线。
//
// 入参翻译全部在 winopen.go 的 winOpenParamsFor（无 build tag，在 Linux 上有
// 表驱动测试）；本文件只做「调 CreateFileW、把句柄包成 *os.File」，薄到肉眼可查。
func openHostFile(host string, flag int, perm os.FileMode) (*os.File, error) {
	p, err := winOpenParamsFor(flag, perm)
	if err != nil {
		return nil, err
	}

	pathp, err := windows.UTF16PtrFromString(host)
	if err != nil {
		// 含 NUL 之类的非法名字：如实当路径非法，而不是交给内核去猜。
		return nil, &os.PathError{Op: "open", Path: host, Err: ErrInvalidPath}
	}
	h, err := windows.CreateFile(
		pathp,
		p.Access,
		p.ShareMode,
		nil, // lpSecurityAttributes：本项目禁止 fork/exec，不需要可继承句柄
		p.CreateDisposition,
		p.FlagsAndAttributes,
		0, // hTemplateFile
	)
	if err != nil {
		// 包成 *os.PathError：errno 会沿 Unwrap 链被 mapError 的
		// errors.As(err, &errno) 取到（errmap.go），与 os.OpenFile 的返回
		// 形态一致，调用方无需区分这两条实现。
		return nil, &os.PathError{Op: "open", Path: host, Err: err}
	}

	f := os.NewFile(uintptr(h), host)

	// O_TRUNC 走「开完再截断」而不是 CREATE_ALWAYS：CREATE_ALWAYS 配上
	// READONLY 属性会把已存在的文件**替换**成一个新的只读文件，而不是截断它
	// （go.dev/issue/38225）。winOpenParamsFor 已经把「要不要补这一刀」算好。
	if p.TruncateAfterOpen {
		if err := f.Truncate(0); err != nil {
			// 截断失败就别把半成品句柄交出去：关掉再报错，避免调用方拿到一个
			// 语义与请求不符的句柄。
			_ = f.Close()
			return nil, &os.PathError{Op: "truncate", Path: host, Err: err}
		}
	}
	return f, nil
}
