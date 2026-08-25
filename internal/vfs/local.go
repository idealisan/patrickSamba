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
	"errors"
	"hash/fnv"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
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

	// MetadataPath 是旁路元数据库的路径。空则放在 Root 下的默认位置。
	//
	// **所有平台都用它**，有两个消费方，别再按"仅 Windows"理解：
	//   ① openMetadataStore —— POSIX 属主/权限位旁路，仅 Windows 编译进来
	//      （metadata_windows.go；非 Windows 上 openMetadataStore 返回 nil）；
	//   ② oscapMetadataDir —— oscap builtin 六项能力的旁路 bbolt 库
	//      （oscap_xattr.go），**不分平台**，auto 与 portable 两档实测都会
	//      在该目录下建出 .stupidsamba-oscap-<hash>.db。
	//
	// ② 是 PR #159 接线带来的，此前这里写的"仅 Windows 使用"从那时起就是假话。
	MetadataPath string

	// InstanceID 是**本服务实例**的稳定标识，语义与 oscap.Options.InstanceID 一致
	// （装配层用监听端点推导，见 cmd/stupidsamba 的 listenerInstanceID）。
	//
	// 为什么需要：两处旁路存储（oscap/builtin 六项能力、Windows POSIX 元数据）
	// 都是 bbolt 库按 path 拿 flock。多个服务进程共享同一共享目录时，若默认
	// 落点算出同一个文件，第二个进程会卡满 flock 超时后启动失败。
	// 非空时实例被确定性地编进默认库文件名，各进程互不阻塞；
	// 留空表示「单实例」语义，默认落点退回历史文件名
	// （测试、库直接调用等不经由服务装配层的场景都走这条，行为零变化）。
	// 显式配置的 MetadataPath 优先于本字段：用户手写落点时唯一性由用户负责。
	InstanceID string

	// FilesystemMode 是 OS 能力抽象的三态开关（AGENTS.md §1.2 C9）。
	// 零值 oscap.ModeAuto = 逐项探测，能 native 就 native。
	FilesystemMode oscap.Mode

	// Caps 是已经组装好的能力集合。
	//
	// 留 nil 时由 NewLocalFS 按 FilesystemMode 自己组装一个（native + builtin
	// 两侧工厂），并在 Close 时负责关闭它。
	//
	// ⚠️ **这不是「nil 就走老代码」的开关。** 运行期永远只有一条数据路径 ——
	// 扩展属性与命名流一律经由 oscap.Provider。这个字段的用途只有一个：
	// 让测试能注入一个受控的 Provider（例如计数器或返回哨兵错误的变异体），
	// 从而**证明**这条线真的被调用了。没有它，「测试通过」就只是
	// 「测试通过」，证明不了接线成立（AGENTS.md：验收判据必须可证伪）。
	Caps oscap.Provider

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

	// caps 是 OS 可选能力的来源，**恒非 nil**（构造失败就没有 LocalFS）。
	// 扩展属性与命名流全部经由它，vfs 不再直接碰任何平台系统调用。
	caps oscap.Provider

	// ownCaps 记录 caps 是不是我们自己造的。测试注入的 Provider 归调用方
	// 所有，我们不能替它关 —— 关掉别人的资源是一类很难查的 bug。
	ownCaps bool

	// usage 统计本共享自身占了多少空间，只在设了配额时非 nil
	// （不设配额就没人需要这个数字，一次目录遍历都不做）。
	usage *shareUsage

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

	meta, err := openMetadataStore(res.Root(), cfg.MetadataPath, cfg.InstanceID)
	if err != nil {
		return nil, err
	}
	l.meta = meta

	if err := l.openCaps(); err != nil {
		if meta != nil {
			_ = meta.Close()
		}
		return nil, err
	}

	// 配额生效时才需要知道「本共享已用多少」。首次统计立刻异步开始，
	// 好让客户端问到容量时（至少要先走完 NEGOTIATE/SESSION_SETUP/TREE_CONNECT）
	// 已经有真实数字可用。构造过程本身不阻塞。
	if cfg.QuotaBytes > 0 {
		l.usage = newShareUsage(res.Root())
		l.usage.start()
	}
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

