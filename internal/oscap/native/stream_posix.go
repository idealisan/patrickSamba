//go:build linux || darwin

package native

// stream_posix.go —— oscap.NamedStream 的原生实现：把命名流承载在扩展属性里。
//
// # 为什么是 xattr
//
// POSIX 没有 alternate data stream 这个东西，但**有**扩展属性，而扩展属性
// 会随文件一起被 rename/unlink 带走 —— 不会在目录里留下客户端看得见的垃圾
// 条目，也不会让 Time Machine 的 band 计数出错。Samba 的 vfs_streams_xattr
// 与 Netatalk 都是这么做的，probe_{linux,darwin}.go 也是照这个前提把
// CapNamedStream 的探测直接接到 probeXattr 上的。
//
// 代价是受 xattr 值大小上限约束（ext4/xfs 单值 64 KiB），所以本实现给出
// 一个**协议层硬上限**并如实报错，而不是让客户端以为写成功了。
//
// # 落盘格式（与 Samba vfs_streams_xattr 二进制兼容）
//
//	xattr 名   user.DosStream.<流名>:$DATA
//	xattr 值   <流数据><1 字节 marker>
//
// 出处：`source3/include/smb.h` 的 SAMBA_XATTR_DOSSTREAM_PREFIX = "user.DosStream."，
// 以及 `streams_xattr_get_name()` 在 store_stream_type（默认开）时拼上的
// `:$DATA` 后缀。末尾 marker 字节出自 vfs_streams_xattr.c 顶部的设计注释：
// xattr 的值不能为空，所以零长度的流也要占一个字节。
//
// **只支持 marker==0 的单 xattr 形态**。Samba 用非零 marker 表示「还有几个
// 续存 xattr」（把超大流拆开绕过大小上限）。读到非零一律 ErrNotSupported ——
// 那是我们读不全的数据，谎称读到了只会让客户端拿到截断的内容。
//
// 这份格式与 internal/vfs/stream_xattr.go 完全一致，因此 vfs 切换到 oscap
// 之后，磁盘上已有的流仍然读得出来。
//
// # 与 AFP_AfpInfo / AFP_Resource 的分工
//
// 那两个流在 macOS 上另有约定俗成的落点（Netatalk 的 metadata blob、
// com.apple.ResourceFork），映射属于 **Apple 兼容层（vfs/fruit）的策略**，
// 不归 oscap 管：本包只回答「这个对象上有哪些附加数据流」。

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

const (
	// streamXattrSuffix 是拼进 xattr 名的流类型后缀，见文件头。
	streamXattrSuffix = ":$DATA"

	// maxStreamSize 是单个命名流的字节上限。
	//
	// ext4/xfs 的单个 xattr 值上限是 64 KiB，且实际能用多少还取决于 inode
	// 里的剩余空间。这里取 64 KiB 作为协议层硬上限并如实报 ENOSPC，
	// 让客户端知道写不下 —— 而不是写一半、或者悄悄拆成它看不见的多个片段。
	maxStreamSize = 64 * 1024

	// maxStreamNameLen 是流名的最大字节数（算出来是 234）。
	//
	// **总是按最长的平台前缀（Linux 的 "user."）算预算**，即使 macOS 上
	// 前缀为空。理由：同一份数据可能在平台之间迁移，各平台上限不同会造成
	// 「在 macOS 上写得进去的流名，拷到 Linux 就突然写不进去」这种极难排查
	// 的行为。宁可在 macOS 上严一点。
	maxStreamNameLen = maxXattrNameLen - len("user.") -
		len(reservedStreamPrefix) - len(streamXattrSuffix)
)

// streamMaxBytes 是 maxStreamSize 的可覆盖副本，仅供测试把上限调小，
// 从而不依赖宿主文件系统的真实 xattr 值上限来证伪「超限必须被拒」
// （overlayfs 的上限可能远低于 64 KiB，否则超限写入会被文件系统本身挡掉，
// 掩盖「代码自己的上限检查」有没有接线）。生产路径永远走默认值。
var streamMaxBytes = maxStreamSize

// posixStreams 实现 oscap.NamedStream。
type posixStreams struct {
	readOnly bool
}

var _ oscap.NamedStream = (*posixStreams)(nil)

