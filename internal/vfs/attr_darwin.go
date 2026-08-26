//go:build darwin

package vfs

import (
	"io/fs"
	"os"
	"syscall"
	"time"
)

// fillSysAttr 从 macOS 的 struct stat 补齐字段。
//
// 与 Linux 的差别：字段名是 *timespec 而不是 *tim，
// 并且**原生带 Birthtimespec**（HFS+/APFS 都记录真实创建时间），
// 不需要像 Linux 那样另外调 statx。
func fillSysAttr(fi fs.FileInfo, a *Attr) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	a.UID = st.Uid
	a.GID = st.Gid
	a.NLink = uint32(st.Nlink)
	a.FileID = st.Ino
	a.Alloc = int64(st.Blocks) * 512
	a.AccessTime = time.Unix(st.Atimespec.Unix())
	a.WriteTime = time.Unix(st.Mtimespec.Unix())
	a.ChangeTime = time.Unix(st.Ctimespec.Unix())
	a.CreateTime = time.Unix(st.Birthtimespec.Unix())
}

func fillSysAttrFromFile(*os.File, *Attr) {}

// statCreateTime 在 macOS 上是多余的（Birthtimespec 已在 fillSysAttr 里取到）。
func statCreateTime(string) (time.Time, bool) { return time.Time{}, false }

// fileAccessTime 从 FileInfo 取当前 atime（sticky write time 补偿用，
// 见 attr_linux.go 同名函数的说明）。
func fileAccessTime(fi fs.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Atimespec.Unix()), true
}
