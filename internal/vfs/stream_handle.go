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

// streamKind 区分三种存储形态。
//
// 用显式枚举而不是「f == nil 就是缓冲模式」那种隐式判定：加进通用流
// 之后有**两种**缓冲模式（定长的 AfpInfo 与变长的 xattr 流），
// 靠 f 是否为 nil 已经分不开了。
type streamKind uint8

const (
	// streamKindAfpInfo：定长 60 字节，落 netatalk metadata xattr。
	streamKindAfpInfo streamKind = iota
	// streamKindResource：变长，落 ._ 旁路文件。
	streamKindResource
	// streamKindXattr：变长，落 user.DosStream.<名>:$DATA xattr。
	streamKindXattr
)

// streamHandle 实现 Handle。
type streamHandle struct {
	fs   *LocalFS
	host string // 基础对象的宿主机路径
	name string // 基础对象名（用于 Attr.Name）
	slot string // 规范化后的流名
	kind streamKind

	mu     sync.Mutex
	closed bool

	writable bool

	// deleteOnClose 是 CREATE 时的 FILE_DELETE_ON_CLOSE（OpenDeleteOnClose）。
	// 提交动作在 Close 里做，粒度是**这一个流**而不是基础文件
	// （Samba streams_xattr_unlinkat() 语义，见 StreamRemover 注释）。
	deleteOnClose bool

	// 缓冲模式（AfpInfo / 通用 xattr 流）：内存缓冲 + 脏标记。
	buf   []byte
	dirty bool

	// AFP_Resource：旁路文件 + 数据段起始偏移。
	f       *os.File
	dataOff int64
}

var _ Handle = (*streamHandle)(nil)

// openStream 打开 alternate data stream。
//
// 基础对象通常已存在（客户端总是先 CREATE 文件本体再 CREATE 它的流），
// 但**创建语义**的 disposition（Supersede/CreateNew/OpenIf/OverwriteIf）
// 在它不存在时会先把基础文件建出来 —— Samba 对流路径正是先以
// FILE_OPEN_IF 打开基础文件（source3/smbd/open.c:6508 附近，注释原话
// "We may be creating the basefile as part of creating the stream"）。
// FILE_OPEN / FILE_OVERWRITE 保持「不存在即 ErrNotFound」且不建。
func (l *LocalFS) openStream(req *OpenRequest, host, name, stream string) (Handle, Action, error) {
	slot := canonicalStreamName(stream)

	fi, err := os.Lstat(host)
	if err != nil && os.IsNotExist(err) {
		switch {
		case req.Disposition == OpenExisting || req.Disposition == TruncateExisting:
			// 非创建语义：如实报不存在。（有意与 Samba 的差异：
			// 它对 FILE_OVERWRITE 也用 OPEN_IF 建基础文件，那会让一次
			// 注定失败的流打开留下一个空的残留文件。）
			return nil, 0, ErrNotFound
		case l.cfg.ReadOnly:
			// 只读共享上不可能走到这里（入口已按写意图拒绝），
			// 防御分支保持旧行为。
			return nil, 0, ErrNotFound
		default:
			if err := l.createBaseForStream(host); err != nil {
				return nil, 0, err
			}
			if fi, err = os.Lstat(host); err != nil {
				return nil, 0, mapError(err)
			}
		}
	}
	if err != nil {
		return nil, 0, mapError(err)
	}
	isDir := fi.IsDir()

	// 决定这个流用哪种后端。目录的可用范围比文件窄，见下面各分支。
	var kind streamKind
	switch {
	case slot == StreamAFPInfo:
		// AFP_AfpInfo 落 netatalk xattr，**目录上同样可用**。
		// 依据：Samba fruit_open_meta_netatalk()（vfs_fruit.c:1455）
		// 与 fruit_streaminfo_meta_netatalk()（同文件 :3859）都没有
		// 目录判断 —— xattr 本来就能挂在目录上。
		// 这条对 Time Machine 是必需的：.sparsebundle 是**目录**，
		// macOS 会往它上面设 com.apple.FinderInfo。
		kind = streamKindAfpInfo
	case slot == StreamAFPResource:
		if isDir {
			// 目录没有资源派生。依据：Samba fruit_open_rsrc_adouble()
			// （vfs_fruit.c:1561）对目录直接 errno=ENOENT，注释原话
			// "sorry, but directories don't have a resource fork"；
			// fruit_streaminfo_rsrc()（:4047）也对目录一条都不列。
			//
			// 回 ErrNotFound 而不是 ErrNotSupported：客户端探测一个
			// 不存在的流时期待的就是 OBJECT_NAME_NOT_FOUND，
			// NOT_SUPPORTED 会让它以为整个共享不支持 ADS。
			return nil, 0, ErrNotFound
		}
		kind = streamKindResource
	default:
		// 通用 named stream → xattr。目录同样可用（Samba 的
		// vfs_streams_xattr 也没有目录判断）。
		if err := validateDosStreamName(slot); err != nil {
			return nil, 0, err
		}
		// bh5 F10：共享配置为大小写不敏感时，折到磁盘上的真实拼写再开。
		// 必须在存在性判定**之前**解析 —— 否则 OpenIf 会把大小写回访
		// 误判成「新建」，凭空多出一条流；CreateNew 也拦不住与既有流
		// 只差大小写的冲突。精确命中优先，见 resolveDosStreamName。
		if real, ok := l.resolveDosStreamName(host, slot); ok {
			slot = real
		}
		kind = streamKindXattr
	}

	writes := dispositionWrites(req.Disposition, req.Flags)
	if writes && l.cfg.ReadOnly {
		return nil, 0, ErrReadOnly
	}
	writable := writes || req.Flags&OpenWrite != 0

	h := &streamHandle{
		fs: l, host: host, name: name, slot: slot, kind: kind, writable: writable,
		deleteOnClose: req.Flags&OpenDeleteOnClose != 0,
	}

	switch kind {
	case streamKindAfpInfo:
		return h.openAfpInfo(req)
	case streamKindXattr:
		return h.openXattrStream(req)
	default:
		return h.openResource(req)
	}
}

