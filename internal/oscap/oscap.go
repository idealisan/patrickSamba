// Package oscap 定义**操作系统可选能力**的抽象 port（AGENTS.md §1.2 C9 / §5 P7）。
//
// # 这个包为什么存在
//
// C9 规定操作系统只提供两样东西：(a) 一块**普通的**可读写文件系统；
// (b) 网络套接字。而 SMB 语义需要一批「普通文件系统不一定表达得了」的元数据 ——
// 扩展属性、稀疏区间、命名流（ADS）、稳定 FileID、真实创建时间、DOS 属性位。
// 这些能力**有就用，没有就自己补**：
//
//   - internal/oscap          —— 本包，只描述「我需要什么语义」（port）。
//   - internal/oscap/native   —— 借助 OS 能力实现（xattr / FALLOC_FL_PUNCH_HOLE /
//     NTFS ADS / statx STATX_BTIME …）。快、省，且与宿主上的本地工具看到的
//     是同一份东西。
//   - internal/oscap/builtin  —— 只用「普通文件 + 一份旁路存储」把**同一份语义**
//     做出来。慢一些，但任何能跑 Go 的地方都能跑。
//
// # 两条铁律（AGENTS.md §1.2）
//
//  1. **逐能力矩阵降级，不是整体二选一。** 真实场景本来就是混合的：ext4 有 xattr
//     但拿不到可靠的创建时间，于是命名流走 native、创建时间走 builtin。
//     所谓「窄档」只是所有项都指向 builtin 的极限情况，不是一个单独的实现分支 ——
//     本包里没有、也不许出现 `if 窄平台 { 走另一套代码 }` 这种结构。
//  2. **builtin 版必须完整。** 每一项能力都必须有 builtin 实现。缺一项就等于
//     某个未知平台整个不可用，而且往往到现场才发现。New 会在组装时**硬失败**
//     （IncompleteBuiltinError），不给「先欠着」留口子。
//
// # 分层位置
//
// 本包在 internal/vfs **之下**，因此**不 import vfs**（AGENTS.md §5 依赖方向）。
// 错误也自成一套 sentinel，由 vfs 层负责映射成 vfs.Err*，再由 smb 层映射成
// NTSTATUS。别在这里引 vfs 的错误值，那会立刻造成反向依赖。
//
// # 路径契约（重要）
//
// 所有 Ref.Path 都是**宿主机路径**（平台原生分隔符），且**已经过 vfs 层的
// 根目录约束校验**（AGENTS.md §8）。本包与两个 adapter **不再重复做路径穿越
// 校验**，也**不得**把 Ref.Path 当成可信的用户输入去做别的事情（例如据此
// 拼接旁路文件名时仍要自己规范化）。这条边界写在这里，是为了让实现者清楚
// 「安全校验在哪一层做完了」，而不是各写一遍或者各自以为对方做了。
package oscap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Ref 指向一个待操作的对象（文件或目录）。
//
// 为什么同时带路径与句柄：native adapter 用 fd 语义最快也最安全
// （fgetxattr / fallocate 都要 fd），而 builtin adapter 需要**路径**来给
// 旁路存储做 key（rename/delete 之后句柄背后的对象可能已经不在了）。
// 两者都塞进来，各取所需，避免为同一件事定义两套方法。
type Ref struct {
	// Path 是宿主机路径，平台原生分隔符，必填。
	//
	// 保证位于 Options.Root 之下（vfs 层已做过越界校验，见包注释「路径契约」）。
	Path string

	// Handle 是该对象**已打开**的句柄，可以为 nil。
	//
	// 实现方规则：有 Handle 时**可以**走 fd 版系统调用；为 nil 时**必须**
	// 退回按路径操作，不许直接返回 ErrNotSupported —— SMB 的 OpenAttrOnly
	// 句柄背后就没有 *os.File，那条路径是常态而不是异常。
	Handle *os.File
}

// Options 是构造一套能力实现所需的上下文。
type Options struct {
	// Root 是共享根的宿主机绝对路径。探测与旁路存储都以它为基准。
	Root string

	// MetadataPath 是 builtin 旁路存储的落盘路径，对应配置里的
	// shares[].metadata_path。留空时由 builtin adapter 自行决定默认落点
	// （应当放在共享目录**之外**，否则客户端会在共享里看到这个数据库文件）。
	MetadataPath string

	// InstanceID 是**本服务实例**的稳定标识，用于让默认元数据落点在
	// 多个共享同一共享目录的 stupidsamba 进程之间互不冲突。
	//
	// 为什么需要：bbolt 用 flock 做进程间互斥，两个进程若算出同一个
	// 默认库文件，第二个会卡满 flock 超时（5s）后启动失败。
	// 监听地址+端口在每个实例上不同，拿它做 InstanceID 既能区分实例、
	// 又在同一实例重启后保持稳定（复用同一份元数据）。
	//
	// 留空表示「单实例」语义：默认落点退回历史文件名，与旧版本行为一致
	// （测试、库直接调用等不经由服务装配层的场景都走这条）。
	// 显式配置的 MetadataPath 优先于 InstanceID：用户一旦手写了落点，
	// 实例区分交给用户自己负责。
	InstanceID string

	// ReadOnly 表示本共享只读。
	//
	// 对 adapter 的含义：不得创建/写入任何旁路文件。此时写类方法一律返回
	// ErrReadOnly，而不是「假装成功」——「成功回显 ≠ 事情真的发生了」是本项目
	// 反复栽过的坑。
	ReadOnly bool
}

// Validate 校验 Options 自身的完整性。
func (o Options) Validate() error {
	if o.Root == "" {
		return fmt.Errorf("oscap: Options.Root 不能为空: %w", ErrInvalidArg)
	}
	return nil
}

// 跨 adapter 通用错误。vfs 层负责把它们映射成 vfs.Err*（本包不反向依赖 vfs）。
var (
	// ErrNotSupported 表示这一项在当前 adapter 上做不到。
	//
	// 注意与「查不到」的区别：某个对象没有 FinderInfo 是 ErrNotFound，
	// 整个文件系统不支持扩展属性才是 ErrNotSupported。
	ErrNotSupported = errors.New("oscap: capability not supported")

	// ErrNotFound 表示对象/属性/流不存在。
	ErrNotFound = errors.New("oscap: not found")

	// ErrExist 表示目标已存在（创建类操作）。
	ErrExist = errors.New("oscap: already exists")

	// ErrReadOnly 表示共享或宿主文件系统只读。
	ErrReadOnly = errors.New("oscap: read-only")

	// ErrClosed 表示句柄已关闭。
	ErrClosed = errors.New("oscap: handle closed")

	// ErrInvalidArg 表示参数非法（负 offset/length、溢出、空名字等）。
	//
	// 刻意包装 fs.ErrInvalid：vfs.ErrInvalidArg 也是这么做的，
	// 这样跨层 errors.Is 判定不会在中间断掉（见 vfs/fs.go 该字段注释里
	// 记录的那次 STATUS_UNSUCCESSFUL 事故）。
	ErrInvalidArg = fmt.Errorf("oscap: %w", fs.ErrInvalid)
)
