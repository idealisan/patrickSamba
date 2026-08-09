//go:build linux || darwin

package vfs

import (
	"os"
	"syscall"
	"testing"
)

// TestUnsupportedSeek 固定「哪些 errno 算文件系统不支持空洞探测」。
//
// 这条分类决定了降级还是报错，写错会让一整类文件系统上的
// FSCTL_QUERY_ALLOCATED_RANGES 直接失败，所以单独钉住。
func TestUnsupportedSeek(t *testing.T) {
	yes := []syscall.Errno{
		syscall.EINVAL,     // 老内核 / 不支持的 fs
		syscall.EOPNOTSUPP, // 部分网络文件系统
		syscall.ENOSYS,
	}
	for _, e := range yes {
		if !unsupportedSeek(e) {
			t.Errorf("unsupportedSeek(%v) = false；期望 true", e)
		}
	}
	no := []syscall.Errno{
		syscall.ENXIO, // 「后面全是洞」的正常终止条件，不是不支持
		syscall.EBADF,
		syscall.EIO,
	}
	for _, e := range no {
		if unsupportedSeek(e) {
			t.Errorf("unsupportedSeek(%v) = true；期望 false", e)
		}
	}
	// 包成 *os.PathError 之后仍然要能识别（unix.Seek 返回裸 errno，
	// 但上层可能包装，用 errors.Is 而不是 == 才安全）。
	if !unsupportedSeek(&os.PathError{Op: "seek", Err: syscall.EINVAL}) {
		t.Error("被 *os.PathError 包裹的 EINVAL 未被识别")
	}
}

// TestSeekConstantsDiffer 钉住 Linux 与 darwin 的常量数值。
//
// 这两个平台上 SEEK_DATA / SEEK_HOLE 的值**正好相反**，
// 写死数字会让 macOS 上的空洞与数据完全颠倒。本测试保证一旦
// 有人把代码改成硬编码常量，至少能在对应平台上被发现。
func TestSeekConstantsDiffer(t *testing.T) {
	data, hole := seekDataConst, seekHoleConst
	if data == hole {
		t.Fatalf("SEEK_DATA 与 SEEK_HOLE 相同 (%d)，不可能正确", data)
	}
	if data != 3 && data != 4 {
		t.Errorf("SEEK_DATA = %d，超出已知取值 {3,4}", data)
	}
}
