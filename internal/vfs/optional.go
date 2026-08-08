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

var (
	_ DeleteOnCloser = (*localHandle)(nil)
	_ SparseFile     = (*localHandle)(nil)
)

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
