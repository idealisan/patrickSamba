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

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// pathSeparator 是宿主机路径分隔符的字符串形式（"/" 或 "\\"）。
const pathSeparator = string(filepath.Separator)

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

	// SetSparse 标记/取消稀疏文件（FSCTL_SET_SPARSE，MS-FSCC §2.3.69）。
	//
	// POSIX 后端上这个操作是**不对称**的：
	//   v=true  → nil（文件天然可稀疏，客户端要的效果已经成立）
	//   v=false → ErrNotSupported（做不到，且不能假装做到）
	// 理由见 sparse_unix.go 的 platformSetSparse。Windows/NTFS 两个方向
	// 都真实生效。
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

// DirAppleMetadata 是 AppleMetadata 的**目录句柄内**版本，由目录 Handle 实现。
//
// 为什么要有第二个入口：readdir_attr 是逐条目调用的，而
// FileSystem.AppleInfo 每次都要把 "bands/0001a" 这样的路径从共享根
// 重新解析一遍（逐级 lstat + 软链校验）。目录句柄已经持有宿主机路径，
// 直接拼一个分量即可，在十万级 band 目录上省掉的是十万次重复解析。
//
//	if dm, ok := dirHandle.(vfs.DirAppleMetadata); ok {
//	    fi, rsrc, err := dm.AppleInfoAt(entry.Name)
//	}
type DirAppleMetadata interface {
	// AppleInfoAt 取本目录下**直接子项** name 的 Apple 元数据，
	// 语义与 AppleMetadata.AppleInfo 完全一致。
	//
	// name 必须是单个路径分量：含分隔符、".."、或其他非法字符时返回
	// ErrInvalidPath —— 这是安全边界，不能因为「反正是内部调用」就省掉
	// （AGENTS.md §8）。
	AppleInfoAt(name string) (finderInfo [FinderInfoSize]byte, rsrcSize int64, err error)

	// AppleInfoAtBatch 是 AppleInfoAt 的批量版本，语义**逐条完全一致**。
	//
	// 返回的切片长度恒等于 len(names)，第 i 项对应 names[i]；
	// 单条目的失败记在该项的 Err 里，**不会**中断整批 ——
	// 一个名字非法不该让整页 QUERY_DIRECTORY 失败。
	// 返回的 error 只表示句柄级失败（ErrNotDir / ErrClosed），
	// 此时结果切片为 nil。
	//
	// 相对逐条调用省掉的是每条一次的锁获取与 402 字节读缓冲分配
	// （整批复用一个）。getxattr 本身省不掉：POSIX 没有批量扩展属性
	// 接口，见 apple_bench_test.go 的实测结论。
	AppleInfoAtBatch(names []string) ([]AppleInfoResult, error)
}

// AppleInfoResult 是 AppleInfoAtBatch 的单条结果。
type AppleInfoResult struct {
	FinderInfo [FinderInfoSize]byte
	RsrcSize   int64
	// Err 非 nil 表示这一条取失败（名字非法等）。注意「对象没有 Apple
	// 元数据」不是错误：那种情况是零值 FinderInfo + RsrcSize 0 + nil。
	Err error
}

var (
	_ DeleteOnCloser   = (*localHandle)(nil)
	_ SparseFile       = (*localHandle)(nil)
	_ AppleMetadata    = (*LocalFS)(nil)
	_ DirAppleMetadata = (*localHandle)(nil)
	_ HardLinker       = (*LocalFS)(nil)
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
	// 这个 Lstat 不是多余的存在性检查：Resolver.resolveComponents
	// **故意放行不存在的末级分量**（path.go:325「末级不存在是合法的」，
	// FILE_CREATE / FILE_OPEN_IF 要用），所以 Resolve 成功不代表对象存在。
	// 少了它，对不存在的路径查 Apple 元数据会返回全零 + nil，
	// 上层会把「文件不存在」误当成「文件存在但没有 FinderInfo」。
	if _, err := os.Lstat(host); err != nil {
		return fi, 0, mapError(err)
	}

	return l.appleInfoAt(host, true, nil)
}

// AppleInfoAt 实现 DirAppleMetadata。
func (h *localHandle) AppleInfoAt(name string) ([FinderInfoSize]byte, int64, error) {
	var fi [FinderInfoSize]byte
	if !h.isDir {
		return fi, 0, ErrNotDir
	}
	if err := validateChildName(name); err != nil {
		return fi, 0, err
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return fi, 0, ErrClosed
	}
	host := h.host
	// 已经拍过快照的话，「这个条目有没有 ._ 旁路文件」是已知事实，
	// 不必再对每个条目做一次注定 ENOENT 的 open。Time Machine 的 bands
	// 目录里一个 ._ 都没有，这一条省掉的是「一次 syscall × 条目数」。
	probeRsrc := true
	if h.dirNames != nil {
		_, probeRsrc = h.dotUnder[name]
	}
	h.mu.Unlock()

	return h.fs.appleInfoAt(filepath.Join(host, name), probeRsrc, nil)
}

