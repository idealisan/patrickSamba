package vfs

// local.go —— FileSystem 接口的本地磁盘实现。
//
// 职责边界：
//   - **所有**来自客户端的路径都必须先过 Resolver（安全边界，AGENTS.md §8）；
//   - **所有**宿主机错误都必须过 mapError 收敛成 sentinel，
//     绝不能把 *os.PathError 泄漏给 SMB 层（否则 NTSTATUS 会退化成
//     STATUS_UNSUCCESSFUL，客户端行为会变得难以预测）；
//   - 只读共享在**入口**就拒绝所有写意图，不依赖宿主机的权限位。

import (
	"hash/fnv"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LocalConfig 是构造 LocalFS 所需的参数。
//
// 注意 UID/GID：按 AGENTS.md §1.1，它们只是**给 VFS 层用的数字标签**，
// 不做系统用户解析，也不要求宿主机上真的存在这个用户。
type LocalConfig struct {
	// Root 是共享根目录的宿主机路径，必须已存在且是目录。
	Root string

	// ReadOnly 为 true 时所有写操作返回 ErrReadOnly。
	ReadOnly bool

	// CaseInsensitive 打开大小写不敏感的回退查找（SMB 语义）。
	// 建议置 true：Windows 客户端经常用与磁盘上不同的大小写来打开文件。
	CaseInsensitive bool

	// VolumeLabel 是卷标，空则用共享根的目录名。
	VolumeLabel string

	// UID / GID 是报给客户端的属主标签。
	// 仅在宿主文件系统表达不了属主时（Windows）作为默认值使用。
	UID, GID uint32

	// FileMode / DirMode 是新建文件/目录的权限位，0 表示用默认值。
	FileMode fs.FileMode
	DirMode  fs.FileMode

	// MetadataPath 是旁路元数据库的路径（仅 Windows 使用，见 AGENTS.md §5 P7）。
	// 空则放在 Root 下的默认位置。
	MetadataPath string

	// QuotaBytes 限制**向客户端上报**的卷容量（字节），0 表示不限。
	//
	// Time Machine 会一直备份到把整个卷吃满为止，所以真实 NAS 都提供
	// 给 TM 共享设配额的能力。设了以后 StatFS 按 min(宿主真实值, 配额) 上报。
	//
	// 这**不是**强制配额：只影响上报的数字，不阻止本地写入。
	// 真正的强制配额要靠宿主文件系统，不在本软件职责范围内。
	QuotaBytes uint64
}

const (
	defaultFileMode fs.FileMode = 0o644
	defaultDirMode  fs.FileMode = 0o755
)

// LocalFS 把宿主机的一个目录导出为 SMB 共享。
//
// 并发安全：所有方法可被多 goroutine 并发调用。
type LocalFS struct {
	res *Resolver
	cfg LocalConfig

	label  string
	serial uint32

	// meta 是 POSIX 属主/权限的旁路存储，只在宿主文件系统表达不了它们的
	// 平台（Windows）上非 nil。Linux/macOS 上恒为 nil，零开销。
	meta MetadataStore

	closeOnce sync.Once
}

// 编译期断言：LocalFS 必须满足 FileSystem 契约。
var _ FileSystem = (*LocalFS)(nil)

// NewLocalFS 构造一个本地磁盘共享。
func NewLocalFS(cfg LocalConfig) (*LocalFS, error) {
	res, err := NewResolver(cfg.Root, cfg.CaseInsensitive)
	if err != nil {
		return nil, err
	}
	if cfg.FileMode == 0 {
		cfg.FileMode = defaultFileMode
	}
	if cfg.DirMode == 0 {
		cfg.DirMode = defaultDirMode
	}

	label := cfg.VolumeLabel
	if label == "" {
		label = filepath.Base(res.Root())
	}

	l := &LocalFS{
		res:    res,
		cfg:    cfg,
		label:  label,
		serial: volumeSerial(res.Root()),
	}

	meta, err := openMetadataStore(res.Root(), cfg.MetadataPath)
	if err != nil {
		return nil, err
	}
	l.meta = meta
	return l, nil
}

// volumeSerial 由共享根路径派生一个稳定的卷序列号。
//
// 为什么要稳定：Windows 客户端会缓存 (VolumeSerial, FileID) 作为文件身份，
// 服务重启后序列号变了会导致客户端缓存失效甚至误判文件被替换。
// 用路径哈希而不是随机数正是为了跨重启稳定。
func volumeSerial(root string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(root))
	s := h.Sum32()
	if s == 0 {
		s = 1 // 0 在部分客户端里被当作「无效卷」
	}
	return s
}

