package vfs

// stream_handle.go —— alternate data stream 的句柄实现。
//
// 两种流的存储形态差别很大，但对 SMB 层必须表现成同一种东西
// （一个可读写、有长度、能 Truncate 的字节流），所以统一封装成 streamHandle：
//
//	AFP_AfpInfo   内存缓冲（固定 60 字节），Close/Flush 时整体回写 xattr
//	AFP_Resource  ._ 旁路文件，所有偏移加上 AppleDouble 头长度
//
// AFP_AfpInfo 用内存缓冲而不是直接映射 xattr 的原因：xattr 只能整体
// 读写，没有「在偏移 16 处写 4 字节」这种语义，而客户端确实会分段写。

import (
	"io"
	"os"
	"sync"
)

// streamHandle 实现 Handle。
type streamHandle struct {
	fs   *LocalFS
	host string // 基础对象的宿主机路径
	name string // 基础对象名（用于 Attr.Name）
	slot string // 规范化后的流名

	mu     sync.Mutex
	closed bool

	writable bool

	// AFP_AfpInfo：内存缓冲 + 脏标记。
	buf   []byte
	dirty bool

	// AFP_Resource：旁路文件 + 数据段起始偏移。
	f       *os.File
	dataOff int64
}

var _ Handle = (*streamHandle)(nil)

// openStream 打开 alternate data stream。
//
// 基础对象必须已存在：SMB 不允许「只创建流不创建文件」，
// 客户端总是先 CREATE 文件本体再 CREATE 它的流。
func (l *LocalFS) openStream(req *OpenRequest, host, name, stream string) (Handle, Action, error) {
	slot := canonicalStreamName(stream)
	if !IsAFPStream(slot) {
		// 通用 ADS 不支持。明确拒绝，不要假装成功（见 stream_store.go 说明）。
		return nil, 0, ErrNotSupported
	}

	fi, err := os.Lstat(host)
	if err != nil {
		return nil, 0, mapError(err)
	}
	if fi.IsDir() {
		// 目录上的 AFP 流：Samba 也只在文件上支持，目录的 FinderInfo
		// 走 ._ 同名文件那套，不在本阶段范围内。
		return nil, 0, ErrNotSupported
	}

	writes := dispositionWrites(req.Disposition, req.Flags)
	if writes && l.cfg.ReadOnly {
		return nil, 0, ErrReadOnly
	}
	writable := writes || req.Flags&OpenWrite != 0

	h := &streamHandle{
		fs: l, host: host, name: name, slot: slot, writable: writable,
	}

	if slot == StreamAFPInfo {
		return h.openAfpInfo(req)
	}
	return h.openResource(req)
}

// openAfpInfo 准备 AFP_AfpInfo 流的内存缓冲。
func (h *streamHandle) openAfpInfo(req *OpenRequest) (Handle, Action, error) {
	ai, err := h.fs.readAfpInfo(h.host)
	exists := err == nil
	if err != nil && err != ErrNotFound {
		if err == ErrNotSupported {
			// 宿主文件系统没有 xattr（比如挂载时关了 user_xattr）。
			// 这不是客户端的错，但也没法支持 —— 明确告知。
			return nil, 0, ErrNotSupported
		}
		return nil, 0, err
	}

	action, err := streamAction(req.Disposition, exists)
	if err != nil {
		return nil, 0, err
	}
	if !exists || action == ActionSuperseded || action == ActionOverwritten {
		ai = NewAfpInfo()
		// 新建/覆盖时立刻落盘一个空流，这样后续的 Streams() 能看到它。
		// 注意 writeAfpInfo 对全零 FinderInfo 是「删除」语义，
		// 所以这里不写 —— 流的存在性由第一次真正的写入建立。
	}

	h.buf = ai.Marshal()
	return h, action, nil
}