// streamXattrName 把流名映射成**宿主机**扩展属性名，并做安全校验。
//
// 字符层面的校验（分隔符、控制字符）由跨平台的 validateStreamName 统一做，
// 见 native.go 的说明；这里只补 xattr 特有的长度预算。
func streamXattrName(name string) (string, error) {
	if err := validateStreamName(name); err != nil {
		return "", err
	}
	if len(name) > maxStreamNameLen {
		// 不截断，如实拒绝：截断会让两个不同的流名映射到同一个 xattr，
		// 后写的把先写的悄悄覆盖掉。
		return "", fmt.Errorf("oscap/native: 命名流名过长（%d > %d 字节）: %w",
			len(name), maxStreamNameLen, oscap.ErrInvalidArg)
	}
	return xattrNamespace + reservedStreamPrefix + name + streamXattrSuffix, nil
}

// streamNameFromXattr 是逆映射，第二个返回值表示这个属性是不是一个命名流。
func streamNameFromXattr(raw string) (string, bool) {
	prefix := xattrNamespace + reservedStreamPrefix
	if !strings.HasPrefix(raw, prefix) {
		return "", false
	}
	// 后缀可以缺省：Samba 在 store_stream_type=false 时不写它。
	// 两种都认，我们自己写出去时统一带后缀。
	name := strings.TrimSuffix(raw[len(prefix):], streamXattrSuffix)
	if name == "" {
		return "", false
	}
	return name, true
}

func (s *posixStreams) ListStreams(ref oscap.Ref) ([]oscap.StreamInfo, error) {
	raw, err := listXattrRaw(ref)
	if err != nil {
		return nil, err
	}
	var out []oscap.StreamInfo
	for _, r := range raw {
		name, ok := streamNameFromXattr(r)
		if !ok {
			continue
		}
		// 只问长度不取值：目录枚举会对每个条目调一次，把几十 KiB 的资源叉
		// 整个读进来只为算个长度是划不来的。
		size, err := xattrGet(ref, r, nil)
		if err != nil {
			// 两次调用之间被别人删了 —— 跳过而不是让整次枚举失败。
			continue
		}
		// 减掉 marker 字节。理论上 size 至少为 1（我们写的一定带 marker），
		// 别的工具写出的空值按 0 处理，不要算出 -1。
		if size > 0 {
			size--
		}
		out = append(out, oscap.StreamInfo{
			Name:  name,
			Size:  int64(size),
			Alloc: int64(size),
		})
	}
	return out, nil
}

func (s *posixStreams) OpenStream(ref oscap.Ref, name string, flags oscap.StreamFlags) (oscap.StreamHandle, error) {
	full, err := streamXattrName(name)
	if err != nil {
		return nil, err
	}
	wantWrite := flags&(oscap.StreamWrite|oscap.StreamCreate|oscap.StreamTruncate) != 0
	if wantWrite && s.readOnly {
		return nil, oscap.ErrReadOnly
	}

	h := &xattrStreamHandle{ref: ref, full: full, write: wantWrite}

	_, getErr := xattrGet(ref, full, nil)
	switch {
	case getErr == nil:
		if flags&oscap.StreamTruncate != 0 {
			if err := h.store(nil); err != nil {
				return nil, err
			}
		}
		return h, nil

	case errors.Is(mapPosixErr(getErr), oscap.ErrNotFound) && flags&oscap.StreamCreate != 0:
		// 注意这条分支也会吃到「宿主对象本身不存在」的 ENOENT，
		// 但紧接着的 store 会因为同一个原因失败并如实返回 ErrNotFound，
		// 不会凭空创造出一个流。
		if err := h.store(nil); err != nil {
			return nil, err
		}
		return h, nil

	default:
		return nil, mapPosixErr(getErr)
	}
}

func (s *posixStreams) RemoveStream(ref oscap.Ref, name string) error {
	if s.readOnly {
		return oscap.ErrReadOnly
	}
	full, err := streamXattrName(name)
	if err != nil {
		return err
	}
	return mapPosixErr(xattrRemove(ref, full))
}

// xattrStreamHandle 是承载在扩展属性上的 oscap.StreamHandle。
//
// **每次操作都重新读一遍属性**，不做内存缓存。理由：xattr 是共享状态，
// 宿主机上的别的进程（甚至同一个客户端的另一个句柄）随时可能改它，
// 缓存会让两个句柄各写各的、后关闭的那个整个覆盖先关闭的。
// 流本来就只有几十到几百字节，多一次系统调用换一份始终正确的视图很划算。
type xattrStreamHandle struct {
	mu   sync.Mutex
	ref  oscap.Ref
	full string
	// write 记录打开时有没有要写。没要就拒绝写操作 ——
	// 悄悄放行会让「只读打开」这个承诺变成空话。
	write  bool
	closed bool
}

var _ oscap.StreamHandle = (*xattrStreamHandle)(nil)

