package oscap

// ports.go —— 六项能力的接口定义（**跨模块共享契约**）。
//
// 修改任何一个方法签名前必须通知 oscap-native / oscap-builtin / vfs
// 三方（AGENTS.md §7.3）—— 这里每改一个字，下游两套实现都要跟着改。
//
// 命名约定：**六个接口的方法名两两不重名**。这不是洁癖 ——
// 重名（比如都叫 Get/Set）会让「一个类型同时实现多项能力」在语言层面
// 变成不可能，而 builtin adapter 恰恰会用一个共享旁路存储的结构体一次
// 实现好几项。留出这个自由度，代价只是方法名长一点。

import (
	"io"
	"time"
)

// Xattr 是扩展属性能力。
//
// 名字语义：这里的 name 是**SMB/上层看到的名字**，不带平台命名空间前缀。
// Linux 的 `user.` 前缀由 native adapter 内部加减，且必须**双向对称** ——
// ListXattr 报出来的名字，客户端拿去 GetXattr 必须能取到。
// 不对称是这一层最典型的 bug 形态，实现方请务必有对应用例。
type Xattr interface {
	// GetXattr 读取属性值。属性不存在返回 ErrNotFound；
	// 文件系统不支持扩展属性返回 ErrNotSupported。
	//
	// 值可以为空（零长度）——「存在但为空」与「不存在」是两回事，
	// 前者返回 ([]byte{}, nil)。
	GetXattr(ref Ref, name string) ([]byte, error)

	// SetXattr 写入属性值，已存在则覆盖。
	SetXattr(ref Ref, name string, value []byte) error

	// RemoveXattr 删除属性。属性不存在返回 ErrNotFound。
	RemoveXattr(ref Ref, name string) error

	// ListXattr 列出全部属性名。
	//
	// 对象存在但一个属性都没有时返回 (nil, nil) —— 空结果是合法答案，不是错误。
	ListXattr(ref Ref) ([]string, error)
}

// Range 是文件内的一段区间 [Offset, Offset+Length)。
type Range struct {
	Offset int64
	Length int64
}

// SparseFile 是稀疏文件能力。
//
// 为什么它对本项目是刚需：Time Machine 的 .sparsebundle 由大量固定大小的
// band 文件组成，备份过期回收时会把 band 里的区间置零。没有打洞能力，
// 备份卷只会越来越大、永远不会缩小（AGENTS.md §2 阶段二）。
//
// 对应 SMB2 IOCTL 的 FSCTL_SET_ZERO_DATA / FSCTL_SET_SPARSE /
// FSCTL_QUERY_ALLOCATED_RANGES。
type SparseFile interface {
	// PunchHole 把 [off, off+length) 释放成空洞，读回来是零，文件逻辑长度不变。
	//
	// builtin 实现允许「写零 + 在旁路记账」来达到同样的**可观测语义**
	// （读回来是零、AllocatedRanges 不报这段），即便磁盘占用并没有真的下降。
	PunchHole(ref Ref, off, length int64) error

	// Preallocate 预留 [off, off+length) 的空间但**不改变**文件逻辑长度
	// （对应 AllocationSize / AlSi create context）。
	Preallocate(ref Ref, off, length int64) error

	// AllocatedRanges 返回 [off, off+length) 内实际已分配（非空洞）的子区间。
	//
	// 保证：按 Offset 升序、互不重叠、已裁剪到查询窗口且不越过 EOF。
	// 窗口内全是空洞或完全落在 EOF 之外时返回 (nil, nil) —— 空结果是合法答案。
	//
	// 探测不可用时**必须**降级为「整个窗口都已分配」而不是报错：
	// 多报已分配是安全的（客户端最多多读一遍零），少报会让客户端以为数据丢了。
	AllocatedRanges(ref Ref, off, length int64) ([]Range, error)

	// SetSparse 标记/取消稀疏（FSCTL_SET_SPARSE，MS-FSCC §2.3.69）。
	//
	// POSIX 语义下这个操作是**不对称**的，且这个不对称是刻意的：
	//   v=true  → nil（文件天然可稀疏，客户端要的效果已经成立）
	//   v=false → ErrNotSupported（做不到，且不能假装做到：SPARSE 属性位是
	//             由 Alloc < Size 现算的，谎称成功会让客户端看到自相矛盾的视图）
	// NTFS 上两个方向都真实生效。
	SetSparse(ref Ref, v bool) error
}

// StreamInfo 描述一个命名流。
type StreamInfo struct {
	// Name 是**裸流名**，不含 SMB 线格式的冒号与 `$DATA` 后缀
	// （例如 "AFP_Resource"）。主数据流用空串表示。
	//
	// 为什么不在这一层用 `:AFP_Resource:$DATA`：那是 wire 层的表示法，
	// 拼装它是 smb 层的职责；oscap 只谈「这个对象上有哪些附加数据流」。
	Name  string
	Size  int64
	Alloc int64
}

