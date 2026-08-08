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
	// Alternate data stream 尚未实现（阶段二的 AFP_Resource 等）。
	// 明确返回不支持，而不是悄悄打开主数据流 —— 后者会让客户端
	// 把资源叉的内容写进文件本体，造成数据损坏。
	if req.Stream != "" {
		return nil, 0, ErrNotSupported
	}
	if l.cfg.ReadOnly && dispositionWrites(req.Disposition, req.Flags) {
		return nil, 0, ErrReadOnly
	}

	host, err := l.res.Resolve(req.Path)
	if err != nil {
		return nil, 0, err
	}
	name := path.Base("/" + req.Path) // 根目录时得到 "/"，下面会归一
	if req.Path == "" {
		name = "."
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
		return
	}
	// 没有记录：用配置里的数字标签兜底，让客户端看到一个稳定的属主。
	a.UID, a.GID = l.cfg.UID, l.cfg.GID
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

	if _, err := os.Lstat(dst); err == nil {
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
	return info, nil
}

// Streams 实现 FileSystem，列出 alternate data stream。
//
// 当前只报告主数据流 —— 这已经足够让 Windows 属性页与 macOS Finder 工作。
// 真正的 ADS（AFP_Resource / AFP_AfpInfo）是阶段二的事。
func (l *LocalFS) Streams(p string) ([]StreamInfo, error) {
	a, err := l.Stat(p)
	if err != nil {
		return nil, err
	}
	if a.FileAttributes&FileAttributeDirectory != 0 {
		// 目录没有主数据流。
		return nil, nil
	}
	return []StreamInfo{{Name: "::$DATA", Size: a.Size, Alloc: a.Alloc}}, nil
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
