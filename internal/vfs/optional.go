package vfs

// optional.go —— **可选能力接口**。
//
// 为什么不直接加进 fs.go 的 Handle/FileSystem：那两个接口是跨模块契约
// （AGENTS.md §7.3），每加一个方法所有实现都得跟着改。而这里的能力
// 是「后端支持就用，不支持就退化」的性质，用可选接口 + 类型断言表达更合适：
//
//	if d, ok := handle.(vfs.DeleteOnCloser); ok {
//	    err = d.SetDeleteOnClose(true)
//	} else {
//	    err = vfs.ErrNotSupported
//	}
//
// 这也是 Go 标准库的惯用法（io.ReaderFrom / http.Flusher 都是这个套路）。

import "os"

// DeleteOnCloser 让已打开的句柄可以**事后**被标记为「关闭时删除」。
//
// 对应 SMB2 SET_INFO 的 FileDispositionInformation（MS-FSCC §2.4.11）：
// 客户端删文件的标准流程是 CREATE(带 DELETE 权限) → SET_INFO(DeletePending=1)
// → CLOSE，而不是发一个「删除」命令。CREATE 时的 FILE_DELETE_ON_CLOSE
// 只是另一条路径。
type DeleteOnCloser interface {
	SetDeleteOnClose(del bool) error
}

// SparseFile 是稀疏文件能力。
//
// Time Machine 的 .sparsebundle 由大量固定大小的 band 文件组成，
// 备份过期回收时会把 band 里的区间置零。没有打洞能力的话，
// 备份卷只会越来越大、永远不会缩小（AGENTS.md §2 阶段二）。
//
// 对应 SMB2 IOCTL 的 FSCTL_SET_ZERO_DATA / FSCTL_SET_SPARSE /
// FSCTL_QUERY_ALLOCATED_RANGES。
type SparseFile interface {
	// PunchHole 把 [off, off+length) 释放成空洞，读回来是零。
	// 文件逻辑长度不变。
	PunchHole(off, length int64) error

	// Preallocate 预留 [off, off+length) 的空间但不改变文件逻辑长度
	// （对应 AllocationSize / AlSi create context）。
	Preallocate(off, length int64) error

	// AllocatedRanges 返回 [off, off+length) 内实际已分配（非空洞）的子区间，
	// 对应 FSCTL_QUERY_ALLOCATED_RANGES（MS-FSCC §2.3.20/§2.3.21）。
	//
	// 保证：按 Offset 升序、互不重叠、已裁剪到查询窗口且不越过 EOF。
	// 查询窗口完全落在 EOF 之外，或窗口内全是空洞时，返回 nil, nil ——
	// **空结果是合法答案**，不是错误。
	//
	// 宿主文件系统不支持空洞探测（tmpfs / overlayfs / 部分网络文件系统）
	// 时降级为「整个窗口都已分配」，而不是报错：多报已分配是安全的
	// （客户端最多多读一遍零），少报会让客户端以为数据丢了。
	AllocatedRanges(off, length int64) ([]Range, error)

	// SetSparse 标记/取消稀疏文件（FSCTL_SET_SPARSE）。
	//
	// POSIX 上文件天然稀疏、没有这个开关，实现为无操作返回 nil ——
	// 返回 ErrNotSupported 会让 macOS 在建 .sparsebundle 时直接放弃。
	SetSparse(v bool) error
}

// Range 是文件内的一段区间 [Offset, Offset+Length)。
type Range struct {
	Offset int64
	Length int64
}

// HardLinker 是硬链接能力，对应 SMB2 SET_INFO 的 FileLinkInformation
// （MS-FSCC §2.4.21.2）。
//
// 由 FileSystem 实现（链接是路径级操作，不是句柄级）：
//
//	if hl, ok := fs.(vfs.HardLinker); ok {
//	    err = hl.Link(oldPath, newPath, info.ReplaceIfExists)
//	}
type HardLinker interface {
	// Link 在 newPath 处创建一个指向 oldPath 的硬链接。
	//
	// 两个路径都会过 Resolver 做根目录约束校验（AGENTS.md §8）。
	// replace 对应 ReplaceIfExists：目标已存在时先删再建。
	//
	// 错误：目标已存在且 replace=false → ErrExist；源是目录 → ErrIsDir
	// （POSIX 与 Windows 都不允许目录硬链接）；宿主文件系统不支持硬链接
	// 或跨设备 → ErrNotSupported。
	Link(oldPath, newPath string, replace bool) error
}