// CapabilityMatrix 报出这个共享**实际**在用的能力矩阵（哪一项走 native、哪一项走 builtin）。
//
// 存在的理由是可证伪性，不是好奇心：`filesystem_mode` 这类开关最典型的失败形态是
// "配置读进来了、字段填上了、运行期没人消费"，而这种失败**不会报错**，
// 表现为三个取值行为完全一样。装配层的测试需要一个从包外看得见的判据来钉死
// "YAML 里写的 portable 真的让这个共享落到了 builtin"，光看配置结构体做不到这件事。
//
// 也适合启动日志打一行：运维排查"为什么这台机器上创建时间不对"时，
// 第一件想知道的就是这几项到底走的哪条路。
func (l *LocalFS) CapabilityMatrix() oscap.Matrix { return l.caps.Matrix() }

// Close 实现 FileSystem。
func (l *LocalFS) Close() error {
	var err error
	l.closeOnce.Do(func() {
		l.usage.stop()
		if l.meta != nil {
			err = l.meta.Close()
		}
		// 只关自己造的那个（注入的 Provider 归调用方所有）。
		// **不因为 meta 关失败就跳过这一步**：另一侧的资源同样要还，
		// 尤其 builtin 侧持有 bbolt 的文件锁，漏关会让下一次打开同一个库
		// 阻塞到超时。
		if l.ownCaps && l.caps != nil {
			if cerr := l.caps.Close(); cerr != nil && err == nil {
				err = cerr
			}
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
	if action == ActionCreated {
		l.applyCreateDOSAttrs(req.FileAttributes, host, nil, true, action)
		l.stampCreationTime(host, nil)
	}
	if req.Flags&OpenAttrOnly != 0 {
		return h, action, nil
	}
	// 目录只需读句柄：SMB 对目录的「写」是 SET_INFO（改属性/改名），
	// 那些操作走路径而不是 fd。
	f, err := openHostFile(host, os.O_RDONLY|openNoFollow, 0)
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

	f, err := openHostFile(host, flag|accessFlags(req.Flags, flag)|openNoFollow, l.cfg.FileMode)
	if err != nil {
		return nil, 0, mapError(err)
	}
	h.f = f
	if action == ActionSuperseded {
		// SUPERSEDE 的既定语义是「删除重建」（见上面对 O_TRUNC 近似的 TODO）。
		// 旧对象在旁路库里的记录必须整体作废 —— 否则新对象顶着前任的
		// 创建时间与 DOS 位，是跨对象元数据泄漏的又一入口（B5/B3 同源）。
		l.forgetPathMetadata(host)
	}
	l.applyCreateDOSAttrs(req.FileAttributes, host, f, false, action)
	if action == ActionCreated || action == ActionSuperseded {
		l.stampCreationTime(host, f)
	}
	return h, action, nil
}

// stampCreationTime 在**真正创建出新对象**时把 btime 落进旁路库（B4）。
//
// builtin/times.go 的注释描述的就是这一步 —— 此前它并不存在，新建对象的
// 创建时间永远查不到存储值。跳过条件与 caps 探测口径一致：矩阵已把
// CapCreationTime 交给 native 时（macOS birthtimespec / Windows / Linux statx
// BTIME），内核自己维护且读得到真值，旁路记录纯属浪费 —— Time Machine 的
// band 目录动辄十万级创建，能省一次库写入就省。
//
// SUPERSEDE 走到这里时旧记录已被 forgetPathMetadata 作废，
// 这里写下的就是新对象的起点，语义正确。
func (l *LocalFS) stampCreationTime(host string, f *os.File) {
	if l.caps.Matrix().Kind(oscap.CapCreationTime) == oscap.KindNative {
		return
	}
	_ = l.caps.Times().SetCreationTime(oscap.Ref{Path: host, Handle: f}, time.Now())
}

// applyCreateDOSAttrs 把 CREATE 请求携带的 FileAttributes 按 Samba 语义落库
// （对照 source3/smbd/open.c 的 open_file_ntcreate 与 possibly_set_archive）：
//
//   - 只对 created / overwritten / superseded 三种动作生效；
//     FILE_WAS_OPENED 一律不动属性（打开已存在文件时请求里的属性位被忽略）；
//   - FILE_ATTRIBUTE_DIRECTORY 静默剥掉（open.c:3891-3894，Windows 同款行为），
//     SPARSE/REPARSE 等客观事实位经 settableDOSAttributes 一并滤除；
//   - 普通文件叠加 FILE_ATTRIBUTE_ARCHIVE（open.c:3896-3899 "this mode is
//     only used if the file is created new"；possibly_set_archive 对
//     OVERWRITTEN/SUPERSEDED 也补）；
//   - 请求未携带属性位（raw==0）的新建**不落**记录：合成逻辑对普通文件本就
//     报 ARCHIVE，可观测行为一致 —— 避免给 Time Machine 十万级 band 目录的
//     每次创建都写一条旁路记录。覆盖/取代时若已有存储记录则读改写补 ARCHIVE，
//     没有就同样跳过。
//
// 落库失败静默忽略：CREATE 本身已成功，Samba 的 file_set_dosmode 失败同样
// 不回滚创建；把失败回给客户端只会让它误以为创建没发生。
func (l *LocalFS) applyCreateDOSAttrs(raw uint32, host string, f *os.File, isDir bool, action Action) {
	switch action {
	case ActionCreated, ActionOverwritten, ActionSuperseded:
	default:
		return
	}
	ref := oscap.Ref{Path: host, Handle: f}
	var eff uint32
	switch {
	case raw != 0:
		eff = raw & settableDOSAttributes // DIRECTORY 等客观位不落地
		if !isDir {
			eff |= FileAttributeArchive
		}
	case !isDir:
		bits, err := l.caps.DOS().DOSAttributes(ref)
		if err != nil || bits&FileAttributeArchive != 0 {
			return // 没有存储记录（合成兜底）或 ARCHIVE 已在，都无需写
		}
		eff = (bits | FileAttributeArchive) & settableDOSAttributes
	default:
		return
	}
	_ = l.caps.DOS().SetDOSAttributes(ref, eff)
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
	if bt, ok := l.creationTimeAt(host, nil); ok {
		a.CreateTime = bt
	}
	// 用 caps 的稳定 FileID 覆盖 fillSysAttr 填的 st.Ino：native 档直接给
	// 原生稳定 ID，builtin 档给 inode / 旁路分配号；拿不到时回落 st.Ino。
	a.FileID = l.fileIDAt(host, nil, a.FileID)
	// 把客户端显式设置过的 DOS 位 OR 进合成后的 base（DIRECTORY/SPARSE/
	// REPARSE 与「点开头 → HIDDEN」「属主无写权限 → READONLY」都不归 caps 管，
	// 由本文件其它逻辑负责）。
	l.mergeStoredDOS(host, nil, a)
	l.applyMetadata(host, a)
	return a, nil
}

// creationTimeAt 返回 host 对应的创建时间（birth time）。
//
// 优先用 oscap.Times()：builtin 旁路能给出**写入**时记下的真实值
// （POSIX 宿主大多没有 btime，statx 也未必拿得到），native 档在支持的文件系统
// 上给真值。
//
// 拿不到时（ErrNotSupported，典型的 POSIX 宿主无 btime）回落到平台的
// statCreateTime（statx STATX_BTIME / macOS Birthtimespec）：那是 fillSysAttr
// 之外唯一能补出真实创建时间的来源。两者都没有时返回 ok=false，调用方保留
// attrFromFileInfo 填的兜底值（Linux 上是 ctime，macOS/Windows 上已是真值）。
//
// 其它错误不打断 Stat：回落 statCreateTime，再不行就保留兜底值。创建时间读不出
// 不该让整次属性查询失败。
func (l *LocalFS) creationTimeAt(host string, f *os.File) (time.Time, bool) {
	ct, err := l.caps.Times().CreationTime(oscap.Ref{Path: host, Handle: f})
	if err == nil {
		return ct, true
	}
	if errors.Is(err, oscap.ErrNotSupported) {
		return statCreateTime(host)
	}
	return statCreateTime(host)
}

// fileIDAt 返回 host 的稳定 FileID。
//
// 优先用 oscap.IDs()（native 档给原生稳定 ID，builtin 档给 inode 或旁路分配号，
// 跨重命名稳定）。拿不到时返回 fallback —— 调用方传入 fillSysAttr 已填好的
// st.Ino，保证任何情况下 Attr.FileID 都有合理值。
func (l *LocalFS) fileIDAt(host string, f *os.File, fallback uint64) uint64 {
	if id, err := l.caps.IDs().FileID(oscap.Ref{Path: host, Handle: f}); err == nil {
		return id
	}
	return fallback
}

// mergeStoredDOS 把客户端**显式设置过**的 DOS 属性位 OR 进 a.FileAttributes。
//
// 边界（ports.go 已划清）：由文件系统客观事实推导的位（DIRECTORY / SPARSE /
// REPARSE_POINT）与 POSIX 约定合成的位（点开头 → HIDDEN、属主无写权限 →
// READONLY）由 vfs 层负责合成，caps.DOS() 只回答「有没有人显式设置过、设的是什么」。
//
// 没人设置过（ErrNotFound）就跳过；其它错误忽略 —— 读属性失败不该让整次
// Stat 失败。
func (l *LocalFS) mergeStoredDOS(host string, f *os.File, a *Attr) {
	bits, err := l.caps.DOS().DOSAttributes(oscap.Ref{Path: host, Handle: f})
	if err != nil {
		return
	}
	a.FileAttributes |= bits
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
	l.forgetPathMetadata(host)
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
	// 与 CREATE 建目录同语义：剥 DIRECTORY、其余位经 settable 过滤后存档
	//（applyCreateDOSAttrs 内部不叠 ARCHIVE —— 目录不该有 ARCHIVE 位）；
	// btime 同步落库。
	l.applyCreateDOSAttrs(attrs, host, nil, true, ActionCreated)
	l.stampCreationTime(host, nil)
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
	l.migratePathMetadata(src, dst)
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
//	Free  = min(宿主 Free,  配额 - **本共享**已用)
//
// 「已用」必须是本共享自己的占用量。曾经这里取的是宿主卷的已用量
// （Total-Free），理由是递归统计目录大小太慢 —— 那是一个**真实的阻断级 bug**：
// 一个**空的**、配了 2 GiB 配额的共享，只要放在一块已用 3.7 GiB 的宿主卷上，
// 算出来的可用空间就是 0，macOS 会直接拒绝启动 Time Machine 备份。
// 共享用了多少空间与宿主卷上别的东西用了多少空间毫无关系，这个口径从根上就是错的。
//
// 性能问题由 shareUsage 解决：统计在后台做、结果带缓存、重扫频率随目录规模
// 自动退避，QUERY_FS_INFO 路径上一次目录遍历都不做。
// 由此带来的失效场景（预热窗口、刷新滞后）在 usage.go 的 shareUsage 注释里列全了。
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

	// 还没有统计结果时按 0 处理（上报「配额全部可用」）。
	// 这个方向是安全的：宁可预热窗口内短暂高报，也不要像旧实现那样
	// 在一个空共享上报 0 而直接阻断备份。
	usedBytes, _ := l.usage.used()
	// 已用量向**上**取整成块数，与上面 Total 的向下取整同向：两边都让剩余偏小。
	usedBlocks := (usedBytes + bs - 1) / bs

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

// migratePathMetadata / forgetPathMetadata 是 caps 侧旁路账本
// （oscap builtin 的 btime/dosattr/xattr/stream/holes/fileid 六桶）的
// 同类伴随维护（B5）。meta 与 caps 是**两套**按路径记账的存储，
// rename/remove 必须两边都搬/都清，漏一边就等于没修。
//
// 错误刻意吞掉：宿主上的 rename/unlink 已经发生，此时把失败回给客户端
// 只会让它认为操作没成功，从而做出与服务端状态相悖的后续动作 ——
// 比「一条旁路记录暂时错位」伤害更大。这与上面 meta.Rename 的既有取舍一致。
func (l *LocalFS) migratePathMetadata(src, dst string) {
	m := l.caps.Migration()
	if m == nil {
		return
	}
	_ = m.RenameMetadata(src, dst)
}

func (l *LocalFS) forgetPathMetadata(host string) {
	m := l.caps.Migration()
	if m == nil {
		return
	}
	_ = m.DeleteMetadata(host)
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
