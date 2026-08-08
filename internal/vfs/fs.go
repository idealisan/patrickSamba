// Package vfs 定义 stupidSamba 的可写虚拟文件系统抽象。
//
// 设计说明（见 AGENTS.md §5 P3）：
//
//   - Go 标准库的 io/fs 是**只读**的，无法满足 SMB 服务端需求。
//   - spf13/afero 是**路径语义**（每次操作都传路径），而 SMB 是严格的
//     **句柄语义**：CREATE 拿到 FileId → READ/WRITE/QUERY_INFO/SET_INFO
//     全部基于该句柄 → CLOSE。两者阻抗不匹配。
//   - 因此本项目自定义句柄式接口。
//
// 本文件是**跨模块共享契约**，修改前必须通知所有相关 agent（AGENTS.md §7.3）。
package vfs

import (
	"errors"
	"io"
	"time"
)

// Handle 是一个已打开对象（文件或目录）的不透明句柄。
//
// 句柄由 FileSystem.Open 创建，必须调用 Close 释放。
// 实现必须保证同一个 Handle 上的方法可以被并发调用而不损坏内部状态
// （SMB 客户端会在同一个 FileId 上并发下发 READ/WRITE）。
type Handle interface {
	io.Closer

	// ReadAt 从 off 处读取。语义同 io.ReaderAt：
	// 读到文件尾且读取字节数不足时返回 io.EOF。
	ReadAt(p []byte, off int64) (int, error)

	// WriteAt 在 off 处写入。语义同 io.WriterAt。
	WriteAt(p []byte, off int64) (int, error)

	// Truncate 将文件长度设置为 size（对应 FileEndOfFileInformation）。
	Truncate(size int64) error

	// Sync 将数据落盘。full 为 true 时要求**强制刷盘**
	// （对应 macOS 的 F_FULLFSYNC / SMB2 FLUSH，Time Machine 依赖此语义）。
	Sync(full bool) error

	// Stat 返回该句柄对应对象的属性。
	Stat() (*Attr, error)

	// SetAttr 修改属性。仅 mask 中置位的字段生效。
	SetAttr(attr *Attr, mask AttrMask) error

	// ReadDir 枚举目录项。
	//
	//   - 仅对目录句柄有效，对文件句柄返回 ErrNotDir。
	//   - 枚举状态保存在 Handle 内部（SMB2 QUERY_DIRECTORY 是分批拉取的）。
	//   - restart 为 true 时从头开始（对应 SMB2_RESTART_SCANS）。
	//   - pattern 为 SMB 通配符（"*"、"?"，空串等价于 "*"）；
	//     过滤由实现负责，以便后端可以下推优化。
	//   - max <= 0 表示不限制条数。
	//   - 枚举完毕返回空切片与 io.EOF。
	ReadDir(pattern string, restart bool, max int) ([]DirEntry, error)

	// Xattr 返回扩展属性访问器。不支持时返回 ErrNotSupported。
	// Apple 扩展与 alternate data stream 依赖它。
	Xattr() (XattrAccessor, error)
}

// XattrAccessor 提供扩展属性读写（用于 Apple 的 AFP_AfpInfo / AFP_Resource
// 以及 com.apple.* 元数据）。
type XattrAccessor interface {
	Get(name string) ([]byte, error)
	Set(name string, value []byte) error
	Remove(name string) error
	List() ([]string, error)
}

// DirEntry 是一条目录项。
type DirEntry struct {
	Name string
	Attr Attr
}

// Attr 是与 SMB/Windows 语义对齐的对象属性。
//
// 时间统一用 Go 的 time.Time；到 Windows FILETIME（1601 epoch, 100ns）的
// 转换由 wire 层负责，本层不关心线格式。
type Attr struct {
	// Size 是文件逻辑长度（EndOfFile）。
	Size int64
	// Alloc 是分配长度（AllocationSize）。稀疏文件时可小于 Size。
	Alloc int64

	CreateTime time.Time
	AccessTime time.Time
	WriteTime  time.Time
	ChangeTime time.Time

	// FileAttributes 是 Windows FILE_ATTRIBUTE_* 位图。
	// 由后端根据自身语义映射（例如 POSIX 下点开头文件映射为 HIDDEN）。
	FileAttributes uint32

	// Mode 是 POSIX 权限位（低 12 位）。非 POSIX 后端可填 0。
	Mode uint32
	UID  uint32
	GID  uint32

	// NLink 是硬链接数，用于目录项计数。未知填 1。
	NLink uint32

	// FileID 是卷内稳定唯一 ID（对应 FileInternalInformation / QFid）。
	// 通常用 inode。后端必须保证同一对象多次查询返回相同值。
	FileID uint64
}

// AttrMask 标记 SetAttr 中哪些字段生效。
type AttrMask uint32

const (
	AttrSize AttrMask = 1 << iota
	AttrAlloc
	AttrCreateTime
	AttrAccessTime
	AttrWriteTime
	AttrChangeTime
	AttrFileAttributes
	AttrMode
	AttrUID
	AttrGID
)

// OpenFlags 描述打开意图，由 SMB2 CREATE 的 DesiredAccess/CreateOptions 映射而来。
type OpenFlags uint32

