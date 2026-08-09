//go:build linux

package native

// sparse_linux.go —— oscap.SparseFile 的原生实现：fallocate(2) + lseek(2)。
//
// 这一项对本项目是刚需：Time Machine 的 .sparsebundle 由大量固定大小的 band
// 文件组成，备份过期回收时会把 band 里的区间置零。没有真正的打洞，
// 备份卷只会越来越大、永远不会缩小（AGENTS.md §2 阶段二）。

import (
	"errors"

	"golang.org/x/sys/unix"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// linuxSparse 实现 oscap.SparseFile。
type linuxSparse struct {
	readOnly bool
}

var _ oscap.SparseFile = (*linuxSparse)(nil)

// PunchHole 用 FALLOC_FL_PUNCH_HOLE 释放区间，读回来是零，逻辑长度不变。
//
// 必须同时带 FALLOC_FL_KEEP_SIZE：内核要求这两个标志一起用，
// 少一个直接 EINVAL。
func (s *linuxSparse) PunchHole(ref oscap.Ref, off, length int64) error {
	if err := checkWindow(off, length); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	if s.readOnly {
		return oscap.ErrReadOnly
	}
	fd, cleanup, err := openForWrite(ref)
	if err != nil {
		return err
	}
	defer cleanup()

	return mapPosixErr(unix.Fallocate(fd,
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length))
}

// Preallocate 预留空间但**不改变** EOF（FALLOC_FL_KEEP_SIZE），
// 与 Windows 的 AllocationSize / SMB2 的 AlSi create context 语义一致。
func (s *linuxSparse) Preallocate(ref oscap.Ref, off, length int64) error {
	if err := checkWindow(off, length); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	if s.readOnly {
		return oscap.ErrReadOnly
	}
	fd, cleanup, err := openForWrite(ref)
	if err != nil {
		return err
	}
	defer cleanup()

	return mapPosixErr(unix.Fallocate(fd, unix.FALLOC_FL_KEEP_SIZE, off, length))
}

// AllocatedRanges 用 lseek 的 SEEK_DATA / SEEK_HOLE 走一遍空洞分布。
//
// ⚠️ 常量在 Linux 与 darwin 上**数值相反**（Linux 3/4，darwin 4/3），
// 所以只能用 unix.SEEK_DATA / unix.SEEK_HOLE 这两个符号，绝不能写死数字。
// 本文件只在 Linux 编译，但这条纪律照抄一遍是有意的：将来有人把它复制到
// darwin 版时，写死的数字会让空洞与数据完全颠倒且毫无报错。
func (s *linuxSparse) AllocatedRanges(ref oscap.Ref, off, length int64) ([]oscap.Range, error) {
	if err := checkWindow(off, length); err != nil {
		return nil, err
	}
	if length == 0 {
		return nil, nil
	}
	fd, cleanup, err := openForRead(ref)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, mapPosixErr(err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		// 目录没有「已分配区间」这回事。如实拒绝，而不是让 lseek 在目录 fd 上
		// 返回 EINVAL、进而被下面的降级逻辑当成「整段已分配」报回去。
		return nil, oscap.ErrInvalidArg
	}
	size := st.Size
	if off >= size {
		// 完全落在 EOF 之外：空结果是合法答案（ports.go）。
		return nil, nil
	}
	end := off + length
	if end > size {
		end = size
	}

	out := make([]oscap.Range, 0, 8)
	cur := off
	for cur < end {
		dataOff, err := unix.Seek(fd, cur, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				// cur 之后到 EOF 全是空洞 —— 正常终止条件，不是错误。
				break
			}
			if unsupportedSeek(err) {
				return wholeWindow(off, end), nil
			}
			return nil, mapPosixErr(err)
		}
		if dataOff >= end {
			break
		}
		if dataOff < cur {
			// 理论上不会发生；防御性地保证游标单调前进，否则死循环。
			dataOff = cur
		}

		holeOff, err := unix.Seek(fd, dataOff, unix.SEEK_HOLE)
		if err != nil {
			if unsupportedSeek(err) {
				return wholeWindow(off, end), nil
			}
			// SEEK_HOLE 在 EOF 处总能成功（EOF 之后被视为空洞），
			// 走到这里说明是别的问题，如实上报。
			return nil, mapPosixErr(err)
		}
		if holeOff > end {
			holeOff = end
		}
		if holeOff <= dataOff {
			break
		}

		out = append(out, oscap.Range{Offset: dataOff, Length: holeOff - dataOff})
		cur = holeOff
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// SetSparse 在 POSIX 上是**刻意不对称**的，理由见 ports.go：
//
//	true  → nil            文件天然可稀疏，客户端要的效果已经成立
//	false → ErrNotSupported 做不到，且不能假装做到
//
// 为什么 false 不能也谎称成功：FILE_ATTRIBUTE_SPARSE_FILE 不是存下来的
// 标志位，而是由 `Alloc < Size` 现算的。假装取消成功之后，客户端回头查属性
// 会照样看到 SPARSE 位（洞还在），得到一个自相矛盾的视图。
// 真要取消稀疏就得把所有洞填零 —— 那会让一个 8 MiB 的 Time Machine band
// 从占几 KiB 变成占满 8 MiB，绝不能作为一个 FSCTL 的副作用悄悄发生。
func (s *linuxSparse) SetSparse(_ oscap.Ref, v bool) error {
	if !v {
		return oscap.ErrNotSupported
	}
	return nil
}

// unsupportedSeek 判断 errno 是不是「本文件系统不支持空洞探测」。
//
// errno 不统一：老内核/不支持的 fs 给 EINVAL，部分网络文件系统给
// ENOTSUP/EOPNOTSUPP，极老的内核给 ENOSYS。一律降级成「整段已分配」
// 而不是把一个可用的查询变成失败 —— 多报已分配是安全的，少报会让客户端
// 以为数据丢了（ports.go）。
func unsupportedSeek(err error) bool {
	return errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOSYS)
}
