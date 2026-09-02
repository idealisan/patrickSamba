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

	// Durable 非 nil 表示本句柄被授予了 durable 能力（tm-handle 的持久句柄）。
	// 字段只占一行，与 tm-lease 的 Lease 指针互不干扰，合并零冲突。
	Durable *DurableState

	// Path 是相对共享根的路径（'/' 分隔，不以 '/' 开头，空串表示根）。
	Path string
	// Stream 是 alternate data stream 名，空串表示主数据流。
	Stream string

	// Handle 是 VFS 句柄。仅请求属性（vfs.OpenAttrOnly）时也会有句柄，
	// 由 VFS 后端决定是否真的持有 fd。
	// IPC$ 上的管道句柄没有 VFS 后端，此处为 nil。
	Handle vfs.Handle

	// Pipe 是 IPC$ 上的命名管道实例，非管道句柄为 nil。
	Pipe Pipe

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

	// oplockFileID 是本句柄被授予 oplock/lease 时用来定位对象的**文件身份**
	// （见 oplock_state.go 的 oplockKey）。
	//
	// 单独存一份而不是用 Path：句柄改名后 Path 会变，而"谁持有这个文件的
	// 缓存许可"不该随之改变（否则改名一次就会让 oplock 找不回来，
	// 那个文件的缓存许可永久泄漏）。只在授予时写一次，之后只读。
	oplockFileID uint64

	// shareModeKey / shareModeOn 记录本句柄在共享模式表里的登记位置
	// （见 share_access.go）。只在 shareModeTable.add 里写一次，
	// 此时句柄尚未进会话表、对其它 goroutine 不可见，之后只读。
	shareModeKey shareModeKey
	shareModeOn  bool

	mu sync.Mutex

	// deleteOnClose 表示关闭时删除目标（FILE_DELETE_ON_CLOSE 或
	// SET_INFO(FileDispositionInformation)）。
	deleteOnClose bool
	// vfsOwnsDelete 为 true 时，底层的 vfs 句柄会在 Close 时自己删除文件
	// （FILE_DELETE_ON_CLOSE 创建选项会走这条路，见 create.go），命令层
	// 的 CLOSE handler 就**不能再删一次**，否则会与 vfs 重复删除，
	// 报出 "delete-on-close 删除失败 ... vfs: not found"。
	vfsOwnsDelete bool

	// dirPattern 是 QUERY_DIRECTORY 的当前枚举通配符。
	// SMB2 只在第一次（或带 SMB2_RESTART_SCANS 时）携带模式，
	// 后续调用沿用它（MS-SMB2 §3.3.5.18）。
	dirPattern string
	// dirStarted 表示已经开始过一轮枚举，用于区分
	// STATUS_NO_SUCH_FILE（首次无匹配）与 STATUS_NO_MORE_FILES（枚举完毕）。
	dirStarted bool

	// dirPending 是上一轮从 VFS 取出但没能塞进响应缓冲的目录项。
	// VFS 的枚举游标已经走过它们了，所以必须由这里保管到下一轮，
	// 否则客户端会**丢文件**。
	dirPending []vfs.DirEntry

	// pipeOut 是管道上待客户端 READ 取走的响应字节。
	// 客户端用 WRITE+READ 两步做 DCERPC 调用时，WRITE 阶段产生的响应
	// 先存在这里，READ 阶段分批吐出。
	pipeOut []byte

	closed bool
}

// IsPipe 报告本句柄是否为 IPC$ 上的命名管道。
func (o *Open) IsPipe() bool { return o.Pipe != nil }

// PipeTransact 在管道上做一次请求/响应交换，并把响应存入待读缓冲。
//
// 返回的是**本次可以立即回给客户端**的字节数上限内的响应；
// 调用方（WRITE handler）通常不直接使用它，而是让后续 READ 取走。
func (o *Open) PipeTransact(in []byte, maxOut int) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	out, err := o.Pipe.Transact(in, maxOut)
	// ErrPipeMoreData 时 out 仍是有效的截断前缀，要一并缓存。
	if out != nil {
		o.pipeOut = append(o.pipeOut, out...)
	}
	return err
}

// PipeRead 从待读缓冲取走至多 max 字节。返回的切片是缓冲的副本。
func (o *Open) PipeRead(max int) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()

	if len(o.pipeOut) == 0 {
		return nil
	}
	n := min(max, len(o.pipeOut))
	out := make([]byte, n)
	copy(out, o.pipeOut[:n])
	o.pipeOut = o.pipeOut[n:]
	return out
}

// PipePending 返回待读缓冲中剩余的字节数。
func (o *Open) PipePending() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pipeOut)
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
		o.dirPending = nil
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

