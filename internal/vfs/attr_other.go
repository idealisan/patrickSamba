//go:build !linux && !darwin && !windows

package vfs

import (
	"io/fs"
	"os"
	"time"
)

// 其余平台（freebsd / openbsd / solaris / js 等）：只用 fs.FileInfo 能表达的
// 信息，uid/gid/inode 等留空。项目的目标平台是 linux/darwin/windows
// （AGENTS.md C7），这里只是保证能编译过。
//
// TODO: 若要正式支持 BSD，按 attr_darwin.go 的样子补 Stat_t 映射即可
// （BSD 的字段名也是 *timespec，且有 Birthtimespec）。

func fillSysAttr(fs.FileInfo, *Attr) {}

func fillSysAttrFromFile(*os.File, *Attr) {}

func statCreateTime(string) (time.Time, bool) { return time.Time{}, false }

// fileAccessTime 在仅保证可编译的平台（freebsd/openbsd/solaris/js）上没有
// Stat_t 映射可走：返回 false 让调用方放弃补偿（bh3-F6 是尽力而为语义）。
func fileAccessTime(fs.FileInfo) (time.Time, bool) { return time.Time{}, false }