// createBaseForStream 在开流之前建出一个空的宿主机基础文件。
//
// O_EXCL 让「输给并发的创建者」成为可识别事件：对方建好了就直接用，
// 不算错误。
func (l *LocalFS) createBaseForStream(host string) error {
	f, err := openHostFile(host, os.O_CREATE|os.O_EXCL|os.O_WRONLY|openNoFollow, l.cfg.FileMode)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return mapError(err)
	}
	return mapError(f.Close())
}

// openXattrStream 准备通用 named stream 的内存缓冲。
//
// 与 AFP_Resource 不同，这里整条流一次性读进内存：xattr 本来就只能
// 整体读写，没有「在偏移 N 处读 M 字节」的语义，而通用流受
// maxDosStreamSize 约束，最大 64 KiB，全读进来是可以接受的。
func (h *streamHandle) openXattrStream(req *OpenRequest) (Handle, Action, error) {
	data, err := h.fs.readDosStream(h.host, h.slot)
	exists := err == nil
	if err != nil && err != ErrNotFound {
		// ErrNotSupported（宿主机没开 user_xattr）如实上报：
		// 这不是客户端的错，但也确实支持不了。
		return nil, 0, err
	}

	action, err := streamAction(req.Disposition, exists)
	if err != nil {
		return nil, 0, err
	}
	if !exists || action == ActionSuperseded || action == ActionOverwritten {
		data = nil
	}

	h.buf = data
	if action == ActionCreated || action == ActionSuperseded || action == ActionOverwritten {
		// 立刻落一个空流，这样后续的 Streams() 能看到它 ——
		// 客户端 CREATE 完一个流之后就认为它存在了，哪怕还没写内容。
		if err := h.fs.writeDosStream(h.host, h.slot, h.buf); err != nil {
			return nil, 0, err
		}
	}
	return h, action, nil
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

	if h.kind != streamKindResource {
		// 缓冲模式（AfpInfo / 通用 xattr 流）：从内存缓冲读。
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

	if h.kind == streamKindXattr {
		// 通用流是**变长**的：按需扩容缓冲，Close/Sync 时整体回写 xattr。
		end := off + int64(len(p))
		if end > maxDosStreamSize {
			// 超过 xattr 能装下的量。如实报错而不是截断 ——
			// 截断会让客户端以为整条流都写进去了。
			return 0, ErrTooLarge
		}
		if end > int64(len(h.buf)) {
			grown := make([]byte, end)
			copy(grown, h.buf)
			h.buf = grown
		}
		copy(h.buf[off:], p)
		h.dirty = true
		return len(p), nil
	}

	if h.kind == streamKindAfpInfo {
		// AFP_AfpInfo 是**定长**的：写超出 60 字节的部分直接拒绝，
		// 而不是悄悄扩容 —— Samba/Apple 的服务器都按定长处理，
		// 让缓冲变长会写出一个对端解析不了的 blob。
		if off+int64(len(p)) > AfpInfoSize {
			return 0, ErrInvalidArg
		}

		// 整块写（客户端的常规写法就是 offset=0、长度 60）**当场校验**。
		//
		// 为什么不能等到 flush：校验错误只能从 Sync/Close 返回，而
		// SMB2 CLOSE 响应没有地方承载它 —— 于是一次非法写入会表现为
		// 「WRITE 成功 → CLOSE 成功 → xattr 根本没建 → 后续 GET 报
		// OBJECT_NAME_NOT_FOUND」，数据无声蒸发，极难排查。
		// 写时报错才能让 SMB2 WRITE 当场回 STATUS_INVALID_PARAMETER。
		//
		// 对齐 Samba 的 fruit_pwrite_meta_stream：它只接受
		// offset==0 && length==60 的写，其余一律 EINVAL。我们比它宽松，
		// 保留分段写的缓冲能力（见下方），但只要某次写覆盖了完整的
		// [0,60) 区间就必须立刻验。
		if off == 0 {
			switch {
			case int64(len(p)) == AfpInfoSize:
				if _, err := ParseAfpInfo(p); err != nil {
					// 缓冲保持原值不动：一次非法写入不该破坏磁盘上的旧
					// FinderInfo，也不该污染后续的合法分段写。
					return 0, ErrBadAfpInfo
				}
			case len(p) < len(afpSigPrefix):
				// Samba fruit_pwrite_meta：`n < 3` 直接 EINVAL。
				// 连签名都放不下的写一定是垃圾，早拒早好 ——
				// 放进缓冲的话签名会被截断，最后在 flush 里失败，
				// 而那个错误 CLOSE 承载不了，表现成数据无声蒸发。
				return 0, ErrInvalidArg
			case string(p[:len(afpSigPrefix)]) != afpSigPrefix:
				// 同上，Samba 在 memcmp(data, "AFP", 3) 处就拦掉。
				// 分段写的第一段必然带签名，所以这一检查不会误伤。
				return 0, ErrBadAfpInfo
			}
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
	switch h.kind {
	case streamKindXattr:
		// 通用流变长，任意长度都合法（受 xattr 上限约束）。
		if size > maxDosStreamSize {
			return ErrTooLarge
		}
		grown := make([]byte, size)
		copy(grown, h.buf)
		h.buf = grown
		h.dirty = true
		return nil

	case streamKindAfpInfo:
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
	if h.kind != streamKindResource {
		// 缓冲模式的流落在 xattr 上，flushLocked 已经写下去了；
		// xattr 的持久化跟随基础文件的元数据，没有独立的 fsync 通道。
		return nil
	}
	if full {
		return mapError(platformFullSync(h.f))
	}
	return mapError(h.f.Sync())
}

// flushLocked 把缓冲模式的流内容回写到 xattr。
func (h *streamHandle) flushLocked() error {
	if h.kind == streamKindResource || !h.dirty {
		return nil
	}

	if h.kind == streamKindXattr {
		if err := h.fs.writeDosStream(h.host, h.slot, h.buf); err != nil {
			return err
		}
		h.dirty = false
		return nil
	}

	ai, err := ParseAfpInfo(h.buf)
	if err != nil {
		// 客户端写进来的内容不是合法 AfpInfo。整块写在 WriteAt 就被拦了，
		// 走到这里的只可能是分段写拼出来的非法内容 —— 拒绝落盘并保留
		// 磁盘上的旧值。SMB2 FLUSH 能承载这个错误，CLOSE 不能。
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
	switch h.kind {
	case streamKindAfpInfo:
		// 定长，恒为 60。
		a.Size = AfpInfoSize
	case streamKindXattr:
		a.Size = int64(len(h.buf))
	default:
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

	// 流不是目录，即使基础对象是目录也不能带 DIRECTORY 位 ——
	// 客户端看到一个「是目录」的流会拿它去做 QUERY_DIRECTORY。
	// .sparsebundle 上的 com.apple.FinderInfo 正是这种情况。
	a.FileAttributes &^= FileAttributeDirectory
	if a.FileAttributes == 0 {
		a.FileAttributes = FileAttributeArchive
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
	return h.fs.xattrAt(h.host, nil), nil
}

func (h *streamHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true

	// FILE_DELETE_ON_CLOSE 落在流句柄上：只删这一个流自己的存储，
	// 基础文件与其余流不动。此时缓冲内容不必再回写 —— 马上就要删了。
	del := h.deleteOnClose && !h.fs.cfg.ReadOnly

	var err error
	if del {
		err = h.fs.removeStreamStorage(h.host, h.slot)
	} else {
		err = h.flushLocked()
	}
	if h.f != nil {
		if cerr := h.f.Close(); err == nil {
			err = mapError(cerr)
		}
		if !del {
			// 资源段为空的 ._ 文件是垃圾，删掉 —— 留着会在目录里堆积，
			// 也会让 macOS 认为每个文件都有资源派生。
			h.cleanupEmptyResource()
		}
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
