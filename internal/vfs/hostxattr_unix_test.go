//go:build linux || darwin

package vfs

// hostxattr_unix_test.go —— **绕过本项目全部代码**、直接向内核问宿主机上的
// 扩展属性。
//
// 为什么必须有这么一层：接线之后，「我们自己的接口读得回来」只能证明
// 数据在某个地方，证明不了它在**哪里**。而 portable 档的全部承诺就是
// 「完全不碰 OS 的可选能力」—— 要证伪它，只能拿内核当裁判。
//
// 因此这里刻意**不复用** oscap/native 的任何函数（连命名空间前缀的拼接都
// 自己写一遍）：复用它就等于让被测代码给自己打分，它把名字算错时这层探针
// 会跟着一起错，两边一致地错到一起去，测试照样绿。

import (
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// hostXattrNamespace 是 Linux 上非特权进程唯一可写的命名空间前缀。
// macOS 没有命名空间概念。
func hostXattrNamespace() string {
	if runtime.GOOS == "linux" {
		return "user."
	}
	return ""
}

// hostXattrList 列出宿主机上该文件的**全部**扩展属性名（原样，含命名空间前缀）。
func hostXattrList(t *testing.T, path string) []string {
	t.Helper()
	size, err := unix.Listxattr(path, nil)
	if err != nil {
		if err == unix.ENOTSUP || err == unix.EOPNOTSUPP {
			// 宿主文件系统根本没有扩展属性：那 native 档在这台机器上就跑
			// 不起来，相关用例应当在别处以 matrix 断言的形式判红，
			// 而不是在这里假装「列表为空」蒙混过去。
			t.Fatalf("宿主 %s 所在文件系统不支持扩展属性；本用例校验的是宿主落盘，无法在此环境成立", path)
		}
		t.Fatalf("listxattr(%s): %v", path, err)
	}
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	n, err := unix.Listxattr(path, buf)
	if err != nil {
		t.Fatalf("listxattr(%s): %v", path, err)
	}
	if n > len(buf) {
		n = len(buf)
	}
	var out []string
	for _, s := range strings.Split(string(buf[:n]), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// hostXattrGet 读宿主机上的一个扩展属性。name 不含命名空间前缀。
func hostXattrGet(t *testing.T, path, name string) ([]byte, error) {
	t.Helper()
	full := hostXattrNamespace() + name
	size, err := unix.Getxattr(path, full, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	n, err := unix.Getxattr(path, full, buf)
	if err != nil {
		return nil, err
	}
	if n > len(buf) {
		n = len(buf)
	}
	return buf[:n], nil
}

// hostXattrSet 直接往宿主机上写一个扩展属性，**完全绕开本项目的写路径**。
// 用于构造「别的实现（Samba/Netatalk）留下的既有数据」。
func hostXattrSet(t *testing.T, path, name string, value []byte) {
	t.Helper()
	if err := unix.Setxattr(path, hostXattrNamespace()+name, value, 0); err != nil {
		t.Fatalf("setxattr(%s, %s): %v", path, name, err)
	}
}

// hostXattrSupported 报告宿主机上这个路径能不能写扩展属性。
//
// **只用于判定「这台机器能不能承载 native 落盘类用例」**，不用于跳过断言。
func hostXattrSupported(path string) bool {
	const probe = "user.stupidsamba.probe"
	name := probe
	if runtime.GOOS != "linux" {
		name = "stupidsamba.probe"
	}
	if err := unix.Setxattr(path, name, []byte{1}, 0); err != nil {
		return false
	}
	_ = unix.Removexattr(path, name)
	return true
}