// AppleInfoAtBatch 实现 DirAppleMetadata。
func (h *localHandle) AppleInfoAtBatch(names []string) ([]AppleInfoResult, error) {
	if !h.isDir {
		return nil, ErrNotDir
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	host := h.host
	// 快照期间记下的 ._ 集合。这里把 map 引用**带出锁**是安全的：
	// snapshotLocked 重新枚举时是整个换一个新 map，从不原地改旧的，
	// 所以我们手上这一份不会被并发写。
	var dotUnder map[string]struct{}
	snapped := h.dirNames != nil
	if snapped {
		dotUnder = h.dotUnder
	}
	h.mu.Unlock()

	// host 是 filepath.Join 的产物（已 Clean，无尾分隔符），唯一例外是
	// 共享根恰好是 "/" 或 "C:\" 这种本身以分隔符结尾的路径。
	prefix := host
	if !strings.HasSuffix(prefix, pathSeparator) {
		prefix += pathSeparator
	}

	out := make([]AppleInfoResult, len(names))
	// 整批复用一个读缓冲 —— 这正是批量接口相对逐条调用的收益所在。
	scratch := make([]byte, adMetaSize)

	for i, name := range names {
		if err := validateChildName(name); err != nil {
			out[i].Err = err
			continue
		}
		probeRsrc := true
		if snapped {
			_, probeRsrc = dotUnder[name]
		}
		// 直接拼接而不是 filepath.Join：name 已经过 validateChildName，
		// 不含分隔符也不是 "."/“..”，无需再走一遍 Clean。
		out[i].FinderInfo, out[i].RsrcSize, out[i].Err =
			h.fs.appleInfoAt(prefix+name, probeRsrc, scratch)
	}
	return out, nil
}

// validateChildName 是目录句柄内按名字寻址的安全边界（AGENTS.md §8）。
//
// ValidateComponent 会拒掉空串、控制字符、'/'、'\\' 与 Windows 保留
// 设备名，但它**故意放行 "." 与 ".."**（SplitPath 另行处理它们），
// 所以这里必须单独拦一道 —— 否则 filepath.Join(host, "..") 会直接
// 拼出父目录，把 readdir_attr 变成一个目录穿越原语。
func validateChildName(name string) error {
	if name == "." || name == ".." {
		return ErrInvalidPath
	}
	return ValidateComponent(name)
}

// appleInfoAt 是三个入口共用的实现：宿主机路径 → FinderInfo + 资源派生大小。
//
// probeRsrc=false 表示调用方已经确知没有 ._ 旁路文件，跳过那次探测。
// scratch 是可复用的 metadata 读缓冲，传 nil 则内部自行分配。
func (l *LocalFS) appleInfoAt(host string, probeRsrc bool, scratch []byte) ([FinderInfoSize]byte, int64, error) {
	var fi [FinderInfoSize]byte
	// 两者都是「没有就算了」：缺 FinderInfo 或缺资源派生都是正常状态，
	// 磁盘上的 blob 损坏也一样（一个坏掉的 FinderInfo 不该让整条目录项失败）。
	// scratch 目前没有用武之地：oscap.Xattr 的 GetXattr 自己分配返回值。
	// 保留形参是刻意的 —— 批量枚举那条路径（AppleInfoAtBatch）复用同一个
	// 缓冲，将来若给 port 补上「读进调用方缓冲」的定长快路径，改动就只在
	// 这一行。**不要**因为「现在没用到」就把它删掉再让后来人重新加回来。
	_ = scratch
	if blob, err := l.xattrAt(host, nil).Get(netatalkMetaXattr); err == nil {
		if got, err := parseMetaXattr(blob); err == nil {
			fi = *got
		}
	}
	if !probeRsrc {
		return fi, 0, nil
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
	return mapOscapError(h.fs.caps.Sparse().PunchHole(oscap.Ref{Path: h.host, Handle: h.f}, off, length))
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
	return mapOscapError(h.fs.caps.Sparse().Preallocate(oscap.Ref{Path: h.host, Handle: h.f}, off, length))
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
	// 第 3 参是**长度**（oscap.SparseFile 契约：返回 [off, off+length) 内的区间），
	// 不是绝对结束位置。传 end 会让实现再算一次 off+length，把窗口多算一个 off：
	// Linux 上多出来的那段通常落在洞里、侥幸不显形，而 builtin（macOS 走的那条）
	// 会直接把超出窗口的区间报给客户端。
	rs, err := h.fs.caps.Sparse().AllocatedRanges(oscap.Ref{Path: h.host, Handle: f}, off, end-off)
	if err != nil {
		return nil, mapOscapError(err)
	}
	out := make([]Range, len(rs))
	for i, r := range rs {
		out[i] = Range{Offset: r.Offset, Length: r.Length}
	}
	return out, nil
}

// SetSparse 实现 SparseFile。
func (h *localHandle) SetSparse(v bool) error {
	if err := h.checkWritable(); err != nil {
		return err
	}
	return mapOscapError(h.fs.caps.Sparse().SetSparse(oscap.Ref{Path: h.host, Handle: h.f}, v))
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