// Root 返回共享根的宿主机绝对路径（供测试与日志用）。
func (l *LocalFS) Root() string { return l.res.Root() }

// ReadOnly 实现 FileSystem。
func (l *LocalFS) ReadOnly() bool { return l.cfg.ReadOnly }

// Close 实现 FileSystem。
func (l *LocalFS) Close() error {
	var err error
	l.closeOnce.Do(func() {
		if l.meta != nil {
			err = l.meta.Close()
		}
	})
	return err
}

// ---------------------------------------------------------------- Open

// Open 实现 FileSystem，把 SMB2 CREATE 的语义落到 os.OpenFile 上。
func (l *LocalFS) Open(req *OpenRequest) (Handle, Action, error) {
	if req == nil {
		return nil, 0, ErrInvalidArg
	}
	if l.cfg.ReadOnly && dispositionWrites(req.Disposition, req.Flags) {
		return nil, 0, ErrReadOnly
	}

	// 流名有两个来源：显式的 req.Stream，以及路径里的 "file:stream:$DATA"
	// 语法。后者是 macOS 客户端常用的写法，必须在 Resolve **之前**剥掉，
	// 否则冒号会被当成非法文件名字符而拒绝掉一个合法请求。
	reqPath, stream := req.Path, req.Stream
	if stream == "" {
		var err error
		if reqPath, stream, err = SplitStreamPath(req.Path); err != nil {
			return nil, 0, err
		}
	} else if err := ValidateStreamName(stream); err != nil {
		return nil, 0, err
	}

	host, err := l.res.Resolve(reqPath)
	if err != nil {
		return nil, 0, err
	}
	name := path.Base("/" + reqPath) // 根目录时得到 "/"，下面会归一
	if reqPath == "" {
		name = "."
	}

	if stream != "" {
		return l.openStream(req, host, name, stream)
	}

	fi, statErr := os.Lstat(host)
	exists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, 0, mapError(statErr)
	}

	// 软链：Resolve 已确认其目标在共享内，这里求值成实路径，
	// 以便后面带 O_NOFOLLOW 打开时不会被 ELOOP 挡住（见 EvalFinal 的说明）。
	if exists && fi.Mode()&fs.ModeSymlink != 0 && req.Flags&OpenNoFollow == 0 {
		real, err := l.res.EvalFinal(host)
		if err != nil {
			return nil, 0, err
		}
		host = real
		if fi, err = os.Lstat(host); err != nil {
			return nil, 0, mapError(err)
		}
	}

	isDir := exists && fi.IsDir()
	wantDir := req.Flags&OpenDirectory != 0

	switch {
	case exists && isDir && req.Flags&OpenNonDirectory != 0:
		// FILE_NON_DIRECTORY_FILE 遇到目录 → STATUS_FILE_IS_A_DIRECTORY
		return nil, 0, ErrIsDir
	case exists && !isDir && wantDir:
		// FILE_DIRECTORY_FILE 遇到文件 → STATUS_NOT_A_DIRECTORY
		return nil, 0, ErrNotDir
	}

	if isDir || (!exists && wantDir) {
		return l.openDir(req, host, name, exists)
	}
	return l.openFile(req, host, name, exists)
}

// dispositionWrites 判断这次 CREATE 是否带有写意图。
// 只读共享在入口就要挡掉，不能等到真正写的时候才失败 ——
// 客户端拿到句柄之后会认为写一定成功。
func dispositionWrites(d Disposition, f OpenFlags) bool {
	if f&(OpenWrite|OpenAppend|OpenDeleteOnClose) != 0 {
		return true
	}
	switch d {
	case Supersede, CreateNew, OpenAlways, TruncateExisting, TruncateAlways:
		return true
	}
	return false
}

