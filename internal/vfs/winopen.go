package vfs

// winopen.go —— `os.OpenFile` 的 flag 到 Win32 `CreateFileW` 入参的**纯翻译**。
//
// 没有 build tag、不 import `golang.org/x/sys/windows`、不做任何系统调用，
// 因此可以在 Linux 上做表驱动测试。真正的 `CreateFileW` 调用在
// open_windows.go —— 那部分我们没有 Windows 机器可以跑，所以能挪到这里的
// 判断逻辑就都挪到这里，让"不可测的那一半"尽可能薄。
//
// # 为什么不直接用 os.OpenFile
//
// Go 的 `syscall.Open`（go1.25 `syscall/syscall_windows.go:365`）对文件服务器
// 来说有三处不合适，且都**没有**通过 `os.OpenFile` 暴露出来可以调整：
//
//  1. `sharemode = FILE_SHARE_READ|FILE_SHARE_WRITE`（:396），**缺 DELETE**。
//     于是我们自己打开着的文件，别人（包括我们自己的另一个句柄）就改不了名、
//     删不掉。SMB 的共享冲突判定应当由 server 层按 MS-SMB2 的 ShareAccess
//     来做，而不是借宿主的句柄语义顺带实现 —— 借了就会把协议上合法的操作
//     也一起挡掉。
//  2. `FILE_FLAG_OPEN_REPARSE_POINT` 只在 `CREATE_NEW` 时加（:430）。
//     其余情况一律**跟随**重解析点，等于 Linux 那边 `O_NOFOLLOW` 的防护在
//     Windows 上整个不存在。
//  3. 句柄默认**可继承**（:398 `makeInheritSa()`，除非传 `O_CLOEXEC`）。
//     本项目禁止 fork/exec（AGENTS.md C3），可继承句柄是纯粹的多余攻击面。
//
// 另有一处我们**刻意跟随** Go 的做法：不使用 `CREATE_ALWAYS` /
// `TRUNCATE_EXISTING`，而是开完之后再 ftruncate。原因见 go.dev/issue/38225：
// `CREATE_ALWAYS` 配上 `FILE_ATTRIBUTE_READONLY` 会把已存在的文件**替换**成
// 一个新的只读文件，而不是截断它。

import (
	"fmt"
	"os"
)

// Win32 常量。手写而不是 import `golang.org/x/sys/windows`，因为那个包所有
// 文件都带 `//go:build windows`，本文件要在 Linux 上编译。
//
// 每个值都已与 `golang.org/x/sys@v0.47.0/windows` 的定义逐一核对
// （`security_windows.go` / `types_windows.go`），出处见各自注释。
const (
	// —— dwDesiredAccess（winnt.h）——
	winGenericRead  uint32 = 0x80000000
	winGenericWrite uint32 = 0x40000000

	// —— dwShareMode（winnt.h）——
	winFileShareRead   uint32 = 0x00000001
	winFileShareWrite  uint32 = 0x00000002
	winFileShareDelete uint32 = 0x00000004

	// —— dwCreationDisposition（fileapi.h）——
	winCreateNew    uint32 = 1
	winOpenExisting uint32 = 3
	winOpenAlways   uint32 = 4

	// —— dwFlagsAndAttributes（winnt.h）——
	winFileAttributeReadonly uint32 = 0x00000001
	winFileAttributeNormal   uint32 = 0x00000080
	// FILE_FLAG_OPEN_REPARSE_POINT：拿到重解析点**本身**的句柄而不是跟随它。
	// 这是 Windows 上最接近 POSIX `O_NOFOLLOW` 的东西。
	winFileFlagOpenReparsePoint uint32 = 0x00200000
	// FILE_FLAG_BACKUP_SEMANTICS：不带它就**打不开目录句柄**。
	winFileFlagBackupSemantics uint32 = 0x02000000
)

// 重解析标记的位定义（MS-FSCC §2.1.2.1 Reparse Tags，winnt.h 的
// IsReparseTagMicrosoft / IsReparseTagNameSurrogate / IsReparseTagDirectory）。
//
//	bit 31 (0x80000000) M —— Microsoft 保留的标记
//	bit 29 (0x20000000) N —— **name surrogate**：该对象代表系统中的另一个具名实体
//	bit 28 (0x10000000) D —— 目录
const winReparseTagNameSurrogate uint32 = 0x20000000

