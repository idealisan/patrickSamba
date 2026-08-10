package vfs

// local_handle.go —— Handle 接口的本地磁盘实现。
//
// 关键约束：
//   - 所有来自网络的 offset/length 在使用前必须校验边界（AGENTS.md §8）。
//     Go 的 pread/pwrite 对负 offset 会返回错误而不是崩，但**加法溢出**
//     会把 off+len 绕回负数，必须自己拦。
//   - 同一个句柄会被并发调用（客户端在一个 FileId 上并发下发 READ/WRITE），
//     ReadAt/WriteAt 用 pread/pwrite 天然并发安全；有状态的只有目录枚举
//     游标，用 mutex 保护。

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

type localHandle struct {
	fs   *LocalFS
	host string // 宿主机绝对路径
	rel  string // 共享内相对路径（'/' 分隔）
	name string // 末级名字，用于 HIDDEN 判定

	// f 为 nil 表示 OpenAttrOnly 句柄：没有真正打开文件，只能查属性。
	f *os.File

	isDir        bool
	writable     bool
	writeThrough bool

	mu            sync.Mutex
	closed        bool
	deleteOnClose bool

	// 目录枚举状态。dirNames 是**一次性快照**：
	// SMB2 的 QUERY_DIRECTORY 是分批拉取的，中途目录内容变化时
	// 客户端期待看到的是一个稳定的视图（Windows 服务端亦然）。
	dirNames []string
	dirPos   int

	// dirExact 记录本轮枚举走的是「精确名字」快速路径（见 lookupExactLocked）。
	// 那条路径不拍快照，dirNames 保持 nil，所以需要一个独立的标志位
	// 来表示「唯一那条已经吐过了，再问就是 NO_MORE_FILES」。
	dirExactDone bool

	// dotUnder 是快照时看到的、存在 "._<name>" 旁路文件的 <name> 集合。
	//
	// 只在真的看到 ._ 文件时才分配 —— Time Machine 的 bands 目录一个都没有，
	// 那里恒为 nil，零内存开销。AAPL readdir_attr 靠它跳过对每个条目
	// 「试着打开 ._ 文件」的那次注定失败的 open（见 AppleInfoAt）。
	dotUnder map[string]struct{}
}

var _ Handle = (*localHandle)(nil)

// ---------------------------------------------------------------- 数据读写

// checkRange 校验 offset/length。这是**防越界与整数溢出的唯一入口**。
func checkRange(off int64, n int) error {
	if off < 0 || n < 0 {
		return ErrInvalidArg
	}
	if int64(n) > math.MaxInt64-off {
		// off + n 会溢出成负数，后续任何比较都不可信。
		return ErrInvalidArg
	}
	return nil
}

// ReadAt 实现 Handle。
func (h *localHandle) ReadAt(p []byte, off int64) (int, error) {
	if err := checkRange(off, len(p)); err != nil {
		return 0, err
	}
	if h.isDir {
		return 0, ErrIsDir
	}
	if h.f == nil {
		// OpenAttrOnly 句柄没有数据流。
		return 0, ErrNotSupported
	}
	n, err := h.f.ReadAt(p, off)
	if err == io.EOF {
		return n, io.EOF
	}
	return n, mapError(err)
}

// WriteAt 实现 Handle。
func (h *localHandle) WriteAt(p []byte, off int64) (int, error) {
	if err := checkRange(off, len(p)); err != nil {
		return 0, err
	}
	if err := h.checkWritable(); err != nil {
		return 0, err
	}
	n, err := h.f.WriteAt(p, off)
	if err != nil {
		return n, mapError(err)
	}
	if h.writeThrough {
		// FILE_WRITE_THROUGH：客户端要求每次写都落盘。
		// 慢，但这是它显式要的语义，不能偷懒。
		//
		// 用 platformFullSync 而不是 f.Sync()：在 macOS 上后者只把数据
		// 交给磁盘控制器，掉电仍可能丢 —— 客户端显式要了持久化就应该
		// 给它真正的持久化。Linux/Windows 上两者等价，无额外代价。
		if err := platformFullSync(h.f); err != nil {
			return n, mapError(err)
		}
	}
	return n, nil
}

