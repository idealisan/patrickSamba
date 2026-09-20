//go:build !darwin

package vfs

// platformXattrNameMax 是**宿主内核**允许的完整扩展属性名字节上限。
//
// Linux 是 255（XATTR_NAME_MAX，<linux/limits.h>）。Windows 上通用流走 NTFS
// 备用数据流、不落 xattr，这里保留同一个上限只是为了让流名校验有一条
// 平台一致的边界。见 stream_xattr.go 的 maxXattrNameLen。
const platformXattrNameMax = 255
