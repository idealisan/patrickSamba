//go:build windows

package oscap

// probe_helper_windows_test.go —— probe_test.go 的 Windows 侧交叉验证工具。

import (
	"os"

	"golang.org/x/sys/windows"
)

// fileIDForTest 独立取一次 file reference number。
//
// 目录也能这么取：Go 的 os.Open 在 Windows 上对目录会带
// FILE_FLAG_BACKUP_SEMANTICS 打开，句柄可以直接喂给
// GetFileInformationByHandle。
func fileIDForTest(root string) (uint64, error) {
	f, err := os.Open(root)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), nil
}

// runtimeIsPOSIX：Windows 上命名流走 ADS，与 xattr 无关，不适用那条耦合。
func runtimeIsPOSIX() bool { return false }
