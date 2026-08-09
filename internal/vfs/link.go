package vfs

// link.go —— 硬链接（SMB2 SET_INFO 的 FileLinkInformation，MS-FSCC §2.4.21.2）。
//
// 不需要 build tag：Go 的 os.Link 在 Windows 上走 CreateHardLinkW，
// 在 POSIX 上走 link(2)，语义一致（都不允许目录、都不能跨卷）。

import (
	"os"
	"path/filepath"
)

// Link 实现 HardLinker。
//
// 与 Rename 共用同一套路径处理：两端都走 Resolver.ResolveParent，
// 于是 "../"、绝对路径、指向共享外的软链在这里就被挡掉（AGENTS.md §8）。
func (l *LocalFS) Link(oldPath, newPath string, replace bool) error {
	if l.cfg.ReadOnly {
		return ErrReadOnly
	}

	// 流路径（"file:AFP_Resource:$DATA"）不能作为链接的任何一端：
	// 流不是独立的文件系统对象，链接它没有意义。
	if _, stream, err := SplitStreamPath(oldPath); err != nil {
		return err
	} else if stream != "" {
		return ErrInvalidPath
	}
	if _, stream, err := SplitStreamPath(newPath); err != nil {
		return err
	} else if stream != "" {
		return ErrInvalidPath
	}

	oldDir, oldName, err := l.res.ResolveParent(oldPath)
	if err != nil {
		return err
	}
	newDir, newName, err := l.res.ResolveParent(newPath)
	if err != nil {
		return err
	}
	src := filepath.Join(oldDir, oldName)
	dst := filepath.Join(newDir, newName)

	fi, err := os.Lstat(src)
	if err != nil {
		return mapError(err)
	}
	if fi.IsDir() {
		// POSIX 的 link(2) 对目录返回 EPERM，Windows 的 CreateHardLink
		// 返回 ERROR_ACCESS_DENIED —— 两者都会被 mapError 收敛成
		// ErrPermission，而客户端真正需要知道的是「这是个目录」。
		return ErrIsDir
	}

	if src == dst {
		// 链到自己：Windows 在这里成功返回，不要去删了再建（会丢数据）。
		return nil
	}

	if _, err := os.Lstat(dst); err == nil {
		if !replace {
			return ErrExist
		}
		if err := os.Remove(dst); err != nil {
			return mapError(err)
		}
	} else if !os.IsNotExist(err) {
		return mapError(err)
	}

	// 跨设备（bind mount / 子卷挂载点让它在共享内部也可能真实发生）
	// 与「本文件系统不支持硬链接」都由 mapErrno 收敛成 ErrNotSupported。
	if err := os.Link(src, dst); err != nil {
		return mapError(err)
	}
	// 硬链接是同一个 inode 的新名字，旁路元数据（uid/gid/mode）应当一致。
	l.copyMetadata(src, dst)
	return nil
}

// copyMetadata 把旁路存储里的记录复制到新名字上。
// meta 为 nil（Linux/macOS）时是空操作。
func (l *LocalFS) copyMetadata(src, dst string) {
	if l.meta == nil {
		return
	}
	relSrc, err1 := filepath.Rel(l.res.Root(), src)
	relDst, err2 := filepath.Rel(l.res.Root(), dst)
	if err1 != nil || err2 != nil {
		return
	}
	if md, ok := l.meta.Get(filepath.ToSlash(relSrc)); ok {
		_ = l.meta.Put(filepath.ToSlash(relDst), md)
	}
}