// checkWritable 收敛「这个句柄能不能写」的判断。
func (h *localHandle) checkWritable() error {
	if h.fs.cfg.ReadOnly {
		return ErrReadOnly
	}
	if h.isDir {
		return ErrIsDir
	}
	if h.f == nil {
		return ErrNotSupported
	}
	if !h.writable {
		// 句柄打开时没有申请写权限 → STATUS_ACCESS_DENIED。
		// SMB 的访问检查看的是 CREATE 时授予的权限，不是此刻的文件权限位。
		return ErrPermission
	}
	return nil
}

// Truncate 实现 Handle（对应 FileEndOfFileInformation）。
func (h *localHandle) Truncate(size int64) error {
	if size < 0 {
		return ErrInvalidArg
	}
	if err := h.checkWritable(); err != nil {
		return err
	}
	return mapError(h.f.Truncate(size))
}

// Sync 实现 Handle。
//
// full=true 对应 SMB2 FLUSH 与 macOS 的 F_FULLFSYNC：
// Time Machine 依赖它保证备份数据真的落到盘片上（AGENTS.md §2 阶段二）。
// 各平台的落地见 platformFullSync：
//
//	linux    fsync(2)（Linux 的 fsync 本就要求刷到持久介质；
//	         fdatasync 不够，元数据也要落）
//	darwin   fcntl(F_FULLFSYNC)，失败时退化为 fsync
//	windows  FlushFileBuffers
func (h *localHandle) Sync(full bool) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	f := h.f
	h.mu.Unlock()

	if f == nil {
		// OpenAttrOnly 句柄没有数据流，没有东西需要刷。
		return nil
	}
	if full {
		return mapError(platformFullSync(f))
	}
	return mapError(f.Sync())
}

// ---------------------------------------------------------------- 属性

// Stat 实现 Handle。
func (h *localHandle) Stat() (*Attr, error) {
	if h.f != nil {
		// 优先 fstat：句柄已经打开，即使文件被改名（甚至已被 unlink，
		// 例如 DELETE_ON_CLOSE 还没触发）也能拿到正确属性。
		if fi, err := h.f.Stat(); err == nil {
			a := attrFromFileInfo(fi, h.name, h.fs.cfg.ReadOnly)
			if bt, ok := h.fs.creationTimeAt(h.host, h.f); ok {
				a.CreateTime = bt
			}
			// 用 caps 的稳定 FileID 覆盖 fillSysAttr 填的 st.Ino。
			a.FileID = h.fs.fileIDAt(h.host, h.f, a.FileID)
			fillSysAttrFromFile(h.f, a)
			h.fs.mergeStoredDOS(h.host, h.f, a)
			h.fs.applyMetadata(h.host, a)
			return a, nil
		}
	}
	return h.fs.statHost(h.host, h.name)
}

