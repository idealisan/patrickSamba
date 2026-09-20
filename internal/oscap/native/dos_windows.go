//go:build windows

package native

// dos_windows.go —— Windows 上的 DOS 属性位：文件系统原生就存着这些位。

import (
	"fmt"

	"golang.org/x/sys/windows"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// winSettableDOS 是允许客户端设置的位（MS-FSCC §2.6）。
//
// 与 vfs.settableDOSAttributes **必须保持一致**，但这里是**平行定义**
// 而不是 import vfs —— oscap 位于 vfs 之下，反向依赖会破坏 §5 的分层
// （同 vfs/attr.go 相对 wire 层的处理）。
//
// 不在名单里的位分两类，都不接受设置：
//   - 客观事实位：DIRECTORY / SPARSE / REPARSE / COMPRESSED / ENCRYPTED —— 由
//     文件系统自己决定，清掉 DIRECTORY 位会让 Windows 认为这不再是目录。
//   - NORMAL：它的定义是"没有别的位"，只能单独出现，不能与别的位一起设。
const winSettableDOS = windows.FILE_ATTRIBUTE_READONLY |
	windows.FILE_ATTRIBUTE_HIDDEN |
	windows.FILE_ATTRIBUTE_SYSTEM |
	windows.FILE_ATTRIBUTE_ARCHIVE |
	windows.FILE_ATTRIBUTE_TEMPORARY |
	windows.FILE_ATTRIBUTE_OFFLINE |
	windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED

// winObjectiveDOS 是由文件系统客观事实推导、**由 vfs 层负责合成**的位
// （ports.go 明文列举的三个）。读的时候把它们剔掉，避免与 vfs 的合成结果
// 重复表达同一件事 —— 两处都说了算的字段，出现分歧时没人知道该信谁。
const winObjectiveDOS = windows.FILE_ATTRIBUTE_DIRECTORY |
	windows.FILE_ATTRIBUTE_SPARSE_FILE |
	windows.FILE_ATTRIBUTE_REPARSE_POINT

// winDOS 用 GetFileAttributes / SetFileAttributes 实现 oscap.DOSAttributes。
type winDOS struct {
	readOnly bool
}

var _ oscap.DOSAttributes = (*winDOS)(nil)

// DOSAttributes 读取 DOS 属性位。
//
// # 为什么这里**永远不返回 ErrNotFound**
//
// ports.go 说"从来没有被设置过时返回 (0, ErrNotFound)，让上层走它的合成逻辑"。
// 那条是为 builtin 的旁路存储写的 —— 旁路里确实存在"没这条记录"的状态。
// Windows 上不存在这个状态：属性字是文件元数据的固有部分，任何文件都有。
//
// 于是这里如实返回读到的值，包括"可设置位一个都没有"时的 0 + nil。
// **不要**把 0 改判成 ErrNotFound：那会把上层推去跑 POSIX 约定的合成逻辑
// （点开头 → HIDDEN、属主无写权限 → READONLY），在 Windows 上是错的 ——
// 一个叫 `.gitignore` 的普通文件会凭空多出 HIDDEN 位，而且是**每次查询都多**。
func (d *winDOS) DOSAttributes(ref oscap.Ref) (uint32, error) {
	p, err := windows.UTF16PtrFromString(ref.Path)
	if err != nil {
		return 0, fmt.Errorf("oscap/native: 路径 %q 非法: %w", ref.Path, oscap.ErrInvalidArg)
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return 0, mapWinErr(err)
	}
	if attrs == windows.INVALID_FILE_ATTRIBUTES {
		// GetFileAttributes 失败时按约定既置 error 又回这个哨兵值，
		// 上面那个分支通常已经拦住。多这一道是防止某天错误被吞掉后，
		// 0xFFFFFFFF 被当成"所有位都置上了"交给客户端。
		return 0, fmt.Errorf("oscap/native: %q 属性不可读: %w",
			ref.Path, oscap.ErrNotSupported)
	}
	return attrs &^ winObjectiveDOS, nil
}

// SetDOSAttributes 设置 DOS 属性位。
//
// 读-改-写，只动 winSettableDOS 那几位；客观事实位原样保留。
// 调用方按约定已经剔过一遍，这里再剔一次是纵深防御 —— 这一层的入参
// 最终源自网络报文，不该假设上游一定过滤干净（AGENTS.md §8）。
func (d *winDOS) SetDOSAttributes(ref oscap.Ref, attrs uint32) error {
	if d.readOnly {
		return fmt.Errorf("oscap/native: 只读挂载: %w", oscap.ErrReadOnly)
	}
	p, err := windows.UTF16PtrFromString(ref.Path)
	if err != nil {
		return fmt.Errorf("oscap/native: 路径 %q 非法: %w", ref.Path, oscap.ErrInvalidArg)
	}
	cur, err := windows.GetFileAttributes(p)
	if err != nil {
		return mapWinErr(err)
	}
	next := (cur &^ winSettableDOS) | (attrs & winSettableDOS)
	if next == 0 {
		// SetFileAttributes(0) 是非法调用。"什么位都没有"在 Win32 里的
		// 表示法是 FILE_ATTRIBUTE_NORMAL，不是 0。
		next = windows.FILE_ATTRIBUTE_NORMAL
	}
	if next == cur {
		return nil
	}
	if err := windows.SetFileAttributes(p, next); err != nil {
		return mapWinErr(err)
	}
	return nil
}
