//go:build !linux && !darwin

package vfs

import "os"

// Windows 上「扩展属性」的等价物是 NTFS 的 alternate data stream，
// 语义与 POSIX xattr 不同（ADS 是有大小、可流式读写的独立数据流），
// 应当由 FileSystem.Streams + OpenRequest.Stream 那条路径实现，
// 而不是套进 XattrAccessor。
//
// TODO(阶段二): 在 local.go 的 Open 里支持 req.Stream，
// Windows 直接用 "path:streamname" 打开，POSIX 用 user.DosStream.* 模拟。
func newXattrAccessor(string, *os.File) (XattrAccessor, error) {
	return nil, ErrNotSupported
}