// SetAttr 实现 Handle。
//
// 设计取舍：**无法表达的属性静默忽略，而不是报错**。
// Windows 在复制文件时会成套地设置 basic information，
// 其中的 CreateTime / ChangeTime / 大部分 DOS 位在 POSIX 上没有对应物；
// 若返回错误，Explorer 会认为整个复制失败。Samba 也是这么做的。
func (h *localHandle) SetAttr(attr *Attr, mask AttrMask) error {
	if attr == nil || mask == 0 {
		return nil
	}
	if h.fs.cfg.ReadOnly {
		return ErrReadOnly
	}

	if mask&AttrSize != 0 {
		if err := h.Truncate(attr.Size); err != nil {
			return err
		}
	}
	if mask&AttrAlloc != 0 && h.f != nil && attr.Alloc > 0 {
		// 预分配失败不致命：它只是性能优化（AlSi create context）。
		_ = platformPreallocate(h.f, 0, attr.Alloc)
	}

	if mask&(AttrAccessTime|AttrWriteTime) != 0 {
		var at, mt time.Time
		if mask&AttrAccessTime != 0 {
			at = attr.AccessTime
		}
		if mask&AttrWriteTime != 0 {
			mt = attr.WriteTime
		}
		if err := setTimes(h.host, at, mt); err != nil {
			return err
		}
	}
	if mask&AttrCreateTime != 0 && !attr.CreateTime.IsZero() {
		// 优先走 oscap.Times()：builtin 旁路能真实记下创建时间，
		// native 档在支持的文件系统上调用 SetFileTime。
		// 拿不到（ErrNotSupported，典型的 POSIX 宿主无 btime）回落到平台接口；
		// 其它错误按 oscap 错误映射上报（创建时间落不进旁路是真实失败，
		// 不应静默吞掉）。
		ref := oscap.Ref{Path: h.host, Handle: h.f}
		if err := h.fs.caps.Times().SetCreationTime(ref, attr.CreateTime); err != nil {
			if errors.Is(err, oscap.ErrNotSupported) {
				// 平台没有设置 btime 的接口（POSIX），尽力而为，失败忽略
				// （见函数头：无法表达的属性静默忽略，避免 Windows 复制失败）。
				_ = platformSetCreateTime(h.f, h.host, attr.CreateTime)
			} else {
				return mapOscapError(err)
			}
		}
	}

	if mask&AttrFileAttributes != 0 {
		if err := h.setDOSAttributes(attr.FileAttributes); err != nil {
			return err
		}
	}
	if mask&AttrMode != 0 {
		if err := os.Chmod(h.host, os.FileMode(attr.Mode&0o7777)); err != nil {
			return mapError(err)
		}
	}
	// 注意这里必须带上 AttrMode。上面那次 os.Chmod 在 Windows 上只翻
	// READONLY 位（NTFS 没有 POSIX 权限位），真正的 mode 只能落到旁路存储。
	// 旧实现只在 UID/GID 变更时才调，于是客户端**单独**设 mode 时
	// 改动根本没落库，回读的是旧值。
	if mask&(AttrUID|AttrGID|AttrMode) != 0 {
		h.recordPOSIXMetadata(attr, mask)
	}
	return nil
}

// setDOSAttributes 把 FILE_ATTRIBUTE_* 落到宿主机上。
//
// POSIX 上唯一有真实对应物的是 READONLY（写权限位）；
// HIDDEN/SYSTEM/ARCHIVE 等只能忽略。
// TODO: 阶段二可以像 Samba 那样存到 user.DOSATTRIB 扩展属性里，
// 那样 Windows 客户端设置的隐藏属性就能持久化。
func (h *localHandle) setDOSAttributes(attrs uint32) error {
	// 优先走 oscap.DOS()：builtin 旁路真实记下客户端设置的 DOS 位，
	// native 档在 Windows 上写 NTFS 原生属性。
	// 拿不到（ErrNotSupported）回落到平台逻辑；其它错误按 oscap 错误映射上报。
	ref := oscap.Ref{Path: h.host, Handle: h.f}
	if err := h.fs.caps.DOS().SetDOSAttributes(ref, attrs); err != nil {
		if errors.Is(err, oscap.ErrNotSupported) {
			return h.setDOSAttributesPlatform(attrs)
		}
		return mapOscapError(err)
	}
	return nil
}

// setDOSAttributesPlatform 是 caps 不支持 DOS 属性写入时的回落。
//
// POSIX 上唯一有真实对应物的是 READONLY（写权限位），其余位只能忽略——
// 与旧实现一致。Windows 上 platformSetDOSAttributes 直接写 NTFS 原生位。
func (h *localHandle) setDOSAttributesPlatform(attrs uint32) error {
	if err := platformSetDOSAttributes(h.host, attrs); err == nil {
		return nil
	} else if err != ErrNotSupported {
		return err
	}

	fi, err := os.Stat(h.host)
	if err != nil {
		return mapError(err)
	}
	perm := fi.Mode().Perm()
	if attrs&FileAttributeReadonly != 0 {
		perm &^= 0o222
	} else {
		// 取消只读：恢复属主写位。不去猜组/其他人的写位，
		// 保守只加 u+w，避免把文件放宽给别人。
		perm |= 0o200
	}
	if perm == fi.Mode().Perm() {
		return nil
	}
	return mapError(os.Chmod(h.host, perm))
}