// AppleMetadata 一次性取出 AAPL readdir_attr 需要的 Apple 元数据。
//
// 为什么要有这个：macOS 的 AAPL create context 协商成功后，
// QUERY_DIRECTORY 的每一条目录项都可以顺带携带资源派生大小与 FinderInfo
// （Samba `source3/lib/readdir_attr.h` 的 `struct aapl`）。
// 没有它，Finder 为了拿到同样的信息要对**每个文件**额外开两次流，
// 在 Time Machine 的十万级 band 目录上是灾难性的往返次数。
//
// 由 FileSystem 实现，用类型断言取：
//
//	if am, ok := fs.(vfs.AppleMetadata); ok {
//	    fi, rsrc, err := am.AppleInfo(name)
//	}
type AppleMetadata interface {
	// AppleInfo 返回对象的 32 字节 FinderInfo 与资源派生大小。
	//
	// 对象存在但没有 Apple 元数据时返回全零 FinderInfo、大小 0 与 nil error
	// ——「没有 FinderInfo」是正常状态，不是错误。
	//
	// 注意 wire 层拼 readdir_attr 时只用得到 FinderInfo 的一个**子集**
	// （前 8 字节的类型/创建者码 + 偏移 8 起的标志位，且类型/创建者
	// 仅对普通文件填充，目录留零 —— 见 vfs_fruit.c 的
	// readdir_attr_meta_finderi）。这里返回完整 32 字节，切片由 wire 决定。
	AppleInfo(path string) (finderInfo [FinderInfoSize]byte, rsrcSize int64, err error)
}

var (
	_ DeleteOnCloser = (*localHandle)(nil)
	_ SparseFile     = (*localHandle)(nil)
	_ AppleMetadata  = (*LocalFS)(nil)
	_ HardLinker     = (*LocalFS)(nil)
)

// AppleInfo 实现 AppleMetadata。
func (l *LocalFS) AppleInfo(p string) ([FinderInfoSize]byte, int64, error) {
	var fi [FinderInfoSize]byte

	base, stream, err := SplitStreamPath(p)
	if err != nil {
		return fi, 0, err
	}
	if stream != "" {
		// 对一个流查询 Apple 元数据是没有意义的请求。
		return fi, 0, ErrInvalidPath
	}
	host, err := l.res.Resolve(base)
	if err != nil {
		return fi, 0, err
	}
	if _, err := os.Lstat(host); err != nil {
		return fi, 0, mapError(err)
	}

	// 两者都是「没有就算了」：缺 FinderInfo 或缺资源派生都是正常状态。
	if ai, err := l.readAfpInfo(host); err == nil {
		fi = ai.FinderInfo
	}
	rsrc, _ := resourceForkSize(dotUnderscoreName(host))
	return fi, rsrc, nil
}

// SetDeleteOnClose 实现 DeleteOnCloser。
func (h *localHandle) SetDeleteOnClose(del bool) error {
	if h.fs.cfg.ReadOnly {
		return ErrReadOnly
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	h.deleteOnClose = del
	return nil
}

// PunchHole 实现 SparseFile。
func (h *localHandle) PunchHole(off, length int64) error {
	if err := checkRange(off, 0); err != nil || length < 0 {
		return ErrInvalidArg
	}
	if length > 0 && off > maxInt64-length {
		return ErrInvalidArg
	}
	if err := h.checkWritable(); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	return platformPunchHole(h.f, off, length)
}

// Preallocate 实现 SparseFile。
func (h *localHandle) Preallocate(off, length int64) error {
	if off < 0 || length < 0 || (length > 0 && off > maxInt64-length) {
		return ErrInvalidArg
	}
	if err := h.checkWritable(); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	return platformPreallocate(h.f, off, length)
}

// AllocatedRanges 实现 SparseFile。
//
// 只需要读权限：查询空洞分布不修改文件，Time Machine 在只读挂载上
// 也会问（Finder 算「实际占用」）。
func (h *localHandle) AllocatedRanges(off, length int64) ([]Range, error) {
	if off < 0 || length < 0 || (length > 0 && off > maxInt64-length) {
		return nil, ErrInvalidArg
	}
	if h.isDir {
		return nil, ErrIsDir
	}
	f, err := h.dataFile()
	if err != nil {
		return nil, err
	}
	if length == 0 {
		return nil, nil
	}

	// 先夹到 EOF：客户端问的窗口经常直接给 [0, 0x7FFFFFFFFFFFFFFF)，
	// 逐个 lseek 到那么远毫无意义，而且返回超出 EOF 的区间会让
	// macOS 认为文件比实际更大。
	fi, err := f.Stat()
	if err != nil {
		return nil, mapError(err)
	}
	end := off + length
	if end > fi.Size() {
		end = fi.Size()
	}
	if off >= end {
		return nil, nil
	}
	return platformAllocatedRanges(f, off, end)
}

// SetSparse 实现 SparseFile。
func (h *localHandle) SetSparse(v bool) error {
	if err := h.checkWritable(); err != nil {
		return err
	}
	return platformSetSparse(h.f, v)
}

// dataFile 取出句柄背后的 *os.File，顺带做关闭检查。
//
// 直接读 h.f 是有竞态的：Close 会在持锁的情况下把它置 nil。
func (h *localHandle) dataFile() (*os.File, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	if h.f == nil {
		// OpenAttrOnly 句柄没有数据流。
		return nil, ErrNotSupported
	}
	return h.f, nil
}

// wholeRange 是空洞探测不可用时的保守答案：整个查询窗口都算已分配。
func wholeRange(off, end int64) []Range {
	if off >= end {
		return nil
	}
	return []Range{{Offset: off, Length: end - off}}
}

const maxInt64 = int64(^uint64(0) >> 1)