// openDir 处理目录的打开与创建。
func (l *LocalFS) openDir(req *OpenRequest, host, name string, exists bool) (Handle, Action, error) {
	action := ActionOpened

	if !exists {
		switch req.Disposition {
		case CreateNew, OpenAlways, TruncateAlways, Supersede:
			if err := os.Mkdir(host, l.cfg.DirMode); err != nil {
				return nil, 0, mapError(err)
			}
			action = ActionCreated
		default: // OpenExisting / TruncateExisting
			return nil, 0, ErrNotFound
		}
	} else {
		switch req.Disposition {
		case CreateNew:
			return nil, 0, ErrExist
		case Supersede, TruncateExisting, TruncateAlways:
			// 目录无法被截断/取代。Windows 在这里返回
			// STATUS_INVALID_PARAMETER，我们用 ErrIsDir 表达「对象是目录」。
			return nil, 0, ErrIsDir
		}
	}

	h := l.newHandle(req, host, name, true)
	if req.Flags&OpenAttrOnly != 0 {
		return h, action, nil
	}
	// 目录只需读句柄：SMB 对目录的「写」是 SET_INFO（改属性/改名），
	// 那些操作走路径而不是 fd。
	f, err := os.OpenFile(host, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, 0, mapError(err)
	}
	h.f = f
	return h, action, nil
}

// openFile 处理普通文件的打开与创建，实现 MS-SMB2 §2.2.13 的
// CreateDisposition 语义（对照表见 docs/protocol-notes.md §8）。
func (l *LocalFS) openFile(req *OpenRequest, host, name string, exists bool) (Handle, Action, error) {
	var (
		flag   int
		action Action
	)
	switch req.Disposition {
	case Supersede:
		// 「删除重建」：语义上原文件的属性、ADS 都应被丢弃。
		// 用 O_TRUNC 近似（内容清空、inode 保留）。
		// TODO: 严格实现需 unlink + create，但那会破坏其他已打开句柄，
		//       且 Windows 客户端极少用 SUPERSEDE，先按 Samba 的做法来。
		flag = os.O_CREATE | os.O_TRUNC
		if exists {
			action = ActionSuperseded
		} else {
			action = ActionCreated
		}
	case OpenExisting:
		if !exists {
			return nil, 0, ErrNotFound
		}
		action = ActionOpened
	case CreateNew:
		// O_EXCL 让「存在则失败」是原子的，不依赖上面的 lstat 结果。
		flag = os.O_CREATE | os.O_EXCL
		action = ActionCreated
	case OpenAlways:
		flag = os.O_CREATE
		if exists {
			action = ActionOpened
		} else {
			action = ActionCreated
		}
	case TruncateExisting:
		if !exists {
			return nil, 0, ErrNotFound
		}
		flag = os.O_TRUNC
		action = ActionOverwritten
	case TruncateAlways:
		flag = os.O_CREATE | os.O_TRUNC
		if exists {
			action = ActionOverwritten
		} else {
			action = ActionCreated
		}
	default:
		return nil, 0, ErrInvalidArg
	}

	h := l.newHandle(req, host, name, false)

	// 仅查属性：不真的 open。Explorer 会对目录里每个文件做一次属性探测，
	// 省下这一次 open/close 对大目录的收益很大（docs/protocol-notes.md §8）。
	// 注意此时若文件不存在且 disposition 要求创建，仍需真的创建出来。
	if req.Flags&OpenAttrOnly != 0 && flag&os.O_CREATE == 0 && flag&os.O_TRUNC == 0 {
		return h, action, nil
	}

	f, err := os.OpenFile(host, flag|accessFlags(req.Flags, flag)|openNoFollow, l.cfg.FileMode)
	if err != nil {
		return nil, 0, mapError(err)
	}
	h.f = f
	return h, action, nil
}

