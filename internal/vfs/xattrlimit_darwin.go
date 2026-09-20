//go:build darwin

package vfs

// platformXattrNameMax 是**宿主内核**允许的完整扩展属性名字节上限。
//
// macOS 是 127（XATTR_MAXNAMELEN，<sys/xattr.h>）。预算必须用宿主真实上限：
// 取宽会让「预算内」的流名在真正落盘时被内核拒（ENAMETOOLONG），
// 等于给客户端一个假承诺。见 stream_xattr.go 的 maxXattrNameLen。
const platformXattrNameMax = 127
