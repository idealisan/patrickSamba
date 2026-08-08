//go:build !linux && !darwin && !windows

package vfs

import "os"

// 非目标平台的兜底实现（AGENTS.md C7 只要求 linux/darwin/windows）。
// 这里保证能编译过，并把「不支持」如实上报而不是假装成功。

func platformStatFS(string, *FSInfo) error { return ErrNotSupported }

func platformFullSync(f *os.File) error { return f.Sync() }

func platformPunchHole(*os.File, int64, int64) error { return ErrNotSupported }

func platformPreallocate(*os.File, int64, int64) error { return ErrNotSupported }
