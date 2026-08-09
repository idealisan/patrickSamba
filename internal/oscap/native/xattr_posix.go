//go:build linux || darwin

package native

// xattr_posix.go —— oscap.Xattr 的原生实现：POSIX 扩展属性。
//
// 落盘的就是宿主机上真正的扩展属性，`getfattr -d` / `xattr -l` 看到的
// 和 SMB 客户端看到的是同一份东西 —— 这正是 native 相对 builtin 的价值。

import (
	"strings"

	"golang.org/x/sys/unix"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// posixXattr 实现 oscap.Xattr。
//
// 无状态（除只读标志外），可安全并发使用：每个方法都自带 Ref。
type posixXattr struct {
	readOnly bool
}

var _ oscap.Xattr = (*posixXattr)(nil)

func (x *posixXattr) GetXattr(ref oscap.Ref, name string) ([]byte, error) {
	full, err := encodeXattrName(name)
	if err != nil {
		return nil, err
	}
	return getXattrRaw(ref, full)
}

// getXattrRaw 按**宿主机**属性名读值，供 GetXattr 与命名流共用。
func getXattrRaw(ref oscap.Ref, full string) ([]byte, error) {
	// 两趟：先问长度再分配。不能一次开个大 buffer 了事 ——
	// 资源叉可以有几十 KiB，猜大了浪费、猜小了 ERANGE。
	size, err := xattrGet(ref, full, nil)
	if err != nil {
		return nil, mapPosixErr(err)
	}
	if size == 0 {
		// 「存在但为空」与「不存在」是两回事（ports.go 明文规定）：
		// 走到这里说明属性确实存在，返回**非 nil 的空切片**。
		return []byte{}, nil
	}
	buf := make([]byte, size)
	got, err := xattrGet(ref, full, buf)
	if err != nil {
		return nil, mapPosixErr(err)
	}
	if got > len(buf) {
		// 两趟之间被别人写大了：防御性截断，绝不越界切片
		// （AGENTS.md §5「先校验长度再切片」）。
		got = len(buf)
	}
	return buf[:got], nil
}

func (x *posixXattr) SetXattr(ref oscap.Ref, name string, value []byte) error {
	if x.readOnly {
		return oscap.ErrReadOnly
	}
	full, err := encodeXattrName(name)
	if err != nil {
		return err
	}
	return mapPosixErr(xattrSet(ref, full, value))
}

func (x *posixXattr) RemoveXattr(ref oscap.Ref, name string) error {
	if x.readOnly {
		return oscap.ErrReadOnly
	}
	full, err := encodeXattrName(name)
	if err != nil {
		return err
	}
	return mapPosixErr(xattrRemove(ref, full))
}

func (x *posixXattr) ListXattr(ref oscap.Ref) ([]string, error) {
	raw, err := listXattrRaw(ref)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		// 别的命名空间（security./system./trusted.）与命名流的落盘属性
		// 都在这里被过滤掉，见 decodeXattrName 的说明。
		if name, ok := decodeXattrName(r); ok {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		// 「一个都没有」是合法答案而不是错误（ports.go）。
		return nil, nil
	}
	return out, nil
}

// listXattrRaw 列出**宿主机**属性名（未解码、未过滤），供命名流枚举共用。
func listXattrRaw(ref oscap.Ref) ([]string, error) {
	size, err := xattrList(ref, nil)
	if err != nil {
		return nil, mapPosixErr(err)
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	got, err := xattrList(ref, buf)
	if err != nil {
		return nil, mapPosixErr(err)
	}
	if got > len(buf) {
		got = len(buf)
	}
	// 内核返回的是一串 NUL 结尾的名字拼接而成。
	var out []string
	for _, raw := range strings.Split(string(buf[:got]), "\x00") {
		if raw != "" {
			out = append(out, raw)
		}
	}
	return out, nil
}

// --- 系统调用薄封装：有 fd 走 f* 版，没有就按路径 --------------------------
//
// 注意路径版用的是 unix.Getxattr（跟随符号链接）而不是 Lgetxattr：
// 与 vfs/xattr_unix.go 保持一致。vfs 已经在打开阶段做过符号链接逃逸校验，
// 这里换成 L 版反而会让「共享内合法的符号链接」上的属性读不到。

func xattrGet(ref oscap.Ref, name string, dest []byte) (int, error) {
	if fd, ok := refFD(ref); ok {
		return unix.Fgetxattr(fd, name, dest)
	}
	return unix.Getxattr(ref.Path, name, dest)
}

func xattrSet(ref oscap.Ref, name string, value []byte) error {
	if fd, ok := refFD(ref); ok {
		return unix.Fsetxattr(fd, name, value, 0)
	}
	return unix.Setxattr(ref.Path, name, value, 0)
}

func xattrRemove(ref oscap.Ref, name string) error {
	if fd, ok := refFD(ref); ok {
		return unix.Fremovexattr(fd, name)
	}
	return unix.Removexattr(ref.Path, name)
}

func xattrList(ref oscap.Ref, dest []byte) (int, error) {
	if fd, ok := refFD(ref); ok {
		return unix.Flistxattr(fd, dest)
	}
	return unix.Listxattr(ref.Path, dest)
}
