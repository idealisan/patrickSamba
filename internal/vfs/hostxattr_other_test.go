//go:build !linux && !darwin

package vfs

// hostxattr_other_test.go —— 非 POSIX 平台上的宿主扩展属性探针占位。
//
// Windows 没有 POSIX 扩展属性（等价物是 NTFS 的 alternate data stream，
// 语义不同），所以这里给不出真探针。使用这些探针的用例一律带
// `linux || darwin` 约束，本文件只是让**其余**用例在 Windows 上照样编译 ——
// 而「能不能编译」正是 test/ci/check-test-compile.sh 四平台交叉 vet 要查的。

import "testing"

func hostXattrNamespace() string { return "" }

func hostXattrList(t *testing.T, path string) []string {
	t.Helper()
	t.Fatalf("本平台没有 POSIX 扩展属性探针（path=%s）", path)
	return nil
}

func hostXattrGet(t *testing.T, path, name string) ([]byte, error) {
	t.Helper()
	t.Fatalf("本平台没有 POSIX 扩展属性探针（path=%s name=%s）", path, name)
	return nil, nil
}

func hostXattrSet(t *testing.T, path, name string, value []byte) {
	t.Helper()
	t.Fatalf("本平台没有 POSIX 扩展属性探针（path=%s name=%s）", path, name)
}

func hostXattrSupported(string) bool { return false }
