package builtin

// stream.go —— CapNamedStream 的 builtin 实现：每个命名流是旁路 KV 里的一条记录。
//
// # 为什么放 KV 而不是放同目录的旁路文件
//
// 旁路文件（`._foo` 那一套）会被客户端**看见**：ls 里多出一堆文件、
// 拷贝目录时被一起带走、删除主文件时留下孤儿。放进共享目录之外的 KV 就没有
// 这些问题，而 macOS 需要的 AFP_Resource / AFP_AfpInfo 本来也不大。
//
// # 一致性边界（必须说清楚）
//
// 句柄里带一份内存副本，**每次写立刻回写 KV**（write-through），Close 只是
// 兜底再刷一次。所以同一个流被两个句柄同时写时，后写的整体覆盖先写的 ——
// 这与 NTFS 的字节级并发语义不同。SMB 层对同一个流的并发写本来就要靠
// oplock/lease 与共享模式约束，这里不再自建一套锁。
// 之所以不做「只在 Close 时落盘」：客户端崩溃或忘记 Close 时数据会静默丢光，
// 而「成功回显 ≠ 事情真的发生了」是本项目反复栽过的坑。

import (
	"io"
	"sync"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// ListStreams 实现 oscap.NamedStream。
func (a *adapter) ListStreams(ref oscap.Ref) ([]oscap.StreamInfo, error) {
	pairs, err := a.st.scanPrefix(bucketStream, a.st.subPrefix(ref.Path))
	if err != nil {
		return nil, err
	}
	var out []oscap.StreamInfo
	for _, p := range pairs {
		if p.Key == "" {
			// 主数据流不归本能力管，理论上也存不进来（OpenStream 拒空名）。
			continue
		}
		n := int64(len(p.Val))
		out = append(out, oscap.StreamInfo{Name: p.Key, Size: n, Alloc: n})
	}
	return out, nil
}

// OpenStream 实现 oscap.NamedStream。
func (a *adapter) OpenStream(ref oscap.Ref, name string, flags oscap.StreamFlags) (oscap.StreamHandle, error) {
	if name == "" {
		return nil, oscap.ErrInvalidArg
	}
	wantWrite := flags&(oscap.StreamWrite|oscap.StreamCreate|oscap.StreamTruncate) != 0
	if wantWrite && a.st.readOnly {
		return nil, oscap.ErrReadOnly
	}

	key := a.st.subKey(ref.Path, name)
	data, ok, err := a.st.get(bucketStream, key)
	if err != nil {
		return nil, err
	}
	if !ok && flags&oscap.StreamCreate == 0 {
		return nil, oscap.ErrNotFound
	}

	switch {
	case !ok:
		// 立刻落一条空记录：不这么做的话，创建后未写入之前 ListStreams
		// 看不到这个流，而客户端已经拿到了成功的 create 应答。
		data = []byte{}
		if err := a.st.put(bucketStream, key, data); err != nil {
			return nil, err
		}
	case flags&oscap.StreamTruncate != 0:
		data = []byte{}
		if err := a.st.put(bucketStream, key, data); err != nil {
			return nil, err
		}
	}

	return &streamHandle{
		st:       a.st,
		key:      key,
		buf:      data,
		writable: wantWrite,
	}, nil
}

// RemoveStream 实现 oscap.NamedStream。
func (a *adapter) RemoveStream(ref oscap.Ref, name string) error {
	if name == "" {
		return oscap.ErrInvalidArg
	}
	existed, err := a.st.del(bucketStream, a.st.subKey(ref.Path, name))
	if err != nil {
		return err
	}
	if !existed {
		return oscap.ErrNotFound
	}
	return nil
}

// streamHandle 是一个已打开的命名流。
type streamHandle struct {
	st  *store
	key []byte

	mu       sync.Mutex
	buf      []byte
	writable bool
	closed   bool
}

var _ oscap.StreamHandle = (*streamHandle)(nil)

// ReadAt 实现 io.ReaderAt。
//
// 严格按 io.ReaderAt 契约：读到末尾且未填满 p 时返回 (n, io.EOF)。
// 上层的 SMB READ 依赖这个约定来判断流尾，含糊不得。
func (h *streamHandle) ReadAt(p []byte, off int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, oscap.ErrClosed
	}
	if off < 0 {
		return 0, oscap.ErrInvalidArg
	}
	if off >= int64(len(h.buf)) {
		return 0, io.EOF
	}
	n := copy(p, h.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt 实现 io.WriterAt。写穿到 KV，见文件头的一致性说明。
func (h *streamHandle) WriteAt(p []byte, off int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, oscap.ErrClosed
	}
	if off < 0 || !addOK(off, int64(len(p))) {
		return 0, oscap.ErrInvalidArg
	}
	if !h.writable {
		return 0, oscap.ErrReadOnly
	}
	if end := off + int64(len(p)); end > int64(len(h.buf)) {
		h.resize(end)
	}
	n := copy(h.buf[off:], p)
	if err := h.flushLocked(); err != nil {
		return 0, err
	}
	return n, nil
}

// Truncate 实现 oscap.StreamHandle。
func (h *streamHandle) Truncate(size int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return oscap.ErrClosed
	}
	if size < 0 {
		return oscap.ErrInvalidArg
	}
	if !h.writable {
		return oscap.ErrReadOnly
	}
	h.resize(size)
	return h.flushLocked()
}

// Size 实现 oscap.StreamHandle。
func (h *streamHandle) Size() (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, oscap.ErrClosed
	}
	return int64(len(h.buf)), nil
}

// Close 实现 io.Closer。重复 Close 返回 ErrClosed（而不是静默成功）。
func (h *streamHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return oscap.ErrClosed
	}
	h.closed = true
	if !h.writable {
		return nil
	}
	return h.flushLocked()
}

// resize 调整缓冲长度，扩张部分补零（稀疏写入的语义）。
func (h *streamHandle) resize(n int64) {
	switch {
	case n <= int64(len(h.buf)):
		h.buf = h.buf[:n]
	default:
		grown := make([]byte, n)
		copy(grown, h.buf)
		h.buf = grown
	}
}

func (h *streamHandle) flushLocked() error {
	return h.st.put(bucketStream, h.key, h.buf)
}
