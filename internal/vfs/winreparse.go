package vfs

// winreparse.go —— Windows 重解析点（reparse point）与「共享根包含性」的**纯判定逻辑**。
//
// 和 winopen.go 同一个套路，理由也一样：没有 build tag、不 import
// `golang.org/x/sys/windows`、不做任何系统调用，因此可以在 Linux 上做表驱动
// 测试。真正的 `FindFirstFileW` / `GetFinalPathNameByHandleW` 调用在
// winreparse_windows.go —— 那部分我们没有 Windows 机器可以跑，所以能剥出来的
// 判断就都剥到这里，让「不可测的那一半」尽可能薄。
//
// ⚠️ 本文件的测试证明的是**判定规则正确**，不是「在 Windows 上跑通了」。
//
// # 为什么需要这个文件：Go 在 Windows 上认不出 junction
//
// path.go 的 resolveComponents 用 `fi.Mode()&os.ModeSymlink != 0` 判断
// 「这一级是不是链接」，这在 POSIX 上是对的，在 Windows 上**漏得很彻底**。
//
// Go 1.23 起（go1.25 `src/os/types_windows.go:206`）的 mode 判定是：
//
//	if attrs&FILE_ATTRIBUTE_REPARSE_POINT != 0 {
//		switch tag {
//		case IO_REPARSE_TAG_SYMLINK: m |= ModeSymlink   // 只有它
//		case IO_REPARSE_TAG_AF_UNIX: m |= ModeSocket
//		case IO_REPARSE_TAG_DEDUP:                      // 当普通文件
//		default:                     m |= ModeIrregular // junction 落这里
//		}
//	}
//
// 于是 **junction（IO_REPARSE_TAG_MOUNT_POINT）与 WSL 符号链接
// （IO_REPARSE_TAG_LX_SYMLINK）拿到的是 ModeIrregular 而不是 ModeSymlink**，
// 从 resolveComponents 的判据下面整个穿过去，checkSymlink 根本不会被调用。
//
// 兜底也是漏的：`filepath.EvalSymlinks` 在
// `src/path/filepath/symlink.go:90` 用的同样是 `fi.Mode()&fs.ModeSymlink == 0`，
// 所以 **Go 在 Windows 上压根不解析 junction**。
//
// 结果就是：共享内放一个 junction，路径拼接出来的宿主路径始终"在根内"
// （contains() 做的是纯字符串前缀比较），而内核在真实路径遍历时会跟随它，
// 实际读写落到共享外。这是实打实的路径穿越（AGENTS.md §8）。

import "strings"

// Win32 文件属性位（winnt.h）。手写而不是 import `golang.org/x/sys/windows`，
// 原因同 winopen.go：那个包所有文件都带 `//go:build windows`，本文件要在
// Linux 上编译。值已与 `golang.org/x/sys@v0.47.0/windows/types_windows.go` 核对。
const winFileAttributeReparsePoint uint32 = 0x00000400

// winIsRedirect 判断一个宿主对象是否会把名字**重定向到别处**，
// 也就是「这一级需不需要当作链接来做逃逸校验」。
//
// 入参刻意只要 (attrs, tag) 这两个纯数值而不是 os.FileInfo：这样判定规则不带
// 任何系统调用，能在 Linux 上完整测试；取值的那一小段留在 _windows.go 里。
//
// 判据分两段，缺一不可：
//
//  1. 必须真的带 FILE_ATTRIBUTE_REPARSE_POINT。不带这个属性时 tag 字段
//     （WIN32_FIND_DATA.dwReserved0）**存的是别的东西**，不能拿来判断 ——
//     直接读会把普通文件误判成链接。
//  2. tag 必须是 name surrogate，见 isNameSurrogateTag（winopen.go）。
//
// 第 2 条为什么不是「见 reparse point 就拒」，winopen.go 里已经写透了：
// 重删、OneDrive 占位、容器分层文件都是重解析点但**不重定向名字**，
// 拒掉它们等于让整个卷不可共享。
//
// 这个位测试与 Go 标准库自己的 `fileStat.isReparseTagNameSurrogate()`
// （`src/os/types_windows.go:154`）是同一个式子，不是我们另发明的一套。
func winIsRedirect(attrs, tag uint32) bool {
	if attrs&winFileAttributeReparsePoint == 0 {
		return false
	}
	return isNameSurrogateTag(tag)
}

// winLegacyIsRedirect 是**修复前**的判据，只为测试而存在，产品代码不调用。
//
// 它复刻 Go 的 mode 映射里唯一会点亮 ModeSymlink 的那一条
// （tag == IO_REPARSE_TAG_SYMLINK），也就是 path.go 原先
// `fi.Mode()&os.ModeSymlink != 0` 在 Windows 上的等价物。
//
// 留着它是为了让「这个缺口真实存在」这件事**可证伪**：
// TestWinRedirectClosesLegacyGap 会断言 legacy 判据对 junction 返回 false
// （= 放行 = 逃逸），而 winIsRedirect 返回 true。没有这个反向对照，
// 「修好了」就只是一句自述。
const winTagSymlink uint32 = 0xA000000C // IO_REPARSE_TAG_SYMLINK

func winLegacyIsRedirect(attrs, tag uint32) bool {
	if attrs&winFileAttributeReparsePoint == 0 {
		return false
	}
	return tag == winTagSymlink
}

// winPathContains 判断 final path `p` 是否落在共享根 `root` 之内（含根本身）。
//
// # 两边都必须是 GetFinalPathNameByHandleW 的输出
//
// 调用方要保证 root 和 p 来自**同一个 API、同一组 flags**
// （FILE_NAME_NORMALIZED | VOLUME_NAME_DOS）。这不是随口的约定，而是这个函数
// 能写得这么短的前提：`\\?\` 前缀、`\\?\UNC\` 形式、卷 GUID、8.3 短名、
// 大小写规范化——全部由内核在两边一致地处理掉了。
// 我们自己去规范化字符串只会引入分叉。
//
// # 为什么是精确比较而不是大小写不敏感比较
//
// 直觉会想「NTFS 大小写不敏感，所以要 EqualFold」。这里**刻意不这么做**：
//
//   - FILE_NAME_NORMALIZED 返回的是磁盘上的规范大小写，两边同源，正常情况下
//     本来就逐字节相等，折叠没有收益；
//   - Win10 1803+ 支持按目录打开大小写敏感标志。此时 `C:\share` 与 `C:\SHARE`
//     可以是**两个不同的目录**。折叠比较会把 `\\?\C:\SHARE\x` 判成落在
//     `\\?\C:\share` 之内 —— 这是**假接受**，正是我们要防的逃逸方向。
//
// 精确比较的失败方向是「误拒」（安全），折叠比较的失败方向是「误放」（不安全）。
// 安全判定一律选前者。
//
// # 边界
//
// 前缀比较必须卡在分隔符上，否则 `C:\share` 会把 `C:\shareEvil` 也算进来。
// root 末尾的分隔符先剥掉再统一补一个，这样共享根是盘符根（`\\?\C:\`）时
// 也不会拼出 `\\?\C:\\`。
func winPathContains(root, p string) bool {
	root = strings.TrimRight(root, `\`)
	if root == "" || p == "" {
		// 空根不是「匹配一切」，是「配置有问题」。失败方向选拒绝。
		return false
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+`\`)
}
