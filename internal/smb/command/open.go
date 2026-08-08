package command

import (
	"sync"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// Open 是一个已打开的文件/目录句柄（MS-SMB2 §3.3.1.10 Open）。
//
// FileId 由 Persistent(8) + Volatile(8) 组成，线格式共 16 字节。
// 本服务端不支持 durable handle，两者取同一个会话内单调递增值。
type Open struct {
	Persistent uint64
	Volatile   uint64

	// Tree / Session 是所属树连接与会话。
	Tree    *Tree
	Session *Session

	// Path 是相对共享根的路径（'/' 分隔，不以 '/' 开头，空串表示根）。
	Path string
	// Stream 是 alternate data stream 名，空串表示主数据流。
	Stream string

	// Handle 是 VFS 句柄。仅请求属性（vfs.OpenAttrOnly）时也会有句柄，
	// 由 VFS 后端决定是否真的持有 fd。
	Handle vfs.Handle

	// IsDir 表示该句柄指向目录。
	IsDir bool

	// GrantedAccess 是最终授予的访问掩码（已展开 GENERIC_*）。
	GrantedAccess wire.Access
	// ShareAccess 是客户端声明的共享模式。
	ShareAccess wire.ShareAccess
	// CreateAction 是 CREATE 实际发生的动作，用于响应。
	CreateAction wire.CreateAction
	// CreateOptions 是客户端请求的 CreateOptions，CLOSE 时要看 DELETE_ON_CLOSE。
	CreateOptions wire.CreateOptions
	// FileAttributes 是打开时的文件属性快照。
	FileAttributes wire.FileAttributes

	mu sync.Mutex

	// deleteOnClose 表示关闭时删除目标（FILE_DELETE_ON_CLOSE 或
	// SET_INFO(FileDispositionInformation)）。
	deleteOnClose bool

	// dirPattern 是 QUERY_DIRECTORY 的当前枚举通配符。
	// SMB2 只在第一次（或带 SMB2_RESTART_SCANS 时）携带模式，
	// 后续调用沿用它（MS-SMB2 §3.3.5.18）。
	dirPattern string
	// dirStarted 表示已经开始过一轮枚举，用于区分
	// STATUS_NO_SUCH_FILE（首次无匹配）与 STATUS_NO_MORE_FILES（枚举完毕）。
	dirStarted bool

	closed bool
}

// SetDeleteOnClose 设置/清除关闭时删除标志。
func (o *Open) SetDeleteOnClose(v bool) {
	o.mu.Lock()
	o.deleteOnClose = v
	o.mu.Unlock()
}

// DeleteOnClose 报告是否设置了关闭时删除。
func (o *Open) DeleteOnClose() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.deleteOnClose
}

// DirScan 返回本次 QUERY_DIRECTORY 应使用的通配符，以及是否为首轮枚举。
//
// pattern 为空表示客户端未携带模式，沿用上一次的；restart 为 true 时重置。
func (o *Open) DirScan(pattern string, restart bool) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if restart {
		o.dirStarted = false
	}
	if pattern != "" {
		o.dirPattern = pattern
	}
	if o.dirPattern == "" {
		o.dirPattern = "*"
	}
	first := !o.dirStarted
	o.dirStarted = true
	return o.dirPattern, first
}

// close 释放底层 VFS 句柄。幂等。
//
// 注意：delete-on-close 的实际删除动作由 CLOSE handler 负责
// （它需要 Tree 的文件系统与只读判定），这里只关句柄。
func (o *Open) close() {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	h := o.Handle
	o.Handle = nil
	o.mu.Unlock()

	if h != nil {
		_ = h.Close()
	}
}

// Closed 报告句柄是否已释放。
func (o *Open) Closed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}