const (
	// OpenRead 需要读数据。
	OpenRead OpenFlags = 1 << iota
	// OpenWrite 需要写数据。
	OpenWrite
	// OpenAppend 追加写。
	OpenAppend
	// OpenDirectory 要求目标必须是目录（FILE_DIRECTORY_FILE）。
	OpenDirectory
	// OpenNonDirectory 要求目标必须不是目录（FILE_NON_DIRECTORY_FILE）。
	OpenNonDirectory
	// OpenNoFollow 不跟随符号链接（FILE_OPEN_REPARSE_POINT）。
	OpenNoFollow
	// OpenWriteThrough 每次写入后落盘（FILE_WRITE_THROUGH）。
	OpenWriteThrough
	// OpenDeleteOnClose 关闭时删除（FILE_DELETE_ON_CLOSE）。
	OpenDeleteOnClose
	// OpenAttrOnly 仅需要属性，不需要真正打开数据流。
	// 用于优化 Explorer 的大量属性探测（AGENTS.md §5 P3）。
	OpenAttrOnly
)

// Disposition 对应 SMB2 CREATE 的 CreateDisposition（MS-SMB2 §2.2.13）。
type Disposition uint32

const (
	// Supersede 存在则删除重建，不存在则创建。
	Supersede Disposition = 0
	// OpenExisting 存在则打开，不存在则失败。
	OpenExisting Disposition = 1
	// CreateNew 存在则失败，不存在则创建。
	CreateNew Disposition = 2
	// OpenAlways 存在则打开，不存在则创建。
	OpenAlways Disposition = 3
	// TruncateExisting 存在则截断，不存在则失败。
	TruncateExisting Disposition = 4
	// TruncateAlways 存在则截断，不存在则创建。
	TruncateAlways Disposition = 5
)

// Action 是 Open 实际发生的动作，对应 SMB2 CREATE Response 的 CreateAction。
type Action uint32

const (
	ActionSuperseded  Action = 0
	ActionOpened      Action = 1
	ActionCreated     Action = 2
	ActionOverwritten Action = 3
)

// OpenRequest 是一次打开请求。
type OpenRequest struct {
	// Path 是**相对于共享根**的路径，使用 '/' 分隔，不以 '/' 开头。
	// 空串表示共享根目录本身。
	//
	// 调用方（SMB 层）负责把 SMB 的 '\' 转成 '/'；
	// 实现方（VFS 层）负责路径规范化与越界校验，这是**安全边界**。
	Path string

	// Stream 是 alternate data stream 名（Apple 扩展需要，如 "AFP_Resource"）。
	// 空串表示主数据流。
	Stream string

	Flags       OpenFlags
	Disposition Disposition

	// FileAttributes 是创建时要设置的 FILE_ATTRIBUTE_* 位。
	FileAttributes uint32
}

// FSInfo 是卷级信息，用于 SMB2 QUERY_INFO 的 FILE_FS_* 类。
type FSInfo struct {
	// VolumeLabel 是卷标。
	VolumeLabel string
	// VolumeSerial 是卷序列号。
	VolumeSerial uint32
	// BlockSize 是分配单元大小（字节）。
	BlockSize uint32
	// TotalBlocks / FreeBlocks / AvailBlocks 以 BlockSize 为单位。
	TotalBlocks uint64
	FreeBlocks  uint64
	AvailBlocks uint64
	// CaseSensitive 表示是否大小写敏感。
	CaseSensitive bool
	// MaxComponentLen 是单个路径分量的最大长度。
	MaxComponentLen uint32
}

// FileSystem 是共享目录的后端抽象。
//
// 所有方法中的 path 都是**相对于共享根**的 '/' 分隔路径，
// 实现必须在内部做规范化与越界校验（AGENTS.md §8）。
type FileSystem interface {
	// Open 打开或创建对象，返回句柄与实际发生的动作。
	Open(req *OpenRequest) (Handle, Action, error)

	// Stat 查询属性而不打开句柄（用于轻量属性探测）。
	Stat(path string) (*Attr, error)

	// Remove 删除文件或空目录。
	Remove(path string) error

	// Mkdir 创建目录。
	Mkdir(path string, attrs uint32) error

	// Rename 重命名/移动。replace 为 true 时允许覆盖已存在的目标
	// （对应 FileRenameInformation 的 ReplaceIfExists）。
	Rename(oldPath, newPath string, replace bool) error

	// StatFS 返回卷级信息。
	StatFS() (*FSInfo, error)

	// Streams 列出某个对象的 alternate data stream。
	// 不支持时返回 ErrNotSupported。
	Streams(path string) ([]StreamInfo, error)

	// ReadOnly 表示本共享是否只读。只读时所有写操作应返回 ErrReadOnly。
	ReadOnly() bool

	// Close 释放文件系统级资源。
	Close() error
}

// StreamInfo 描述一个 alternate data stream。
type StreamInfo struct {
	// Name 是流名，主数据流为 "::$DATA"，其余形如 ":AFP_Resource:$DATA"。
	Name  string
	Size  int64
	Alloc int64
}

// 跨后端通用错误。SMB 层负责把它们映射为 NTSTATUS。
var (
	ErrNotFound     = errors.New("vfs: not found")
	ErrExist        = errors.New("vfs: already exists")
	ErrNotDir       = errors.New("vfs: not a directory")
	ErrIsDir        = errors.New("vfs: is a directory")
	ErrNotEmpty     = errors.New("vfs: directory not empty")
	ErrPermission   = errors.New("vfs: permission denied")
	ErrReadOnly     = errors.New("vfs: read-only file system")
	ErrInvalidPath  = errors.New("vfs: invalid path")
	ErrNotSupported = errors.New("vfs: not supported")
	ErrNoSpace      = errors.New("vfs: no space left")
	ErrTooLarge     = errors.New("vfs: file too large")
	ErrClosed       = errors.New("vfs: handle closed")

	// ErrInvalidArg 表示**参数**非法（而不是路径非法）：
	// 负的 offset/length、offset+length 溢出、对目录做 Truncate 等。
	// SMB 层应映射为 STATUS_INVALID_PARAMETER (0xC000000D)。
	ErrInvalidArg = errors.New("vfs: invalid argument")
)