// accessFlags 把 OpenFlags 的读写意图翻译成 O_RDONLY/O_WRONLY/O_RDWR。
//
// 两点特别说明：
//
//  1. **绝不设置 O_APPEND**。SMB2 WRITE 永远带显式 offset（pwrite 语义），
//     O_APPEND 会让内核忽略 offset 把数据全追加到尾部，导致文件内容错乱。
//     协议里的「追加」是客户端发 offset=0xFFFFFFFFFFFFFFFF，
//     由 server 层换算成当前 EOF，不需要内核帮忙。
//  2. 若 disposition 要求 O_TRUNC/O_CREATE，即使客户端只请求了读权限，
//     也必须至少有写权限，否则 open 会 EACCES。
func accessFlags(f OpenFlags, dispFlag int) int {
	wantWrite := f&(OpenWrite|OpenAppend) != 0 || dispFlag&(os.O_TRUNC|os.O_CREATE) != 0
	wantRead := f&OpenRead != 0 || !wantWrite

	switch {
	case wantRead && wantWrite:
		return os.O_RDWR
	case wantWrite:
		return os.O_WRONLY
	default:
		return os.O_RDONLY
	}
}

func (l *LocalFS) newHandle(req *OpenRequest, host, name string, isDir bool) *localHandle {
	rel, _ := CleanPath(req.Path) // Resolve 已经校验过，这里不会失败
	return &localHandle{
		fs:            l,
		host:          host,
		rel:           rel,
		name:          name,
		isDir:         isDir,
		writable:      !l.cfg.ReadOnly && req.Flags&(OpenWrite|OpenAppend) != 0,
		writeThrough:  req.Flags&OpenWriteThrough != 0,
		deleteOnClose: req.Flags&OpenDeleteOnClose != 0,
	}
}

// ---------------------------------------------------------------- 路径级操作

// Stat 实现 FileSystem。
func (l *LocalFS) Stat(p string) (*Attr, error) {
	host, err := l.res.Resolve(p)
	if err != nil {
		return nil, err
	}
	rel, _ := CleanPath(p)
	return l.statHost(host, baseName(rel))
}

// statHost 是所有属性查询的唯一出口，保证 Stat / ReadDir / Handle.Stat
// 给出的字段口径完全一致。
func (l *LocalFS) statHost(host, name string) (*Attr, error) {
	// 先 Stat（跟随软链，与 Samba 的默认行为一致；软链是否越界已由
	// Resolver 校验过）。目标不存在的悬空软链退回 Lstat，
	// 这样客户端至少能看到并删除它，而不是撞上一个查不到的幽灵条目。
	fi, err := os.Stat(host)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, mapError(err)
		}
		if fi, err = os.Lstat(host); err != nil {
			return nil, mapError(err)
		}
	}
	a := attrFromFileInfo(fi, name, l.cfg.ReadOnly)
	if bt, ok := statCreateTime(host); ok {
		a.CreateTime = bt
	}
	l.applyMetadata(host, a)
	return a, nil
}

// applyMetadata 用旁路存储里的 POSIX 属主/权限覆盖 Attr。
// 只在宿主文件系统表达不了它们的平台上生效（AGENTS.md §5 P7）。
func (l *LocalFS) applyMetadata(host string, a *Attr) {
	if l.meta == nil {
		// Linux/macOS：宿主机原生就有 uid/gid/mode，直接采信。
		return
	}
	rel, err := filepath.Rel(l.res.Root(), host)
	if err != nil {
		rel = host
	}
	if md, ok := l.meta.Get(filepath.ToSlash(rel)); ok {
		a.UID, a.GID, a.Mode = md.UID, md.GID, md.Mode
	} else {
		// 没有记录：用配置里的数字标签兜底，让客户端看到一个稳定的属主。
		a.UID, a.GID = l.cfg.UID, l.cfg.GID
	}

	// NTFS 没有 POSIX 权限位，attrFromFileInfo 在 Windows 上给不出 Mode。
	// 留 0 会让 macOS/Linux 客户端看到一个「谁都不能读写」的对象，
	// Time Machine 会直接判定备份目标不可用，所以必须兜一个合理默认值。
	//
	// ⚠️ 这段兜底必须对**两条路径**都生效，不能只挂在「没有记录」那一支。
	// 旧实现命中记录后直接 return，于是「客户端只设过 UID」留下的
	// {uid, gid, 0} 记录会让回读的 Mode 变成 0 —— 恰好就是上面这三行注释
	// 描述的后果，而兜底逻辑当时够不着。
	//
	// 代价（有意为之）：Metadata.Mode 没有「未设置」标记，所以这里把
	// **0 一律当成未设置**。客户端若真想把权限设成 000，会被兜回默认值。
	// POSIX 权限在本项目里只是给客户端看的展示值（授权由 valid_users /
	// read_only 决定，见 AGENTS.md §1.1），拿不到 000 不影响访问控制；
	// 而「整个共享显示成 000」会直接废掉 Time Machine —— 两害相权取其轻。
	// 真要忠实表达 000，得让 Metadata 带一个「Mode 已设置」的显式标记。
	if a.Mode == 0 {
		mode := l.cfg.FileMode
		if a.FileAttributes&FileAttributeDirectory != 0 {
			mode = l.cfg.DirMode
		}
		// 与 DOS 的 READONLY 位（以及只读共享）保持一致，避免客户端
		// 看到「权限位可写但一写就被拒」的矛盾状态。
		if a.FileAttributes&FileAttributeReadonly != 0 {
			mode &^= 0o222
		}
		a.Mode = uint32(mode.Perm())
	}
}

