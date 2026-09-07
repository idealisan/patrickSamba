//go:build linux || darwin

package vfs

// sparse_unix.go —— 用 lseek(2) 的 SEEK_DATA / SEEK_HOLE 探测空洞分布。
//
// ⚠️ **本文件的 platformAllocatedRanges / platformSetSparse 当前没有调用点**
// （真实路径是 vfs/optional.go → oscap 层，见 sparse_other.go 的说明）。
// 保留只为不违反禁止删除的约定，改这里不会改变运行期行为。
// 下面那段常量说明仍然有效，是因为 oscap/native 侧用的是同一组符号常量。
//
// **常量在两个平台上的数值是相反的**，这是本文件最容易踩的坑：
//
//	Linux   <linux/fs.h>        SEEK_DATA = 3, SEEK_HOLE = 4
//	darwin  <sys/fcntl.h>       SEEK_HOLE = 3, SEEK_DATA = 4
//
// 已核对 golang.org/x/sys@v0.47.0 的 zerrors_linux.go:3455/3457 与
// zerrors_darwin_arm64.go:1280/1282，与上述一致。
// 因此这里**只能**用 unix.SEEK_DATA / unix.SEEK_HOLE 这两个符号，
// 绝不能写死 3/4 —— 写死会让 macOS 上的空洞与数据完全颠倒。
//
// 支持度：Linux 上 ext4/xfs/btrfs/f2fs 都支持；tmpfs 支持但整个文件
// 常被报成一整段数据；overlayfs 视底层而定。macOS 上 APFS 支持，
// HFS+ 不支持稀疏文件。不支持时 lseek 返回 EINVAL/ENOTSUP，见下面的降级。

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// seekDataConst / seekHoleConst 只是给上面那段说明一个可被测试钉住的名字。
// 取值来自 x/sys/unix 的平台常量表，**不要**改成字面量 3/4。
const (
	seekDataConst = unix.SEEK_DATA
	seekHoleConst = unix.SEEK_HOLE
)

// platformAllocatedRanges 返回 [off, end) 内已分配的子区间。
// 调用方保证 0 <= off < end <= EOF。
func platformAllocatedRanges(f *os.File, off, end int64) ([]Range, error) {
	fd := int(f.Fd())

	// 注意：lseek 会移动该 fd 的文件偏移。本包的所有读写都走
	// pread/pwrite（ReadAt/WriteAt），不依赖文件偏移；目录枚举也另开了
	// 独立的 fd（local_handle.go 的 snapshotLocked）。故这里可以安全地动它。
	out := make([]Range, 0, 8)
	cur := off
	for cur < end {
		dataOff, err := unix.Seek(fd, cur, seekDataConst)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				// cur 之后到 EOF 全是空洞 —— 正常终止条件，不是错误。
				break
			}
			if unsupportedSeek(err) {
				return wholeRange(off, end), nil
			}
			return nil, mapError(err)
		}
		if dataOff >= end {
			break
		}
		if dataOff < cur {
			// 理论上不会发生；防御性地保证游标单调前进，不然会死循环。
			dataOff = cur
		}

		holeOff, err := unix.Seek(fd, dataOff, seekHoleConst)
		if err != nil {
			if unsupportedSeek(err) {
				return wholeRange(off, end), nil
			}
			// SEEK_HOLE 在 EOF 处总能成功（EOF 之后被视为空洞），
			// 走到这里说明是别的问题，如实上报。
			return nil, mapError(err)
		}
		if holeOff > end {
			holeOff = end
		}
		if holeOff <= dataOff {
			break
		}

		out = append(out, Range{Offset: dataOff, Length: holeOff - dataOff})
		cur = holeOff
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// unsupportedSeek 判断 errno 是不是「本文件系统不支持空洞探测」。
//
// 不同实现给的 errno 不统一：Linux 老内核/不支持的 fs 给 EINVAL，
// 部分网络文件系统给 ENOTSUP/EOPNOTSUPP，极老的内核给 ENOSYS。
// 这几种一律降级成「整段已分配」，而不是把一个可用的查询变成失败。
func unsupportedSeek(err error) bool {
	return errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOSYS)
}

// platformSetSparse 在 POSIX 上是**不对称**的，这是有意的：
//
//	SetSparse(true)   无操作返回 nil —— POSIX 文件天然可稀疏，
//	                  客户端要的效果本来就成立。
//	SetSparse(false)  返回 ErrNotSupported —— 我们**真的做不到**。
//
// 为什么 false 不能也谎称成功：本项目的 FILE_ATTRIBUTE_SPARSE_FILE
// 不是存下来的标志位，而是在 attr.go:103 由 `Alloc < Size` 现算的。
// 若这里假装取消成功，客户端回头查属性会**照样看到 SPARSE 位**
// （洞还在），得到一个自相矛盾的视图。如实报不支持，客户端至少知道
// 这个文件的稀疏性关不掉。
//
// 真要取消稀疏就得把所有洞填零，那会让一个 8 MiB 的 Time Machine band
// 从占几 KiB 变成占满 8 MiB —— 正是 .sparsebundle 要避免的事，
// 绝不能作为一个 FSCTL 的副作用悄悄发生。
func platformSetSparse(_ *os.File, v bool) error {
	if !v {
		return ErrNotSupported
	}
	return nil
}
