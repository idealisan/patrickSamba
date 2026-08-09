//go:build !linux && !darwin && !windows

package oscap

// probe_helper_other_test.go —— 未知平台上的占位。
//
// 这些平台的 probeNative 恒为 false（见 probe_other.go），
// 所以 fileIDForTest 那条分支根本不会被走到；这里只是让测试能编译。
// 真返回错误也无妨 —— 走到就说明 probe_other.go 被改坏了，
// 那时报错正是我们想要的。

func fileIDForTest(string) (uint64, error) { return 0, ErrNotSupported }

func runtimeIsPOSIX() bool { return false }
