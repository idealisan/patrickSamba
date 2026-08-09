//go:build linux || darwin

package native

// fileid_posix.go —— oscap.StableFileID 的原生实现：POSIX 的 st_ino。
//
// st_ino 恰好就是 ports.go 要的那两条语义：同一卷内唯一、跨重命名不变
// （重命名只改目录项，inode 不动）。对应 SMB 的 FileInternalInformation
// 与 AAPL 的 QFid。

import (
	"golang.org/x/sys/unix"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// posixIDs 实现 oscap.StableFileID。
type posixIDs struct{}

var _ oscap.StableFileID = posixIDs{}

// FileID 返回 inode 号。
//
// # 只用 ino，不掺 dev
//
// 掺进 st_dev 能覆盖「共享目录里挂了别的文件系统」这种跨卷撞车，
// 但那必须靠哈希把 128 位压回 64 位，而哈希会引入**概率性碰撞** ——
// 用一个「几乎不会撞」的值换掉一个「保证不撞」的值，在同一卷（绝大多数
// 部署）这个主场景里是净亏。而且 internal/vfs/attr_linux.go 现在填的
// FileID 就是裸 st_ino，两处不一致会让同一个文件在 QUERY_INFO 与
// readdir_attr 两条路径上报出不同的 ID —— macOS 会据此认为文件被换掉了，
// 那比跨卷撞车严重得多。
//
// 跨卷共享的正确解法是让 builtin 侧发号，不是在这里做一个更差的哈希。
func (posixIDs) FileID(ref oscap.Ref) (uint64, error) {
	var st unix.Stat_t
	var err error
	if fd, ok := refFD(ref); ok {
		err = unix.Fstat(fd, &st)
	} else {
		// Lstat 而不是 Stat：与 vfs 全程的 lstat/O_NOFOLLOW 语义一致，
		// 否则一个符号链接会报出它指向的那个文件的 ID。
		err = unix.Lstat(ref.Path, &st)
	}
	if err != nil {
		return 0, mapPosixErr(err)
	}
	if st.Ino == 0 {
		// 0 是「未知」的惯用哨兵。拿它当 FileID 会让所有对象撞成同一个，
		// 而**一个会撞的 FileID 比没有 FileID 更糟**（ports.go）。
		return 0, oscap.ErrNotSupported
	}
	return uint64(st.Ino), nil
}