// Remove 实现 FileSystem。文件与空目录都走这里。
func (l *LocalFS) Remove(p string) error {
	if l.cfg.ReadOnly {
		return ErrReadOnly
	}
	dir, name, err := l.res.ResolveParent(p)
	if err != nil {
		return err
	}
	host := filepath.Join(dir, name)
	if err := os.Remove(host); err != nil {
		return mapError(err)
	}
	l.forgetMetadata(host)
	return nil
}

// Mkdir 实现 FileSystem。
func (l *LocalFS) Mkdir(p string, attrs uint32) error {
	if l.cfg.ReadOnly {
		return ErrReadOnly
	}
	dir, name, err := l.res.ResolveParent(p)
	if err != nil {
		return err
	}
	host := filepath.Join(dir, name)
	if err := os.Mkdir(host, l.cfg.DirMode); err != nil {
		return mapError(err)
	}
	// attrs 里客户端可能要求 HIDDEN/SYSTEM 等。POSIX 上除了 READONLY
	// 之外没有对应物，静默忽略（返回错误会让客户端认为 mkdir 失败）。
	if attrs&FileAttributeReadonly != 0 {
		_ = os.Chmod(host, l.cfg.DirMode&^0o222)
	}
	return nil
}

// Rename 实现 FileSystem（对应 FileRenameInformation）。
func (l *LocalFS) Rename(oldPath, newPath string, replace bool) error {
	if l.cfg.ReadOnly {
		return ErrReadOnly
	}
	oldDir, oldName, err := l.res.ResolveParent(oldPath)
	if err != nil {
		return err
	}
	newDir, newName, err := l.res.ResolveParent(newPath)
	if err != nil {
		return err
	}
	src := filepath.Join(oldDir, oldName)
	dst := filepath.Join(newDir, newName)

	// 大小写不敏感时 ResolveParent 会把 newName 折叠成磁盘上已存在的写法，
	// 于是 "a.txt" → "A.TXT" 这种**纯改大小写**的重命名会被当成同名 no-op 丢掉。
	// 用客户端原始路径的最后一个分量还原它真正想要的写法。
	//
	// 只在「同一父目录 + 仅大小写不同 + 折叠确实发生了」时才还原，
	// 此时目标必然就是 src 自己（sameObject），不能走下面的「目标已存在」分支：
	// 否则 replace=false 会误报 ErrExist，replace=true 会先把源文件删掉丢数据。
	sameObject := false
	if l.cfg.CaseInsensitive && oldDir == newDir && strings.EqualFold(oldName, newName) {
		if want := lastComponent(newPath); want != "" && want != newName {
			dst = filepath.Join(newDir, want)
			sameObject = true
		}
	}

	if src == dst {
		return nil
	}

	if dstFI, err := os.Lstat(dst); err == nil {
		if !sameObject && sameDirEntry(oldDir, newDir, src, dstFI) {
			sameObject = true
		}
		if !sameObject {
			if !replace {
				return ErrExist
			}
			// os.Rename 在 Windows 上不会覆盖已存在的目标，在 Unix 上会。
			// 统一先删再改名，保证行为一致。
			if err := os.Remove(dst); err != nil {
				return mapError(err)
			}
		}
	} else if !os.IsNotExist(err) {
		return mapError(err)
	}

	if err := os.Rename(src, dst); err != nil {
		return mapError(err)
	}
	l.renameMetadata(src, dst)
	return nil
}