// load 读出流内容（已剥掉 marker 字节）。
func (h *xattrStreamHandle) load() ([]byte, error) {
	raw, err := getXattrRaw(h.ref, h.full)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		// 空值不该出现（marker 保证至少 1 字节）。当成空流而不是报错：
		// 可能是别的工具直接写的 xattr。
		return nil, nil
	}
	if marker := raw[len(raw)-1]; marker != 0 {
		// Samba 的多 xattr 续存形态，我们读不全，见文件头说明。
		return nil, fmt.Errorf(
			"oscap/native: 命名流使用了 Samba 多 xattr 续存格式（marker=%d），本实现读不全: %w",
			marker, oscap.ErrNotSupported)
	}
	return raw[:len(raw)-1], nil
}

// store 覆盖写入流内容（自动补 marker 字节）。
func (h *xattrStreamHandle) store(data []byte) error {
	if len(data) > streamMaxBytes {
		// 用 ENOSPC 而不是自造 sentinel：vfs 的 errmap 已经把它映射成
		// ErrNoSpace → STATUS_DISK_FULL，客户端能看懂「写不下」。
		return fmt.Errorf("oscap/native: 命名流 %d 字节超过 xattr 承载上限 %d: %w",
			len(data), streamMaxBytes, errNoSpace)
	}
	buf := make([]byte, len(data)+1)
	copy(buf, data)
	// buf[len(data)] 保持 0：既满足「xattr 值不能为空」，也让 Samba 读得懂。
	return mapPosixErr(xattrSet(h.ref, h.full, buf))
}

// begin 是每个方法的统一入口：加锁并确认句柄没关。
func (h *xattrStreamHandle) begin() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return oscap.ErrClosed
	}
	return nil
}

func (h *xattrStreamHandle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("oscap/native: 读偏移 %d 为负: %w", off, oscap.ErrInvalidArg)
	}
	if err := h.begin(); err != nil {
		return 0, err
	}
	defer h.mu.Unlock()

	data, err := h.load()
	if err != nil {
		return 0, err
	}
	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(p, data[off:])
	if n < len(p) {
		// io.ReaderAt 的契约：短读必须带上错误，否则调用方会以为
		// 缓冲区被填满了。
		return n, io.EOF
	}
	return n, nil
}

func (h *xattrStreamHandle) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("oscap/native: 写偏移 %d 为负: %w", off, oscap.ErrInvalidArg)
	}
	if err := h.begin(); err != nil {
		return 0, err
	}
	defer h.mu.Unlock()
	if !h.write {
		return 0, oscap.ErrReadOnly
	}
	if off > int64(streamMaxBytes)-int64(len(p)) {
		return 0, fmt.Errorf("oscap/native: 写入 [%d,%d) 超过 xattr 承载上限 %d: %w",
			off, off+int64(len(p)), maxStreamSize, errNoSpace)
	}

	data, err := h.load()
	if err != nil {
		return 0, err
	}
	end := off + int64(len(p))
	if end > int64(len(data)) {
		// 越过原长度时中间的空隙补零 —— 与普通文件的 pwrite 语义一致。
		grown := make([]byte, end)
		copy(grown, data)
		data = grown
	}
	copy(data[off:end], p)
	if err := h.store(data); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (h *xattrStreamHandle) Truncate(size int64) error {
	if size < 0 {
		return fmt.Errorf("oscap/native: 截断长度 %d 为负: %w", size, oscap.ErrInvalidArg)
	}
	if err := h.begin(); err != nil {
		return err
	}
	defer h.mu.Unlock()
	if !h.write {
		return oscap.ErrReadOnly
	}

	data, err := h.load()
	if err != nil {
		return err
	}
	switch {
	case size == int64(len(data)):
		return nil
	case size < int64(len(data)):
		data = data[:size]
	default:
		grown := make([]byte, size)
		copy(grown, data)
		data = grown
	}
	return h.store(data)
}

func (h *xattrStreamHandle) Size() (int64, error) {
	if err := h.begin(); err != nil {
		return 0, err
	}
	defer h.mu.Unlock()

	// 只问长度，不取值。
	size, err := xattrGet(h.ref, h.full, nil)
	if err != nil {
		return 0, mapPosixErr(err)
	}
	if size > 0 {
		size-- // marker 字节
	}
	return int64(size), nil
}

// Close 释放句柄。
//
// 没有需要 flush 的东西（每次写都已经落到 xattr 了），所以重复关闭
// 也只是幂等地返回 nil —— 不报错是刻意的：defer Close() 与显式 Close()
// 同时存在是常见写法，为此报错纯属添乱。
func (h *xattrStreamHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}