// openResource 准备 AFP_Resource 流的旁路文件。
func (h *streamHandle) openResource(req *OpenRequest) (Handle, Action, error) {
	_, exists := resourceForkSize(dotUnderscoreName(h.host))

	action, err := streamAction(req.Disposition, exists)
	if err != nil {
		return nil, 0, err
	}

	create := action == ActionCreated || action == ActionSuperseded
	f, dataOff, err := h.fs.openResourceFork(h.host, h.writable, create || h.writable)
	if err != nil {
		return nil, 0, err
	}
	h.f, h.dataOff = f, dataOff

	if action == ActionSuperseded || action == ActionOverwritten {
		if err := h.truncateLocked(0); err != nil {
			_ = f.Close()
			return nil, 0, err
		}
	}
	return h, action, nil
}

// streamAction 把 CreateDisposition 映射成流上的 Action。
//
// 语义与主数据流一致（MS-SMB2 §2.2.13 / §2.2.14），
// 单独写一份是因为流没有「目录」这条分支，混在一起反而难读。
func streamAction(d Disposition, exists bool) (Action, error) {
	switch d {
	case Supersede:
		if exists {
			return ActionSuperseded, nil
		}
		return ActionCreated, nil
	case OpenExisting:
		if !exists {
			return 0, ErrNotFound
		}
		return ActionOpened, nil
	case CreateNew:
		if exists {
			return 0, ErrExist
		}
		return ActionCreated, nil
	case OpenAlways:
		if exists {
			return ActionOpened, nil
		}
		return ActionCreated, nil
	case TruncateExisting:
		if !exists {
			return 0, ErrNotFound
		}
		return ActionOverwritten, nil
	case TruncateAlways:
		if exists {
			return ActionOverwritten, nil
		}
		return ActionCreated, nil
	}
	return 0, ErrInvalidArg
}

// ---------------------------------------------------------------- IO

func (h *streamHandle) ReadAt(p []byte, off int64) (int, error) {
	if err := checkRange(off, len(p)); err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, ErrClosed
	}

	if h.f == nil {
		// AFP_AfpInfo：从内存缓冲读。
		if off >= int64(len(h.buf)) {
			return 0, io.EOF
		}
		n := copy(p, h.buf[off:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}

	// AFP_Resource：偏移平移到数据段。
	n, err := h.f.ReadAt(p, h.dataOff+off)
	if err != nil && err != io.EOF {
		return n, mapError(err)
	}
	return n, err
}

func (h *streamHandle) WriteAt(p []byte, off int64) (int, error) {
	if err := checkRange(off, len(p)); err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, ErrClosed
	}
	if !h.writable {
		return 0, ErrPermission
	}
	if h.fs.cfg.ReadOnly {
		return 0, ErrReadOnly
	}

	if h.f == nil {
		// AFP_AfpInfo 是**定长**的：写超出 60 字节的部分直接拒绝，
		// 而不是悄悄扩容 —— Samba/Apple 的服务器都按定长处理，
		// 让缓冲变长会写出一个对端解析不了的 blob。
		if off+int64(len(p)) > AfpInfoSize {
			return 0, ErrInvalidArg
		}
		copy(h.buf[off:], p)
		h.dirty = true
		return len(p), nil
	}

	n, err := h.f.WriteAt(p, h.dataOff+off)
	if err != nil {
		return n, mapError(err)
	}
	if err := h.syncResourceLen(); err != nil {
		return n, err
	}
	return n, nil
}

// syncResourceLen 把资源段的当前长度回写进 AppleDouble 头。
func (h *streamHandle) syncResourceLen() error {
	fi, err := h.f.Stat()
	if err != nil {
		return mapError(err)
	}
	n := fi.Size() - h.dataOff
	if n < 0 {
		n = 0
	}
	return updateResourceForkLen(h.f, h.dataOff, n)
}