// sameDirEntry 判断 dst 是不是 src **同一个目录项**的另一个写法。
//
// 为什么需要它：宿主文件系统可能对名字做等价折叠 —— NTFS/APFS 大小写不敏感，
// APFS 还认 Unicode 规范化等价（é 的 NFC 与 NFD 两种写法）。于是
// os.Lstat(dst) 会**成功**，但它命中的其实就是 src 自己。此时若照
// 「目标已存在」处理，replace=true 会先 os.Remove(dst) —— 删掉的正是源文件，
// **数据当场丢失**，紧接着 os.Rename 报 ENOENT。
//
// 上面 Rename 里那个 sameObject 只覆盖「大小写折叠」这一种成因，而且判据是
// l.cfg.CaseInsensitive（本软件的配置项），不是宿主文件系统的真实行为 ——
// 那个开关一旦变成用户可配的，这个洞立刻就能打到。这里改成问内核：
// os.SameFile 在 Unix 上比 dev+ino，在 Windows 上比卷序列号 + 文件索引。
//
// 为什么还要求同一个父目录：SameFile 对**硬链接**也返回 true，而两个不同
// 目录项的硬链接删掉一个不会丢数据，旧的「先删后改名」对它是正确的。
// 不同目录下的两个名字不可能是同一个目录项，所以限定同父目录既堵住了
// 数据丢失，又不改硬链接的既有行为。
//
// 残留的取舍（有意为之）：同一目录下互为硬链接的两个**不同**名字，
// 现在会被判成同一对象而跳过删除，rename() 在 POSIX 上退化为 no-op，
// 结果是两个名字都还在。这个角落极其罕见，且后果是「少删了一个名字」，
// 不是丢数据 —— 与上面那条相比，取轻的。
func sameDirEntry(oldDir, newDir, src string, dstFI os.FileInfo) bool {
	if oldDir != newDir {
		return false
	}
	srcFI, err := os.Lstat(src)
	return err == nil && os.SameFile(srcFI, dstFI)
}

// StatFS 实现 FileSystem。
func (l *LocalFS) StatFS() (*FSInfo, error) {
	info := &FSInfo{
		VolumeLabel:  l.label,
		VolumeSerial: l.serial,
		// 对客户端一律宣称大小写不敏感：这是 SMB/Windows 的语义，
		// 也是 Resolver 在做大小写回退查找时实际提供的行为。
		CaseSensitive:   false,
		MaxComponentLen: MaxComponentLen,
		BlockSize:       4096,
	}
	if err := platformStatFS(l.res.Root(), info); err != nil {
		return nil, err
	}
	if info.BlockSize == 0 {
		info.BlockSize = 4096
	}
	l.applyQuota(info)
	return info, nil
}

