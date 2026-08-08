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
// 对应 SMB2 IOCTL 的 FSCTL_SET_ZERO_DATA / FSCTL_SET_SPARSE。
type SparseFile interface {
	// PunchHole 把 [off, off+length) 释放成空洞，读回来是零。
	// 文件逻辑长度不变。
	PunchHole(off, length int64) error

	// Preallocate 预留 [off, off+length) 的空间但不改变文件逻辑长度
	// （对应 AllocationSize / AlSi create context）。
	Preallocate(off, length int64) error
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

const maxInt64 = int64(^uint64(0) >> 1)