func (h *streamHandle) Truncate(size int64) error {
	if size < 0 {
		return ErrInvalidArg
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	if !h.writable {
		return ErrPermission
	}
	if h.fs.cfg.ReadOnly {
		return ErrReadOnly
	}
	return h.truncateLocked(size)
}

func (h *streamHandle) truncateLocked(size int64) error {
	if h.f == nil {
		// AFP_AfpInfo 定长，只接受截到 0（等价于清空 FinderInfo）
		// 或者截到 60（无操作）。
		switch size {
		case 0:
			h.buf = NewAfpInfo().Marshal()
			h.dirty = true
			return nil
		case AfpInfoSize:
			return nil
		}
		return ErrInvalidArg
	}
	if err := h.f.Truncate(h.dataOff + size); err != nil {
		return mapError(err)
	}
	return updateResourceForkLen(h.f, h.dataOff, size)
}

func (h *streamHandle) Sync(full bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	if err := h.flushLocked(); err != nil {
		return err
	}
	if h.f == nil {
		// AFP_AfpInfo 落在 xattr 上，flushLocked 已经写下去了；
		// xattr 的持久化跟随基础文件的元数据，没有独立的 fsync 通道。
		return nil
	}
	if full {
		return mapError(platformFullSync(h.f))
	}
	return mapError(h.f.Sync())
}

// flushLocked 把 AFP_AfpInfo 的内存缓冲回写到 xattr。
func (h *streamHandle) flushLocked() error {
	if h.f != nil || !h.dirty {
		return nil
	}
	ai, err := ParseAfpInfo(h.buf)
	if err != nil {
		// 客户端写进来的内容不是合法 AfpInfo。Samba 在这种情况下拒绝写入，
		// 但我们已经把数据收下了，只能在落盘时拒绝并保留磁盘上的旧值。
		return ErrBadAfpInfo
	}
	if err := h.fs.writeAfpInfo(h.host, ai); err != nil {
		return err
	}
	h.dirty = false
	return nil
}

// ---------------------------------------------------------------- 属性

func (h *streamHandle) Stat() (*Attr, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}

	// 流的属性基本沿用基础对象（时间戳、属主、DOS 位都取基础对象的），
	// 只有 Size/Alloc 换成流自己的长度 —— 这是 Windows 的行为。
	a, err := h.fs.statHost(h.host, h.name)
	if err != nil {
		return nil, err
	}
	if h.f == nil {
		a.Size = AfpInfoSize
	} else {
		fi, err := h.f.Stat()
		if err != nil {
			return nil, mapError(err)
		}
		if n := fi.Size() - h.dataOff; n > 0 {
			a.Size = n
		} else {
			a.Size = 0
		}
	}
	a.Alloc = allocSizeFallback(a.Size)
	return a, nil
}

// SetAttr 在流句柄上是无操作。
//
// 属性属于基础对象，客户端在流句柄上设属性时 Windows 的行为是接受并
// 应用到基础对象；这里选择静默忽略，理由与主数据流的 SetAttr 一致 ——
// 报错会让 Finder/Explorer 的复制操作整体失败。
func (h *streamHandle) SetAttr(*Attr, AttrMask) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	return nil
}

// ReadDir 在流句柄上永远无效：流不是目录。
func (h *streamHandle) ReadDir(string, bool, int) ([]DirEntry, error) {
	return nil, ErrNotDir
}

// Xattr 返回基础对象的扩展属性访问器。
func (h *streamHandle) Xattr() (XattrAccessor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	return newXattrAccessor(h.host, nil)
}

func (h *streamHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true

	err := h.flushLocked()
	if h.f != nil {
		if cerr := h.f.Close(); err == nil {
			err = mapError(cerr)
		}
		// 资源段为空的 ._ 文件是垃圾，删掉 —— 留着会在目录里堆积，
		// 也会让 macOS 认为每个文件都有资源派生。
		h.cleanupEmptyResource()
	}
	return err
}

// cleanupEmptyResource 删除资源段为空的 ._ 旁路文件。
func (h *streamHandle) cleanupEmptyResource() {
	if h.fs.cfg.ReadOnly {
		return
	}
	adPath := dotUnderscoreName(h.host)
	if _, ok := resourceForkSize(adPath); ok {
		return // 还有内容，留着
	}
	fi, err := os.Lstat(adPath)
	if err != nil || fi.Size() > adRsrcHeaderSize {
		// 读不到，或者比一个纯头还大（可能含 FinderInfo 之外的东西），
		// 保守起见不删。
		return
	}
	_ = os.Remove(adPath)
}
