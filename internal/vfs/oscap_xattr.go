package vfs

// oscap_xattr.go —— vfs 与 internal/oscap 的**扩展属性接缝**（AGENTS.md §1.2 C9 / §5 P7）。
//
// # 这个文件为什么存在
//
// 在它出现之前，vfs 直接调 `unix.Getxattr` 一类的系统调用（xattr_unix.go），
// 而在没有 POSIX 扩展属性的平台上整项能力直接 `return ErrNotSupported`
// （xattr_other.go）。后果是 AGENTS.md §5 P7 明文记着的那个缺口：
// 宿主文件系统不支持 xattr 时（FAT32/exFAT 外置盘、`nouser_xattr` 挂载、
// 只读根、部分 NAS 导出），这些元数据会**静默丢失，连报错都没有**。
//
// 同一时期 internal/oscap 那套 port + native/builtin 双适配器已经写完了，
// 但**整个产品代码里没有一处用它**（只有 config 拿 ParseMode 校验字符串），
// 也就是说 `filesystem_mode` 设了等于没设。本文件就是把两边接上的那一刀：
// 从此 vfs **只认 oscap.Xattr 这一个接口**，至于它背后是宿主的 setxattr
// 还是 builtin 的旁路 KV，由能力矩阵逐项决定，vfs 不知道也不需要知道。
//
// # 没有第二条路
//
// 这里刻意**不留**「Provider 为 nil 就走老代码」的分支。AGENTS.md §1.2 明确
// 禁止 `if 窄平台 { 走另一套代码 }` 这种结构 —— 那会变成第二份永远没人测的
// 实现，而本仓库已经在这上面栽过（挂 build tag 的代码默认 CI 一行都不编译）。
// LocalFS 构造完成后 `l.caps` 恒非 nil，六个访问器也由 oscap.Provider 保证非 nil。

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/idealisan/patrickSamba/internal/oscap"
	"github.com/idealisan/patrickSamba/internal/oscap/builtin"
	"github.com/idealisan/patrickSamba/internal/oscap/native"
)

// openCaps 按 cfg.FilesystemMode 组装能力集合，或采用调用方注入的那个。
//
// 组装失败**必须**让 NewLocalFS 整体失败：一个没有能力来源的 LocalFS 是
// 半残状态，让它启动起来只会把错误推迟到第一个客户端读扩展属性的时候，
// 而那时现场已经隔了好几层。`filesystem_mode: native` 在宿主不支持时
// 启动即报错，正是靠这条路径兑现的（AGENTS.md §1.2）。
func (l *LocalFS) openCaps() error {
	if l.cfg.Caps != nil {
		l.caps = l.cfg.Caps
		l.ownCaps = false
		return nil
	}
	p, err := oscap.Open(l.cfg.FilesystemMode, oscap.Options{
		Root:         l.res.Root(),
		MetadataPath: oscapMetadataDir(l.cfg.MetadataPath),
		InstanceID:   l.cfg.InstanceID,
		ReadOnly:     l.cfg.ReadOnly,
	}, native.New, builtin.New)
	if err != nil {
		return err
	}
	l.caps = p
	l.ownCaps = true
	return nil
}

// oscapMetadataDir 把配置里的 metadata_path 折算成**交给 oscap 的目录**。
//
// 为什么传目录而不是原样传文件路径 —— 这是一个已经在本仓库发生过的事故形态
// （双实现撞同一个库文件，静默吐垃圾）：
//
//	internal/meta（Windows 的 POSIX 属主/权限旁路）  metadata-<hash>.db
//	internal/oscap/builtin（六项能力旁路）           .stupidsamba-oscap-<hash>.db
//
// 两者**文件名不同**，所以各自去算默认落点时天然不撞。但 `metadata_path`
// 若被用户配成一个**具体文件**，原样转给两边就会让它们打开同一个 inode ——
// 同一个 bbolt 库、互相看不懂对方的 bucket，且第二个打开方还会被 flock
// 挡在门外（本容器实测阻塞 5 秒后报「是否已被另一个实例占用」）。
//
// 传目录既保留了用户「把元数据放这儿」的意图，又让两边各自生成自己的文件名。
// 所以**不要**把这里「简化」成直接传 l.cfg.MetadataPath。
func oscapMetadataDir(p string) string {
	if p == "" {
		// 交给 builtin 自己决定默认落点（共享目录的兄弟位置）。
		return ""
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p
	}
	// 配成文件（或还不存在的路径）：取它所在的目录。
	// 不存在的路径按「文件」解释，与 internal/meta 的既有行为一致。
	return filepath.Dir(p)
}

