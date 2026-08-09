package builtin

// file.go —— 对「普通文件」的最小访问封装。
//
// 本包只用 C9 明确允许的那一套：open/read/write/seek/stat。没有 fallocate、
// 没有 xattr、没有 ADS、没有 statx —— 那些是 native 适配器的活。

import (
	"errors"
	"io/fs"
	"os"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// openRead / openWrite 的取用次序：**先按路径开，拿不到才用 Ref.Handle**。
//
// 为什么不反过来（直觉上句柄更快）：Ref.Handle 的打开意图是**未知**的 ——
// SMB 客户端完全可能只申请了写权限，此时拿它去 ReadAt 会失败，而失败发生在
// 我们已经开始扫描空洞之后，只能退化成「整段都算已分配」，白白丢掉信息。
// 按路径开销只多一次 open，换来的是意图明确的句柄。Ref.Path 是必填字段，
// 这条路总是可走；句柄留作兜底（路径已被 rename/unlink 的窗口期）。

func (a *adapter) openRead(ref oscap.Ref) (f *os.File, closeFn func(), err error) {
	if h, e := os.Open(ref.Path); e == nil {
		return h, func() { _ = h.Close() }, nil
	} else if ref.Handle == nil {
		return nil, nil, mapPathError(e)
	}
	return ref.Handle, func() {}, nil
}

func (a *adapter) openWrite(ref oscap.Ref) (f *os.File, closeFn func(), err error) {
	if h, e := os.OpenFile(ref.Path, os.O_WRONLY, 0); e == nil {
		return h, func() { _ = h.Close() }, nil
	} else if ref.Handle == nil {
		return nil, nil, mapPathError(e)
	}
	return ref.Handle, func() {}, nil
}

// statFile 取对象的元信息，路径优先、句柄兜底。
func (a *adapter) statFile(ref oscap.Ref) (os.FileInfo, error) {
	if fi, err := os.Stat(ref.Path); err == nil {
		return fi, nil
	} else if ref.Handle == nil {
		return nil, mapPathError(err)
	}
	fi, err := ref.Handle.Stat()
	if err != nil {
		return nil, mapPathError(err)
	}
	return fi, nil
}

// fileSize 返回对象的逻辑长度。
func (a *adapter) fileSize(ref oscap.Ref) (int64, error) {
	fi, err := a.statFile(ref)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// mapPathError 把 os 层错误翻成 oscap sentinel。
//
// 只翻**语义明确**的两种；其余原样上抛，别把不认识的错误一律塞进某个 sentinel ——
// 那会让上层看到一个自信但错误的结论（本项目在错误映射上栽过这个坑）。
func mapPathError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return oscap.ErrNotFound
	case errors.Is(err, fs.ErrInvalid):
		return oscap.ErrInvalidArg
	default:
		return err
	}
}
