//go:build linux || darwin

package oscap

// probe_helper_unix_test.go —— probe_test.go 的 POSIX 侧交叉验证工具。
//
// 这里刻意**不复用** probe_unix.go 的 probeInode：那样就变成「用被测代码
// 验证被测代码」，probeInode 里写错一个字段两边会一起错，测试全绿。
// 交叉验证要的是一条独立取值的路径。

import "golang.org/x/sys/unix"

// fileIDForTest 独立取一次 st_ino，用来核对 CapStableFileID 的探测结果。
func fileIDForTest(root string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		return 0, err
	}
	return uint64(st.Ino), nil
}

// runtimeIsPOSIX 标记「命名流承载在扩展属性之上」这条耦合是否适用。
func runtimeIsPOSIX() bool { return true }
