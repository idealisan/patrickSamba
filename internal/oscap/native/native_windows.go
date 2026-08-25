//go:build windows

package native

// native_windows.go —— Windows 上的原生能力清单与共用的句柄/错误封装。
//
// 与 internal/oscap/probe_windows.go 逐项对齐：
//
//	能力              probe_windows.go            本文件
//	xattr             固定 false                   nil ← 见下
//	named_stream      probeADS                    winStreams（NTFS ADS）
//	sparse_file       probeSparseWindows          winSparse
//	stable_file_id    probeFileIndex              winIDs
//	creation_time     os.Stat 成功即 true          winTimes（可读可写）
//	dos_attributes    probeFileAttributes         winDOS
//
// # C9 合规声明（AGENTS.md §1.2）
//
// 本文件通过 golang.org/x/sys/windows 调用 Win32 API，其底层是
// `NewLazySystemDLL("kernel32.dll")` 一类的**平台调用约定**，
// §1.2 明确把它排除在 C2「禁止依赖外部动态库」之外 ——
// Windows 没有稳定的系统调用号，DLL 导出函数就是官方 ABI 边界，
// Go runtime 的 os/net/time 本身也是这么干的。
//
// 本包**不出现** NewLazyDLL / LoadLibrary（它们不走 System32 安全加载路径，
// 是 DLL 劫持的经典入口），也不出现运行时拼出来的 DLL 名。
// 唯一一处直接的 LazySystemDLL 在 stream_windows.go，取的是
// kernel32 的 FindFirstStreamW/FindNextStreamW（x/sys/windows 没封装它们）。

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

func newSet(o oscap.Options) (oscap.Set, error) {
	// 命名流要用 kernel32 的 FindFirstStreamW/FindNextStreamW/GetFileSizeEx，
	// 而 LazyProc.Call 找不到导出**会 panic**。所以在这里问一次：缺任何一个
	// 就整项留 nil 让 builtin 接管（纪律 2「做不到的整项留 nil」）。
	//
	// 注意这一项与 probe_windows.go 的 probeADS **判据不同**且刻意如此：
	// probeADS 只试着打开一条流（用的是 CreateFile，不碰这三个导出），
	// 所以在缺导出的宿主上它会报 true 而这里是 nil。后果是安全的 ——
	// auto 模式落 builtin，两条路都不会跑到运行期才 panic。
	return newSetProcs(o, streamProcsAvailable() == nil)
}

// newSetProcs 是 newSet 的可注入版本，供测试构造「FindFirstStreamW 缺失」场景：
// 传 procsOK=false 时 Streams 必须为 nil（整项判为不支持、落 builtin），
// 而不是返回一个「有时能有时 panic」的半吊子。这正是 capability.go 要求的
// 「半吊子必须整项判为不支持」，也是本项目在 encryption_required 上栽过的同型坑。
func newSetProcs(o oscap.Options, procsOK bool) (oscap.Set, error) {
	// 三项导出齐全才整项启用；否则 Streams 整项留 nil（纪律 2）。
	var streams oscap.NamedStream
	if procsOK {
		streams = &winStreams{readOnly: o.ReadOnly}
	}

	return oscap.Set{
		// Windows 没有 POSIX 扩展属性，**刻意留 nil**。
		//
		// 别把 NTFS 的 ADS 当成 xattr 的等价物拿来充数：二者语义不同
		// （ADS 是可任意大的数据流，xattr 是小块属性，枚举方式、大小限制、
		// 命名规则都不一样）。真要在 Windows 上提供 xattr 语义，
		// 那是 builtin 用旁路存储做的事。probe_windows.go 同样判 false。
		Xattr: nil,

		Sparse:  &winSparse{readOnly: o.ReadOnly},
		Streams: streams,
		IDs:     winIDs{},
		Times:   &winTimes{readOnly: o.ReadOnly},
		DOS:     &winDOS{readOnly: o.ReadOnly},

		// 元数据迁移是无操作：NTFS 的属性字/ADS/btime 全部长在文件上，
		// 随 rename/unlink 自动跟随/消失（migration.go 有逐项清单）。
		Migration: noopMigration{},

		Close: nil,
	}, nil
}