// recordPOSIXMetadata 把 uid / gid / mode 记进旁路存储。
//
// 名字里不再叫 setOwner：它一直也在处理 AttrMode，而那个名字让调用点
// 想当然地只在 UID/GID 变更时才调它，纯 mode 变更就被漏掉了。
//
// 注意 AGENTS.md §1.1：这里的 uid/gid 只是数字标签，**不做系统用户解析**，
// 也不调用 chown —— 真去 chown 需要 root，且会把本软件的账户体系和
// 宿主机的账户体系绑在一起，正是准则明令禁止的。
// 表达不了 POSIX 属主/权限的平台（Windows）写进旁路存储，其余平台忽略。
func (h *localHandle) recordPOSIXMetadata(attr *Attr, mask AttrMask) {
	if h.fs.meta == nil {
		return
	}
	rel, err := filepath.Rel(h.fs.res.Root(), h.host)
	if err != nil {
		return
	}
	key := filepath.ToSlash(rel)
	md, _ := h.fs.meta.Get(key)
	if mask&AttrUID != 0 {
		md.UID = attr.UID
	}
	if mask&AttrGID != 0 {
		md.GID = attr.GID
	}
	if mask&AttrMode != 0 {
		md.Mode = attr.Mode
	}
	_ = h.fs.meta.Put(key, md)
}

// ---------------------------------------------------------------- 目录枚举

// ReadDir 实现 Handle。
func (h *localHandle) ReadDir(pattern string, restart bool, max int) ([]DirEntry, error) {
	if !h.isDir {
		return nil, ErrNotDir
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}

	if restart {
		h.dirExactDone = false
	}
	if h.dirExactDone {
		// 快速路径已经把唯一那条吐完了，且没有拍快照可继续。
		return nil, io.EOF
	}
	if h.dirNames == nil || restart {
		if ent, ok := h.lookupExactLocked(pattern); ok {
			h.dirExactDone = true
			return []DirEntry{ent}, nil
		}
		if err := h.snapshotLocked(); err != nil {
			return nil, err
		}
	}

	out := make([]DirEntry, 0, 64)
	for h.dirPos < len(h.dirNames) {
		if max > 0 && len(out) >= max {
			break
		}
		name := h.dirNames[h.dirPos]
		h.dirPos++

		if !MatchDOS(pattern, name) {
			continue
		}
		a, err := h.entryAttr(name)
		if err != nil {
			// 条目在快照之后被删掉了 —— 跳过而不是让整批枚举失败。
			continue
		}
		out = append(out, DirEntry{Name: name, Attr: *a})
	}

	if len(out) == 0 && h.dirPos >= len(h.dirNames) {
		// 枚举完毕。server 层据此回 STATUS_NO_MORE_FILES；
		// 若这是**第一次**调用就没有任何匹配，则应回 STATUS_NO_SUCH_FILE
		// （docs/protocol-notes.md §9），那个区分由 server 层做。
		return nil, io.EOF
	}
	return out, nil
}

// lookupExactLocked 是「模式里没有通配符」时的快速路径：直接 stat 目标名字，
// 不去读整个目录。
//
// 为什么值得专门优化：Time Machine 的 .sparsebundle/bands 目录动辄十万条目，
// 而 macOS 反复问的是「某个 band 在不在」——  即一次带精确文件名的
// QUERY_DIRECTORY。走通用路径的话，每问一次就要 readdirnames 十万个名字
// 再排一次序；实测 5 万条目的目录上单次要 ~23ms，走这条路径降到 ~0.2ms。
// Samba 在 source3/smbd/dir.c 的 dptr_ReadDirName() 里做的是同一个优化
// （非通配模式先直接 stat，命中就不进目录扫描）。
//
// 未命中时返回 false，由调用方退回完整快照路径 —— 这一步不能省：
// SMB 的名字比较是大小写不敏感的（见 dosmatch.go），而宿主机文件系统
// 可能是大小写敏感的，精确 stat miss 不等于「不存在」。
//
// 调用者必须持有 h.mu。
func (h *localHandle) lookupExactLocked(pattern string) (DirEntry, bool) {
	var zero DirEntry
	if pattern == "" || HasWildcard(pattern) {
		return zero, false
	}
	// "." / ".." 的属性来源与普通条目不同（见 entryAttr），
	// 且它们必定在快照里，交给通用路径处理。
	if pattern == "." || pattern == ".." {
		return zero, false
	}
	if ValidateComponent(pattern) != nil {
		return zero, false
	}
	// AppleDouble 旁路文件不作为独立条目出现（与 snapshotLocked 的过滤一致），
	// 否则这条快速路径会把 ._foo 变成「快照里看不见、精确查却查得到」。
	if isDotUnderscoreName(pattern) {
		return zero, false
	}

	a, err := h.fs.statHost(filepath.Join(h.host, pattern), pattern)
	if err != nil {
		return zero, false
	}
	return DirEntry{Name: pattern, Attr: *a}, true
}