// ReadDir 取下一批目录项，优先消费上一轮遗留的条目。
//
// max <= 0 表示不限条数。返回空切片表示枚举结束。
func (o *Open) ReadDir(pattern string, restart bool, max int) ([]vfs.DirEntry, error) {
	o.mu.Lock()
	if len(o.dirPending) > 0 {
		out := o.dirPending
		if max > 0 && len(out) > max {
			out, o.dirPending = out[:max], out[max:]
		} else {
			o.dirPending = nil
		}
		o.mu.Unlock()
		return out, nil
	}
	h := o.Handle
	o.mu.Unlock()

	if h == nil {
		return nil, vfs.ErrClosed
	}
	return h.ReadDir(pattern, restart, max)
}

// ResetDirScan 复位目录枚举状态，使下一次 QUERY_DIRECTORY 从头开始。
func (o *Open) ResetDirScan() {
	o.mu.Lock()
	o.dirStarted = false
	o.dirPending = nil
	o.mu.Unlock()
}

// UnreadDir 把没能写进本次响应的目录项退回，下一轮 QUERY_DIRECTORY 先吐它们。
func (o *Open) UnreadDir(entries []vfs.DirEntry) {
	if len(entries) == 0 {
		return
	}
	keep := make([]vfs.DirEntry, len(entries))
	copy(keep, entries)

	o.mu.Lock()
	// 退回的条目排在已有 pending 之前 —— 它们在枚举顺序上更靠前。
	o.dirPending = append(keep, o.dirPending...)
	o.mu.Unlock()
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
	// 显式关闭一个 durable 句柄时，把它从「等待重连」表摘除，
	// 否则它会一直占着登记表直到超时。Durable 为 nil 或未授予时是无害 no-op。
	if o.Durable != nil && o.Durable.Granted {
		durableRegistry.remove(o)
	}
	h := o.Handle
	p := o.Pipe
	o.Handle = nil
	o.Pipe = nil
	o.pipeOut = nil
	o.mu.Unlock()

	// 摘除共享模式登记。这里是所有句柄消失路径的唯一汇合处
	// （CLOSE / 连接断开 / TREE_DISCONNECT / LOGOFF / CREATE 回滚），
	// 放在别处一定会漏掉某一条，那个文件就被永久锁死了。
	//
	// 必须在 o.mu 之外调用：判定路径是「表锁 → 读快照」，这里若反过来
	// 持 o.mu 再取表锁，就凑齐了一个加锁顺序反转。
	if o.shareModeOn && o.Tree != nil && o.Tree.Share != nil {
		o.Tree.Share.shareModes.remove(o)
	}

	// 释放该句柄持有的全部字节范围锁（MS-SMB2 §3.3.5.14 / §3.3.5.10：
	// 句柄消失即释放）。与 shareModes 同理，这里是全部非 CLOSE 关闭路径的
	// 唯一汇合处 —— 此前释放只挂在 CLOSE 命令里，TREE_DISCONNECT /
	// LOGOFF / 断连 / durable 过期回收全都不碰锁表，客户端异常退出后锁会
	// 泄漏到进程重启（bh4-A#2）。Samba 对照：所有关闭路径汇于
	// close_file → brl_close_fnum（source3/smbd/close.c:503）。
	//
	// durable 语义不受影响：等待重连的句柄不走本方法（disconnect 返回 true
	// 时调用方跳过 close），锁随句柄保留；过期回收与显式 CLOSE 才走到这里，
	// 锁随之释放；重连认领回的是同一个 *Open，按指针记账的锁继续有效。
	//
	// 与 CLOSE handler（close.go）里的显式 releaseAll 构成**双保险**：
	// releaseAll 幂等，两处都调无害；close.go 那处刻意保持原样不动。
	// 必须在 o.mu 之外调用（releaseAll 自带表锁，理由同上）。
	//
	// o.Tree 的读取走 o.mu 快照：durable 重连（rebindOpen）会在持 s.mu 时
	// 并发改绑 o.Tree，裸读是 CI race 门禁抓到的数据竞争。
	o.mu.Lock()
	tree := o.Tree
	o.mu.Unlock()
	if tree != nil && tree.Share != nil {
		tree.Share.locks.releaseAll(o.Path, o)
		// 句柄关闭即收回它的 oplock/lease 缓存许可（MS-SMB2 §3.3.5.10）。
		// 不收回的话，别的对象复用同一 fileID 时会继承一份"有人在缓存"
		// 的假象，后续打开会去做一次永远等不到确认的 break。
		tree.Share.oplocks.release(o)
		// 目录句柄关闭：其上的未决 CHANGE_NOTIFY 回 STATUS_NOTIFY_CLEANUP
		// （MS-SMB2 §3.3.5.19）。不处理的话那些请求会一直挂到客户端超时。
		// 与 shareModes 同理放在这里 —— 这是全部句柄消失路径的唯一汇合处。
		tree.Share.notify.cleanup(o)
	}

	if h != nil {
		_ = h.Close()
	}
	if p != nil {
		_ = p.Close()
	}
}

// Closed 报告句柄是否已释放。
func (o *Open) Closed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}