// isNameSurrogateTag 判断一个重解析标记是否会把名字**重定向到别处**。
//
// 这是本文件里最容易写错、也最值得解释的一条规则。
//
// 直觉写法是"看到 FILE_ATTRIBUTE_REPARSE_POINT 就拒绝"，那是**错的**，会造成
// 大面积误伤：NTFS 上一大堆完全正常的文件带着重解析点 ——
//
//	IO_REPARSE_TAG_DEDUP        0x80000013  重复数据删除（文件服务器上极常见）
//	IO_REPARSE_TAG_CLOUD*       0x9000001A… OneDrive 等云同步的占位文件
//	IO_REPARSE_TAG_WCI*         0x80000018  Windows 容器镜像的分层文件
//	IO_REPARSE_TAG_APPEXECLINK  0x8000001B  应用执行别名
//
// 这些都是**同一个对象的另一种存储形态**，不改变名字指向何处。把它们拒掉，
// 等于让开了重删的卷、或 OneDrive 目录整个不可共享。
//
// 真正危险的是会把名字指到别处的那一类，Windows 自己用 name surrogate 位
// 标记它们：
//
//	IO_REPARSE_TAG_SYMLINK      0xA000000C  ✔ 0xA0000000 & N != 0
//	IO_REPARSE_TAG_MOUNT_POINT  0xA0000003  ✔ junction，Go 的 os 包**不**认它是符号链接
//	IO_REPARSE_TAG_LX_SYMLINK   0xA000001D  ✔ WSL 建的符号链接
//
// 所以判据用位测试而不是枚举具体标记：新出现的重定向类标记会自动被覆盖，
// 而新出现的存储形态类标记不会被误伤。**失败方向也是对的** —— 一个不该带
// N 位却带了的标记会被拒（拒绝=安全），反之才是漏。
func isNameSurrogateTag(tag uint32) bool {
	return tag&winReparseTagNameSurrogate != 0
}

// winOpenParams 是 CreateFileW 的入参（省掉恒为 nil / 0 的
// lpSecurityAttributes 与 hTemplateFile）。
type winOpenParams struct {
	Access             uint32 // dwDesiredAccess
	ShareMode          uint32 // dwShareMode
	CreateDisposition  uint32 // dwCreationDisposition
	FlagsAndAttributes uint32 // dwFlagsAndAttributes

	// TruncateAfterOpen 表示调用方必须在拿到句柄之后自己截断到 0。
	// 见文件头对 go.dev/issue/38225 的说明。
	TruncateAfterOpen bool
}

// winOpenParamsFor 把 os.OpenFile 的 (flag, perm) 翻译成 CreateFileW 的入参。
//
// perm 只有属主写位（0o200）参与判断，与 Go 的 `perm&S_IWRITE` 一致；
// 其余权限位在 NTFS 上无对应物，由旁路存储承载（AGENTS.md §5 P7）。
// 注意 dwFlagsAndAttributes 里的属性位**只在文件被创建时生效**，
// 打开已存在的文件时被忽略 —— 这是 Win32 的既定行为，不是本函数的疏漏。
func winOpenParamsFor(flag int, perm os.FileMode) (winOpenParams, error) {
	// O_APPEND 在本项目里是**禁止**的，不是"未实现"。
	// SMB2 WRITE 永远带显式 offset（pwrite 语义），内核替我们改写 offset
	// 会让文件内容错乱；协议里的"追加"是客户端发 offset=0xFFFF...，由 server
	// 层换算成当前 EOF（详见 local.go accessFlags 的说明）。
	// 这里显式拒绝而不是默默翻译，是为了让将来任何一个误加 O_APPEND 的调用
	// 当场失败，而不是产出一个内容错乱的文件。
	if flag&os.O_APPEND != 0 {
		return winOpenParams{}, fmt.Errorf("%w: openHostFile 不接受 O_APPEND", ErrInvalidArg)
	}

	var p winOpenParams

	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		p.Access = winGenericWrite
	case os.O_RDWR:
		p.Access = winGenericRead | winGenericWrite
	default: // os.O_RDONLY == 0
		p.Access = winGenericRead
	}
	if flag&os.O_CREATE != 0 {
		// 要创建就必须有写权限，否则 CreateFileW 直接 ACCESS_DENIED。
		p.Access |= winGenericWrite
	}

	// 永远带 DELETE：见文件头第 1 条。
	p.ShareMode = winFileShareRead | winFileShareWrite | winFileShareDelete

	switch {
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		p.CreateDisposition = winCreateNew
	case flag&os.O_CREATE != 0:
		p.CreateDisposition = winOpenAlways
	default:
		p.CreateDisposition = winOpenExisting
	}

	// O_TRUNC 走"开完再截断"。CREATE_NEW 一定是新建的空文件，没什么可截断；
	// OPEN_ALWAYS 命中新建时截断一个 0 字节文件也是空操作，所以不必像 Go
	// 那样再去看 ERROR_ALREADY_EXISTS，行为等价而少一个分支。
	p.TruncateAfterOpen = flag&os.O_TRUNC != 0 && p.CreateDisposition != winCreateNew

	attrs := winFileAttributeNormal
	if perm&0o200 == 0 {
		attrs = winFileAttributeReadonly
	}
	// 永远带这两个：见文件头第 2 条，以及"不带 BACKUP_SEMANTICS 打不开目录"。
	//
	// 关于 BACKUP_SEMANTICS 与 Go 的差异：Go 只在只读打开时加它（:405~:416），
	// 好让"带写意图打开目录"自然地 ACCESS_DENIED，再映射成 EISDIR 去贴合
	// POSIX。我们不这么做 —— 靠一个错误码的副作用来表达语义太脆。
	// openHostFile 在拿到句柄后显式检查"是不是目录"，判据明确、跨平台一致。
	attrs |= winFileFlagBackupSemantics | winFileFlagOpenReparsePoint
	p.FlagsAndAttributes = attrs

	return p, nil
}
