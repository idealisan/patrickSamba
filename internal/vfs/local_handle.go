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
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
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
		if err := h.f.Sync(); err != nil {
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
func (h *localHandle) Sync(full bool) error {
	if h.f == nil {
		return nil
	}
	if full {
		return mapError(platformFullSync(h.f))
	}
	return mapError(h.f.Sync())
}

// ---------------------------------------------------------------- 属性

// Stat 实现 Handle。
func (h *localHandle) Stat() (*Attr, error) {
	if h.f != nil {
		// 优先 fstat：句柄已经打开，即使文件被改名（甚至已被 unlink，
		// 例如 DELETE_ON_CLOSE 还没触发）也能拿到正确属性。
		if fi, err := h.f.Stat(); err == nil {
			a := attrFromFileInfo(fi, h.name, h.fs.cfg.ReadOnly)
			if bt, ok := statCreateTime(h.host); ok {
				a.CreateTime = bt
			}
			fillSysAttrFromFile(h.f, a)
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
		// POSIX 没有设置 btime 的接口；Windows 有（SetFileTime）。
		// 失败一律忽略，见函数头的说明。
		_ = platformSetCreateTime(h.f, h.host, attr.CreateTime)
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
	if mask&(AttrUID|AttrGID) != 0 {
		h.setOwner(attr, mask)
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

// setOwner 记录属主。
//
// 注意 AGENTS.md §1.1：这里的 uid/gid 只是数字标签，**不做系统用户解析**，
// 也不调用 chown —— 真去 chown 需要 root，且会把本软件的账户体系和
// 宿主机的账户体系绑在一起，正是准则明令禁止的。
// 表达不了 POSIX 属主的平台（Windows）写进旁路存储，其余平台忽略。
func (h *localHandle) setOwner(attr *Attr, mask AttrMask) {
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

	if h.dirNames == nil || restart {
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

// snapshotLocked 拍一张目录内容的快照。
//
// 只读名字不读属性：Time Machine 的 band 目录可以有十万级条目，
// 一次性 lstat 全部会让首个 QUERY_DIRECTORY 卡住好几秒。
// 属性在每一批实际返回时才逐条取。
func (h *localHandle) snapshotLocked() error {
	f, err := os.Open(h.host)
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
	for _, n := range names {
		if ValidateComponent(n) != nil {
			continue
		}
		kept = append(kept, n)
	}

	h.dirNames = kept
	h.dirPos = 0
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
func (h *localHandle) Xattr() (XattrAccessor, error) {
	return newXattrAccessor(h.host, h.f)
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
