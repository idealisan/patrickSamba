//go:build darwin

package native

import "golang.org/x/sys/unix"

// xattrNamespace 在 macOS 上为空：macOS 的扩展属性没有命名空间概念，
// `com.apple.FinderInfo` 这种名字就是原样落盘的。
const xattrNamespace = ""

// errnoNoAttr 是「扩展属性不存在」的 errno。macOS 用 ENOATTR。
const errnoNoAttr = unix.ENOATTR