// applyQuota 把上报的容量压到配额以内。
//
// 语义：配额限制的是**本共享**的总容量，所以
//
//	Total = min(宿主 Total, 配额)
//	Free  = min(宿主 Free,  配额 - 已用)
//
// 「已用」取共享自己的占用量还是宿主的占用量？这里取**宿主的**
// （Total-Free），理由是递归统计共享目录大小在十万级 band 目录上
// 要几秒钟，每次 QUERY_FS_INFO 都做一遍完全不可接受。
// 代价是：同一个宿主卷上放多个带配额的共享时，它们互相看得见对方的占用。
// 对 Time Machine 这个主要场景（一块盘一个备份共享）是准确的。
func (l *LocalFS) applyQuota(info *FSInfo) {
	if l.cfg.QuotaBytes == 0 {
		return
	}
	bs := uint64(info.BlockSize)
	if bs == 0 {
		return
	}
	// 向下取整成块数：宁可少报一点，也不要报出一个写不进去的容量。
	quotaBlocks := l.cfg.QuotaBytes / bs

	usedBlocks := uint64(0)
	if info.TotalBlocks > info.FreeBlocks {
		usedBlocks = info.TotalBlocks - info.FreeBlocks
	}

	var quotaFree uint64
	if quotaBlocks > usedBlocks {
		quotaFree = quotaBlocks - usedBlocks
	}

	if quotaBlocks < info.TotalBlocks {
		info.TotalBlocks = quotaBlocks
	}
	if quotaFree < info.FreeBlocks {
		info.FreeBlocks = quotaFree
	}

	// 收尾时强制维持 Avail <= Free <= Total。
	//
	// 少了这一步会在一种真实情况下倒挂：有些网络文件系统的 statfs
	// 会返回 Free > Total。那时 usedBlocks 被算成 0，配额剩余就没被压住，
	// 于是压完 Total 之后 Free 反而比 Total 大。
	// 客户端（尤其 Time Machine）会拿这几个数做减法，一旦倒挂就会
	// 算出天文数字的可用空间，然后一路写到真正的 ENOSPC。
	if info.FreeBlocks > info.TotalBlocks {
		info.FreeBlocks = info.TotalBlocks
	}
	if info.AvailBlocks > info.FreeBlocks {
		info.AvailBlocks = info.FreeBlocks
	}
}

// Streams 实现 FileSystem，列出 alternate data stream
// （对应 SMB2 QUERY_INFO 的 FileStreamInformation）。
//
// 除主数据流外，还会报告存在的 AFP_AfpInfo / AFP_Resource ——
// macOS Finder 靠这个判断文件有没有资源派生与 FinderInfo。
func (l *LocalFS) Streams(p string) ([]StreamInfo, error) {
	base, stream, err := SplitStreamPath(p)
	if err != nil {
		return nil, err
	}
	if stream != "" {
		// 对一个流本身查询流列表是没有意义的请求。
		return nil, ErrInvalidPath
	}
	host, err := l.res.Resolve(base)
	if err != nil {
		return nil, err
	}
	a, err := l.statHost(host, baseName(base))
	if err != nil {
		return nil, err
	}
	return l.streamsOf(host, a), nil
}

// ---------------------------------------------------------------- 小工具

// baseName 取相对路径的末级名字；共享根返回 "."。
func baseName(rel string) string {
	if rel == "" {
		return "."
	}
	return path.Base(rel)
}

// lastComponent 取客户端原始路径的最后一个分量（不做大小写折叠）。
func lastComponent(p string) string {
	comps, err := SplitPath(p)
	if err != nil || len(comps) == 0 {
		return ""
	}
	return comps[len(comps)-1]
}

// forgetMetadata / renameMetadata 维护旁路存储与真实文件的一致性。
// meta 为 nil（Linux/macOS）时是空操作。
func (l *LocalFS) forgetMetadata(host string) {
	if l.meta == nil {
		return
	}
	if rel, err := filepath.Rel(l.res.Root(), host); err == nil {
		_ = l.meta.Delete(filepath.ToSlash(rel))
	}
}

func (l *LocalFS) renameMetadata(src, dst string) {
	if l.meta == nil {
		return
	}
	relSrc, err1 := filepath.Rel(l.res.Root(), src)
	relDst, err2 := filepath.Rel(l.res.Root(), dst)
	if err1 != nil || err2 != nil {
		return
	}
	_ = l.meta.Rename(filepath.ToSlash(relSrc), filepath.ToSlash(relDst))
}

// setTimes 设置访问/修改时间。未指定的一方保持原值。
func setTimes(host string, atime, mtime time.Time) error {
	if atime.IsZero() && mtime.IsZero() {
		return nil
	}
	if atime.IsZero() || mtime.IsZero() {
		fi, err := os.Stat(host)
		if err != nil {
			return mapError(err)
		}
		if atime.IsZero() {
			atime = fi.ModTime()
		}
		if mtime.IsZero() {
			mtime = fi.ModTime()
		}
	}
	if err := os.Chtimes(host, atime, mtime); err != nil {
		return mapError(err)
	}
	return nil
}