// winShareAll 是我们打开句柄时给出的共享模式。
//
// **必须带 FILE_SHARE_DELETE**：少了它，我们为了查一次属性而临时打开的句柄
// 会让客户端在这段时间里删不掉、改不了名。SMB 的共享冲突判定应当由 server 层
// 按 MS-SMB2 的 ShareAccess 来做，不能被 oscap 这一层顺带实现掉
// （同 vfs/winopen.go 的说明）。
const winShareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE |
	windows.FILE_SHARE_DELETE

// winOpenFlags 是打开句柄时的标志。
//
//   - FILE_FLAG_BACKUP_SEMANTICS：不带它就**打不开目录句柄**，
//     而 DOS 属性/创建时间在目录上同样要能读写。
//   - FILE_FLAG_OPEN_REPARSE_POINT：拿到重解析点**本身**的句柄而不是跟随它，
//     这是 Windows 上最接近 POSIX O_NOFOLLOW 的东西（AGENTS.md §8）。
const winOpenFlags = windows.FILE_FLAG_BACKUP_SEMANTICS |
	windows.FILE_FLAG_OPEN_REPARSE_POINT

// winHandle 取一个可用的句柄。
//
// 优先复用调用方的句柄（它已经带着 SMB 协商出来的访问权），没有时才自己
// 按路径开一个。cleanup 只关我们自己开的那个 —— 关掉别人的句柄会让上层的
// 后续读写莫名其妙地失败，是最难查的一类 bug。
//
// 注意复用时**不检查**它的访问权是否够：够不够由后续调用如实报
// ERROR_ACCESS_DENIED，比我们在这里猜一套权限模型可靠。
func winHandle(ref oscap.Ref, access uint32) (h windows.Handle, cleanup func(), err error) {
	if ref.Handle != nil {
		return windows.Handle(ref.Handle.Fd()), func() {}, nil
	}
	p, err := windows.UTF16PtrFromString(ref.Path)
	if err != nil {
		// 路径里有 NUL —— 只可能是上层拼错了，不是环境问题。
		return 0, nil, fmt.Errorf("oscap/native: 路径 %q 非法: %w", ref.Path, oscap.ErrInvalidArg)
	}
	h, err = windows.CreateFile(p, access, winShareAll, nil,
		windows.OPEN_EXISTING, winOpenFlags, 0)
	if err != nil {
		return 0, nil, mapWinErr(err)
	}
	return h, func() { _ = windows.CloseHandle(h) }, nil
}

// mapWinErr 把 Win32 错误码映射成 oscap 的 sentinel。
//
// 认不出来的原样返回：把「磁盘满」「配额超限」笼统地说成「不支持」，
// 只会让上层去 builtin 白试一遍，而日志里什么线索都不剩。
func mapWinErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND),
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return oscap.ErrNotFound
	case errors.Is(err, windows.ERROR_INVALID_FUNCTION),
		errors.Is(err, windows.ERROR_NOT_SUPPORTED):
		// FAT32/exFAT 卷上稀疏文件与 ADS 的典型报错。
		return oscap.ErrNotSupported
	case errors.Is(err, windows.ERROR_WRITE_PROTECT):
		return oscap.ErrReadOnly
	case errors.Is(err, windows.ERROR_FILE_EXISTS),
		errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		return oscap.ErrExist
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
		return oscap.ErrInvalidArg
	}
	return err
}

// winFileSize 从句柄取当前文件长度（EOF），并告知它是不是目录。
func winFileSize(h windows.Handle) (size int64, isDir bool, err error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, false, mapWinErr(err)
	}
	size = int64(uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow))
	isDir = info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	return size, isDir, nil
}
