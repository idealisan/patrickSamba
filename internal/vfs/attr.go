package vfs

// attr.go —— 宿主文件系统属性 → SMB/Windows 属性的映射（跨平台通用部分）。
//
// 平台相关的取值（uid/gid/nlink/inode/创建时间/分配长度）在
// attr_linux.go / attr_darwin.go / attr_windows.go / attr_other.go 里，
// 通过 fillSysAttr 注入，本文件不含任何 build tag。

import (
	"io/fs"
	"os"
	"strings"
	"time"
)

// FILE_ATTRIBUTE_* 位图（MS-FSCC §2.6 File Attributes，
// 摘录见 docs/protocol-notes.md §11）。
//
// 这里只定义 VFS 层会产生或消费的位；wire 层另有自己的完整定义，
// 两边是**平行**的常量而不是互相 import —— vfs 位于 smb 层之下，
// 反向依赖会破坏 AGENTS.md §5 的分层。
const (
	FileAttributeReadonly  uint32 = 0x00000001
	FileAttributeHidden    uint32 = 0x00000002
	FileAttributeSystem    uint32 = 0x00000004
	FileAttributeDirectory uint32 = 0x00000010
	FileAttributeArchive   uint32 = 0x00000020
	FileAttributeNormal    uint32 = 0x00000080
	FileAttributeTemporary uint32 = 0x00000100
	FileAttributeSparse    uint32 = 0x00000200
	FileAttributeReparse   uint32 = 0x00000400
	FileAttributeCompress  uint32 = 0x00000800
	FileAttributeOffline   uint32 = 0x00001000
	FileAttributeNotIndex  uint32 = 0x00002000
	FileAttributeEncrypted uint32 = 0x00004000
)

// settableDOSAttributes 是允许客户端通过 SET_INFO 修改的位。
// DIRECTORY / SPARSE / REPARSE 等是文件系统的客观事实，不接受设置。
const settableDOSAttributes = FileAttributeReadonly | FileAttributeHidden |
	FileAttributeSystem | FileAttributeArchive | FileAttributeTemporary |
	FileAttributeOffline | FileAttributeNotIndex

// attrFromFileInfo 把 fs.FileInfo 转成 Attr。
//
// name 用于 POSIX → DOS 的隐藏属性映射（点开头的文件视为 HIDDEN），
// 必须是**文件名分量**而不是整条路径，否则 "./a" 这种会被误判。
// readOnlyShare 为 true 时无条件加上 READONLY 位。
func attrFromFileInfo(fi fs.FileInfo, name string, readOnlyShare bool) *Attr {
	a := &Attr{
		Size:  fi.Size(),
		Alloc: allocSizeFallback(fi.Size()),
		Mode:  uint32(fi.Mode().Perm()),
		NLink: 1,
	}
	// POSIX 只有 mtime/atime/ctime，没有真正的创建时间。
	// 先把四个时间都填成 mtime 做兜底，随后由 fillSysAttr 用平台真值覆盖。
	mt := fi.ModTime()
	a.CreateTime = mt
	a.AccessTime = mt
	a.WriteTime = mt
	a.ChangeTime = mt

	fillSysAttr(fi, a)

	a.FileAttributes = dosAttributes(fi, a, name, readOnlyShare)
	return a
}

// hiddenByName 报告一个文件名分量是否命中 Unix 隐藏约定（点开头）。
// "." / ".." 不算 —— 它们是目录游标，不是真名。
//
// dosAttributes（无记录时的推导）与 mergeStoredDOS（有记录时的存储值
// 优先合并）都必须走同一个判定，保证 hide_dot_files 行为在两条路径上一致
// （Samba 对照：dos_mode_from_name，source3/smbd/dosmode.c:594–608）。
func hiddenByName(name string) bool {
	return strings.HasPrefix(name, ".") && name != "." && name != ".."
}

// calcBtimeFallback 是拿不到内核 btime 时创建时间兜底值的统一口径：
// **MIN(ctime, mtime, atime)**，atime 为零值（异常）时退 MIN(ctime, mtime)。
//
// 为什么不用 ctime 单值：Samba 的 calc_create_time_stat
// （source3/lib/system.c:131–150）就是取三者最小 —— ctime 会随任何 inode
// 变更（chmod/链接数）跳动，保时拷贝（rsync/cp -p）的文件 atime/mtime
// 往往远早于 ctime，取最小值更接近真实创建时刻。
func calcBtimeFallback(ctime, mtime, atime time.Time) time.Time {
	out := ctime
	if mtime.Before(out) {
		out = mtime
	}
	if !atime.IsZero() && atime.Before(out) {
		out = atime
	}
	return out
}

// dosAttributes 计算 FILE_ATTRIBUTE_* 位图。
//
// POSIX 映射规则（docs/protocol-notes.md §11）：
//   - 目录 → DIRECTORY
//   - 点开头 → HIDDEN（Unix 的隐藏约定，Samba 的 `hide dot files` 亦然）
//   - 属主无写权限 → READONLY
//   - 分配长度小于逻辑长度 → SPARSE_FILE（Time Machine 的 sparsebundle 需要）
//   - 符号链接 → REPARSE_POINT
//   - **结果不能为 0**：普通文件兜底给 ARCHIVE
//     （返回 0 会让部分 Windows 客户端认为属性无效）
//
// 注意：这里的结果只是「无存储记录时的合成基线」。调用方随后会经
// mergeStoredDOS 用旁路库里的存储值覆盖可设置位（bh3-F5 存储值优先）。
func dosAttributes(fi fs.FileInfo, a *Attr, name string, readOnlyShare bool) uint32 {
	// Windows 宿主上 fillSysAttr 已经填好了**原生** DOS 属性位
	// （NTFS 真的存了 HIDDEN/SYSTEM/ARCHIVE），必须保留而不是覆盖；
	// POSIX 宿主上这里是 0，完全由下面的规则合成。
	out := a.FileAttributes
	mode := fi.Mode()

	switch {
	case mode.IsDir():
		out |= FileAttributeDirectory
	case mode&os.ModeSymlink != 0:
		out |= FileAttributeReparse
	}

	if hiddenByName(name) {
		out |= FileAttributeHidden
	}

	// 0o200 = S_IWUSR。只看属主写位：授权由配置决定（AGENTS.md §1.1），
	// 这里只是把「宿主机上不可写」如实反映给客户端，不是访问控制。
	if readOnlyShare || mode.Perm()&0o200 == 0 {
		out |= FileAttributeReadonly
	}

	if !mode.IsDir() && a.Alloc > 0 && a.Alloc < a.Size {
		out |= FileAttributeSparse
	}

	if out == 0 {
		out = FileAttributeArchive
	}
	return out
}

// allocSizeFallback 在拿不到真实 st_blocks 时按 512 字节向上取整估算分配长度。
// 512 是 POSIX st_blocks 的单位，也是最保守的假设。
func allocSizeFallback(size int64) int64 {
	if size <= 0 {
		return 0
	}
	const unit = 512
	return (size + unit - 1) / unit * unit
}
