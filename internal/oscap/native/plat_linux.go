//go:build linux

package native

import "golang.org/x/sys/unix"

// xattrNamespace 是 Linux 上非特权进程唯一可读写的扩展属性命名空间前缀。
//
// 少了它，setxattr 一律 EPERM —— 于是「支持 xattr 的文件系统」也会表现得
// 像不支持。probe_linux.go 的 probeXattrName 出于同样理由带着这个前缀。
const xattrNamespace = "user."

// errnoNoAttr 是「扩展属性不存在」的 errno。
//
// Linux 用 ENODATA，macOS 用 ENOATTR（见 plat_darwin.go）——
// 这两个常量在两个平台上都不是同一个数值，写死一个会让另一个平台把
// 「属性不存在」误判成一个未知错误，于是 GetXattr 返回的不是 ErrNotFound。
const errnoNoAttr = unix.ENODATA