// snapshotLocked 拍一张目录内容的快照。
//
// 只读名字不读属性：Time Machine 的 band 目录可以有十万级条目，
// 一次性 lstat 全部会让首个 QUERY_DIRECTORY 卡住好几秒。
// 属性在每一批实际返回时才逐条取。
func (h *localHandle) snapshotLocked() error {
	f, err := openHostFile(h.host, os.O_RDONLY, 0)
	if err != nil {
		return mapError(err)
	}
	defer f.Close()

	names, err := f.Readdirnames(-1)
	if err != nil {
		return mapError(err)
	}
	// 固定顺序：宿主机返回的顺序（ext4 的 htree）是哈希序，
	// 同一目录两次枚举可能不一致，客户端做增量对比时会困惑。
	sort.Strings(names)

	// 过滤掉 SMB 表达不了的名字（含非法字符、Windows 保留设备名等）。
	// 它们即使列出来客户端也打不开（ValidateComponent 会拒），
	// 不如不列，免得出现「看得见摸不着」的条目。
	kept := make([]string, 0, len(names)+2)
	// Windows 期待枚举结果里带 "." 与 ".."（docs/protocol-notes.md §9）。
	kept = append(kept, ".", "..")
	var dotUnder map[string]struct{}
	for _, n := range names {
		// AppleDouble 资源派生旁路文件不作为独立条目出现：
		// 客户端要拿资源派生是通过 AFP_Resource 流，不是通过 ._foo。
		// 列出来会让 Finder 显示重影，也会让 Time Machine 的 band 计数翻倍。
		//
		// 但**要记下来**：readdir_attr 需要知道哪些条目有资源派生，
		// 这里顺手记一笔就免掉后面每条一次的失败 open。
		if isDotUnderscoreName(n) {
			if dotUnder == nil {
				dotUnder = make(map[string]struct{})
			}
			dotUnder[strings.TrimPrefix(n, adoubleNamePrefix)] = struct{}{}
			continue
		}
		if ValidateComponent(n) != nil {
			continue
		}
		kept = append(kept, n)
	}

	h.dirNames = kept
	h.dotUnder = dotUnder
	h.dirPos = 0
	h.dirExactDone = false
	return nil
}

// entryAttr 取单条目录项的属性。
func (h *localHandle) entryAttr(name string) (*Attr, error) {
	switch name {
	case ".":
		return h.fs.statHost(h.host, ".")
	case "..":
		parent := filepath.Dir(h.host)
		// 共享根的 ".." 指向自己：绝不能让客户端顺着它走到共享外。
		if h.host == h.fs.res.Root() || !h.fs.res.contains(parent) {
			parent = h.fs.res.Root()
		}
		return h.fs.statHost(parent, "..")
	default:
		return h.fs.statHost(filepath.Join(h.host, name), name)
	}
}

// ---------------------------------------------------------------- 其他

// Xattr 实现 Handle。
//
// 保留 error 返回值是因为它是跨模块契约（fs.go 的 Handle 接口），但这一侧
// 已经不会失败了：能力在构造 LocalFS 时就选定，宿主没有扩展属性时由
// builtin 适配器兜住，不再是「本平台不支持」。
func (h *localHandle) Xattr() (XattrAccessor, error) {
	return h.fs.xattrAt(h.host, h.f), nil
}

// Close 实现 Handle。
func (h *localHandle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	del := h.deleteOnClose
	f := h.f
	h.f = nil
	h.dirNames = nil
	h.mu.Unlock()

	var firstErr error
	if f != nil {
		if err := f.Close(); err != nil {
			firstErr = mapError(err)
		}
	}
	if del && !h.fs.cfg.ReadOnly {
		// 先 close 再 unlink：Windows 上打开中的文件默认删不掉
		// （Go 的 os.OpenFile 不带 FILE_SHARE_DELETE），
		// 这个顺序在三个平台上都成立。
		if err := os.Remove(h.host); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = mapError(err)
		} else if err == nil {
			h.fs.forgetMetadata(h.host)
		}
	}
	return firstErr
}