// oscapXattr 把 oscap.Xattr（按 Ref 寻址）适配成 vfs 的 XattrAccessor
// （绑定单个对象）。
//
// 两个接口的差别只有「对象从哪来」：oscap 每次调用都带 Ref，vfs 的访问器
// 在构造时就绑定好对象。所以这一层只做两件事：塞 Ref、映射错误。
type oscapXattr struct {
	x   oscap.Xattr
	ref oscap.Ref
}

var _ XattrAccessor = (*oscapXattr)(nil)

func (a *oscapXattr) Get(name string) ([]byte, error) {
	v, err := a.x.GetXattr(a.ref, name)
	if err != nil {
		return nil, mapOscapError(err)
	}
	return v, nil
}

func (a *oscapXattr) Set(name string, value []byte) error {
	return mapOscapError(a.x.SetXattr(a.ref, name, value))
}

func (a *oscapXattr) Remove(name string) error {
	return mapOscapError(a.x.RemoveXattr(a.ref, name))
}

func (a *oscapXattr) List() ([]string, error) {
	names, err := a.x.ListXattr(a.ref)
	if err != nil {
		return nil, mapOscapError(err)
	}
	return names, nil
}

// xattrAt 返回作用在宿主机路径 host 上的扩展属性访问器。
//
// f 可以为 nil —— OpenAttrOnly 句柄背后本来就没有 *os.File，这是常态而不是
// 异常（见 oscap.Ref.Handle 的注释）。适配器两侧都备好了按路径操作的分支。
//
// **不返回错误**：能力在构造 Provider 时就已经选定，到这里不存在「拿不到
// 访问器」这种状态。老的 newXattrAccessor 那个 error 返回值实际上只用来表达
// 「本平台没有 xattr」，而这件事现在由 builtin 兜住了。
func (l *LocalFS) xattrAt(host string, f *os.File) XattrAccessor {
	return &oscapXattr{
		x:   l.caps.Xattr(),
		ref: oscap.Ref{Path: host, Handle: f},
	}
}

// mapOscapError 把 oscap 的 sentinel 收敛成 vfs 的 sentinel。
//
// 这是分层要求的一步：oscap 在 vfs **之下**，不 import vfs、自成一套错误
// （见 oscap 包注释「分层位置」），所以映射只能在这一侧做。漏掉这一步的
// 后果不是编译错误而是**运行期静默降级** —— SMB 层只认 vfs.Err*，
// 认不出来的错误会退化成 STATUS_UNSUCCESSFUL，客户端行为随即变得不可预测
// （errmap.go 文件头记的就是这件事）。
//
// 顺序很重要：oscap.ErrInvalidArg 刻意包装了 fs.ErrInvalid，若先落到
// mapError 就会被判成 ErrInvalidPath —— 「参数非法」和「路径非法」在
// NTSTATUS 上不是一回事。
func mapOscapError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, oscap.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, oscap.ErrNotSupported):
		return ErrNotSupported
	case errors.Is(err, oscap.ErrExist):
		return ErrExist
	case errors.Is(err, oscap.ErrReadOnly):
		return ErrReadOnly
	case errors.Is(err, oscap.ErrClosed):
		return ErrClosed
	case errors.Is(err, oscap.ErrInvalidArg):
		return ErrInvalidArg
	}
	// 适配器认不出来的 errno 是**原样往上抛**的（native/posix.go 的
	// mapPosixErr 刻意不把未知 errno 塞成 ErrNotSupported），到这里交给
	// 通用 errmap 处理：ENOSPC → ErrNoSpace、EACCES → ErrPermission 等。
	return mapError(err)
}