// StreamFlags 是打开命名流的意图。
type StreamFlags uint32

const (
	// StreamRead 需要读。
	StreamRead StreamFlags = 1 << iota
	// StreamWrite 需要写。
	StreamWrite
	// StreamCreate 不存在则创建。
	StreamCreate
	// StreamTruncate 打开后截断为 0。
	StreamTruncate
)

// StreamHandle 是一个已打开的命名流。
//
// 只有字节区间的读写与长度操作 —— 命名流没有目录、没有属性、没有子流。
type StreamHandle interface {
	io.ReaderAt
	io.WriterAt
	io.Closer

	// Truncate 设置流长度。
	Truncate(size int64) error
	// Size 返回当前流长度。
	Size() (int64, error)
}

// NamedStream 是命名流（alternate data stream）能力。
//
// 平台现实：Windows/NTFS 原生支持；POSIX 上 native adapter 用扩展属性承载
// （Samba vfs_fruit 与 Netatalk 的做法），因此 POSIX 的 CapNamedStream
// 事实上**建立在 CapXattr 之上** —— 没有 xattr 就没有 native 命名流。
// builtin 则用旁路文件承载，不依赖任何可选能力。
type NamedStream interface {
	// ListStreams 列出对象上的全部命名流，**不含**主数据流。
	// 一个都没有时返回 (nil, nil)。
	ListStreams(ref Ref) ([]StreamInfo, error)

	// OpenStream 打开一个命名流。
	//
	// name 是裸流名，不得为空（空串是主数据流，走普通文件 IO，不归本能力管），
	// 为空返回 ErrInvalidArg。
	// 流不存在且未带 StreamCreate 时返回 ErrNotFound。
	OpenStream(ref Ref, name string, flags StreamFlags) (StreamHandle, error)

	// RemoveStream 删除一个命名流。不存在返回 ErrNotFound。
	RemoveStream(ref Ref, name string) error
}

// StableFileID 是「卷内稳定唯一 ID」能力，对应 SMB 的
// FileInternalInformation 与 AAPL 的 QFid。
type StableFileID interface {
	// FileID 返回对象的稳定 ID。
	//
	// 契约（两条都是硬要求，客户端缓存正确性依赖它们）：
	//   - 同一对象多次查询返回**相同**值，且**跨重命名**保持不变；
	//   - 同一卷内不同对象的值**不得**相同。
	//
	// 拿不到稳定值时返回 ErrNotSupported，**不要**返回一个「差不多的」值 ——
	// 一个会变的 FileID 比没有 FileID 更糟：macOS 会据此认为文件被换掉了。
	FileID(ref Ref) (uint64, error)
}

// CreationTime 是「真实创建时间」能力。
//
// 注意这不是 POSIX 的 ctime（inode 变更时间）。POSIX 传统上根本没有创建时间，
// Linux 要 statx(STATX_BTIME) 且**文件系统得真的存了**它，
// macOS 有 st_birthtimespec，Windows 一直都有。
type CreationTime interface {
	// CreationTime 返回创建时间。
	//
	// 拿不到真实创建时间时返回 ErrNotSupported，而不是拿 mtime 冒充 ——
	// 冒充会让上层永远不知道该去问 builtin 要那个记下来的真值。
	CreationTime(ref Ref) (time.Time, error)

	// SetCreationTime 设置创建时间（对应 SET_INFO 的 FileBasicInformation）。
	SetCreationTime(ref Ref, t time.Time) error
}

// DOSAttributes 是 DOS 属性位能力（FILE_ATTRIBUTE_*，MS-FSCC §2.6）。
type DOSAttributes interface {
	// DOSAttributes 返回**存储下来的** DOS 属性位。
	//
	// 边界说明：由文件系统客观事实推导出来的位（DIRECTORY / SPARSE /
	// REPARSE_POINT），以及 POSIX 约定合成的位（点开头 → HIDDEN、
	// 属主无写权限 → READONLY），都由 vfs 层负责合成，**不归本能力管**。
	// 本能力只回答「有没有人显式设置过这些位、设的是什么」。
	//
	// 从来没有被设置过时返回 (0, ErrNotFound)，让上层走它的合成逻辑。
	DOSAttributes(ref Ref) (uint32, error)

	// SetDOSAttributes 设置 DOS 属性位。
	//
	// 只处理客户端可设置的那些位；DIRECTORY / SPARSE 一类客观事实位由调用方
	// 在传入前剔除（vfs 层的 settableDOSAttributes）。
	SetDOSAttributes(ref Ref, attrs uint32) error
}
