// Package builtin 是 OS 能力抽象的**自实现适配器**（AGENTS.md §1.2 C9 / §5 P7）。
//
// # 它凭什么存在
//
// C9 规定操作系统只提供两样东西：一块**普通的**可读写文件系统，和网络套接字。
// 「普通」意味着不能假设它有扩展属性、稀疏文件、命名流、稳定 inode、创建时间
// 或 DOS 属性位 —— FAT32/exFAT 外置盘、`nouser_xattr` 挂载的分区、某些 NAS 导出、
// 只读根文件系统上这些统统没有。本包用「普通文件 + 一份旁路 KV」把 SMB 需要的
// **同一份语义**做出来，让服务在这些环境下照样完整可用。
//
// # 完整性是硬要求
//
// 六项能力**一项都不能缺**（AGENTS.md §1.2 铁律 2）。New 返回的 oscap.Set 六个
// 字段全部非 nil；缺任何一项，oscap.New 会用 IncompleteBuiltinError 硬失败。
// 理由是将来移植到未知平台时 builtin 是唯一底座，缺一项等于那个平台整个不可用，
// 而且往往到现场才发现。
//
// # 旁路存储用 bbolt
//
// `go.etcd.io/bbolt`：纯 Go、MIT、无 CGO、单文件、事务安全，满足 C1/C2/C6 与 §4。
// 本仓库在 internal/vfs/metadata_windows.go 已经用它承载 Windows 的 POSIX 元数据，
// 这里复用同一个依赖，不引入新的第三方。**禁止**换成 mattn/go-sqlite3 这类需要 CGO 的方案。
//
// 一个必须写明的边界：bbolt 内部用 mmap + flock。严格说这比 C9 允许的
// 「open/read/write/seek/stat/fsync」多要了一点东西。之所以判定可接受：
// 它们是**平台调用约定本身**（Go runtime 也在用），不是可以不依赖的外部服务或驱动，
// 与「依赖 avahi 守护进程」「依赖 cifs 内核驱动」性质不同（同 §1.2 对 kernel32 的论述）。
// 全部 KV 访问都收敛在 store.go 一个文件里，将来若真遇到连 mmap 都没有的平台，
// 换一个纯 read/write 的后端是**局部改动**，不会波及六项能力的实现。
//
// # 与 native 的分工
//
// 本包**不做能力探测**，也不关心宿主到底支不支持某项能力 —— 它对每一项都给出
// 一个「一定能跑」的实现。选谁由 oscap 的矩阵决定（逐项，不是整体二选一）。
// 所以这里没有、也不许出现 `if 窄平台 { ... }` 这种分支。
package builtin

import (
	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// adapter 一个类型实现全部六项能力。
//
// 这正是 oscap/ports.go 刻意让六个接口方法名两两不重名换来的自由度：
// 六项能力共享同一份旁路存储与同一套 key 规则，拆成六个结构体只会让
// 「同一个对象的元数据」散落在六处，反而更难保证一致。
type adapter struct {
	st *store
}

// New 构造一套完整的 builtin 能力实现，签名即 oscap.Factory。
//
// 旁路存储在这里就打开（而不是首次使用时懒加载）：库打不开是**配置/环境问题**，
// 必须在启动阶段就暴露出来，而不是等第一个客户端来读扩展属性时才炸。
func New(o oscap.Options) (oscap.Set, error) {
	if err := o.Validate(); err != nil {
		return oscap.Set{}, err
	}
	st, err := openStore(o)
	if err != nil {
		return oscap.Set{}, err
	}
	a := &adapter{st: st}
	return oscap.Set{
		Xattr:   a,
		Sparse:  a,
		Streams: a,
		IDs:     a,
		Times:   a,
		DOS:     a,
		Close:   st.close,
	}, nil
}

// 编译期断言：六项能力一项都不能少。
//
// 这一行的价值不在于「好看」，而在于**缺项在编译期就红**，
// 不用等到 oscap.New 在运行期抛 IncompleteBuiltinError。
var (
	_ oscap.Xattr         = (*adapter)(nil)
	_ oscap.SparseFile    = (*adapter)(nil)
	_ oscap.NamedStream   = (*adapter)(nil)
	_ oscap.StableFileID  = (*adapter)(nil)
	_ oscap.CreationTime  = (*adapter)(nil)
	_ oscap.DOSAttributes = (*adapter)(nil)
	_ oscap.Factory       = New
)
