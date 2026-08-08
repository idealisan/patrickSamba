//go:build linux || darwin

package vfs

// xattr_unix.go —— 扩展属性访问器。
//
// 用途（阶段二）：macOS 的 Finder 元数据与资源叉最终要落到这里 ——
// `com.apple.FinderInfo`（= SMB 的 AFP_AfpInfo 流）、
// `com.apple.ResourceFork`（= AFP_Resource 流）、`com.apple.metadata:*`。
// Samba 的 vfs_fruit 就是这么干的，macOS 客户端也是这么期待的。
//
// 命名空间差异：
//   - Linux 的非特权进程只能读写 `user.` 命名空间，其余会 EPERM，
//     所以没有已知前缀的名字一律加上 `user.`，读回时再剥掉；
//   - macOS 没有命名空间概念，原样使用。
//
// 这个差异必须双向对称，否则 List() 报出来的名字客户端拿去 Get 会找不到。

import (
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// xattrNeedsUserPrefix 只有 Linux 为 true。
var xattrNeedsUserPrefix = runtime.GOOS == "linux"

// linuxNamespaces 是 Linux 内核认可的扩展属性命名空间前缀。
var linuxNamespaces = []string{"user.", "trusted.", "security.", "system."}

type unixXattr struct {
	path string
	// fd 为 -1 表示只能按路径访问（OpenAttrOnly 句柄）。
	fd int
}

// newXattrAccessor 构造扩展属性访问器。
func newXattrAccessor(path string, f *os.File) (XattrAccessor, error) {
	x := &unixXattr{path: path, fd: -1}
	if f != nil {
		x.fd = int(f.Fd())
	}
	return x, nil
}

// encodeName 把 SMB 侧的名字映射成宿主机的扩展属性名。
func encodeName(name string) string {
	if !xattrNeedsUserPrefix {
		return name
	}
	for _, ns := range linuxNamespaces {
		if strings.HasPrefix(name, ns) {
			return name
		}
	}
	return "user." + name
}

// decodeName 是 encodeName 的逆操作。
func decodeName(raw string) string {
	if !xattrNeedsUserPrefix {
		return raw
	}
	return strings.TrimPrefix(raw, "user.")
}

func (x *unixXattr) Get(name string) ([]byte, error) {
	n := encodeName(name)
	// 两趟：先问长度再分配。不能一次性开个大 buffer 了事 ——
	// 资源叉可以有几十 MB，猜大了浪费、猜小了 ERANGE。
	size, err := x.get(n, nil)
	if err != nil {
		return nil, mapXattrError(err)
	}
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	got, err := x.get(n, buf)
	if err != nil {
		return nil, mapXattrError(err)
	}
	if got > len(buf) {
		// 两趟之间被别人写大了，防御性截断而不是越界。
		got = len(buf)
	}
	return buf[:got], nil
}

func (x *unixXattr) get(name string, dest []byte) (int, error) {
	if x.fd >= 0 {
		return unix.Fgetxattr(x.fd, name, dest)
	}
	return unix.Getxattr(x.path, name, dest)
}

func (x *unixXattr) Set(name string, value []byte) error {
	n := encodeName(name)
	var err error
	if x.fd >= 0 {
		err = unix.Fsetxattr(x.fd, n, value, 0)
	} else {
		err = unix.Setxattr(x.path, n, value, 0)
	}
	return mapXattrError(err)
}

func (x *unixXattr) Remove(name string) error {
	n := encodeName(name)
	var err error
	if x.fd >= 0 {
		err = unix.Fremovexattr(x.fd, n)
	} else {
		err = unix.Removexattr(x.path, n)
	}
	return mapXattrError(err)
}

func (x *unixXattr) List() ([]string, error) {
	size, err := x.list(nil)
	if err != nil {
		return nil, mapXattrError(err)
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	got, err := x.list(buf)
	if err != nil {
		return nil, mapXattrError(err)
	}
	if got > len(buf) {
		got = len(buf)
	}
	// 返回值是一串 NUL 结尾的名字拼接而成。
	out := make([]string, 0, 8)
	for _, raw := range strings.Split(string(buf[:got]), "\x00") {
		if raw == "" {
			continue
		}
		out = append(out, decodeName(raw))
	}
	return out, nil
}

func (x *unixXattr) list(dest []byte) (int, error) {
	if x.fd >= 0 {
		return unix.Flistxattr(x.fd, dest)
	}
	return unix.Listxattr(x.path, dest)
}

// mapXattrError 在通用 errmap 之上补两条扩展属性特有的语义。
func mapXattrError(err error) error {
	if err == nil {
		return nil
	}
	if err == errnoNoAttr {
		// 「这个属性不存在」，对客户端而言等价于对象不存在。
		// Linux 用 ENODATA，macOS 用 ENOATTR，常量在 sys_{linux,darwin}.go。
		return ErrNotFound
	}
	switch err {
	case unix.ENOTSUP:
		// 文件系统没开 user_xattr（老 ext3、某些 tmpfs 配置）。
		return ErrNotSupported
	}
	return mapError(err)
}
