package builtin

// fileid.go —— CapStableFileID 的 builtin 实现。
//
// # 两级取值，先真后补
//
//  1. 宿主 stat 给得出 inode 就直接用它。inode 属于 C9 允许的「普通 stat」范畴，
//     而且它**天然跨重命名不变** —— 这正是 ports.go 那条硬契约要的东西。
//  2. 拿不到 inode（Windows 的 os.FileInfo 不带、某些文件系统报 0）才退到
//     库里分配一个号，按路径记账。
//
// # 唯一性怎么保证
//
// 分配号从 2^63 起（idFallbackBase），真实 inode 落在 [1, 2^63) —— 两个号段
// 不相交，所以同一个卷上「有 inode 的对象」与「靠库分配的对象」不会撞号。
// inode ≥ 2^63 的异常值直接判为不可用，转走分配路径，而不是冒险截断。
//
// # 诚实说明：分配路径**不跨重命名**
//
// 它按路径记账，文件改名后会拿到一个新号。这是普通文件系统上无法回避的：
// 没有 inode 就没有任何可锚定的对象身份。之所以仍然返回值而不是 ErrNotSupported，
// 是因为「稳定且唯一」这两条在文件不改名时都成立，而改名场景由 native 适配器
// （Windows 上走 GetFileInformationByHandle）覆盖。落到这条路径的是
// 「Windows + portable 模式」与「inode 不可用的异种文件系统」，两者都极少见。
// 真到了连库都写不进去（只读共享且从未记过账）的时候，如实返回 ErrNotSupported ——
// **不发一个会变的号**，因为一个会变的 FileID 比没有 FileID 更糟。

import (
	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// FileID 实现 oscap.StableFileID。
func (a *adapter) FileID(ref oscap.Ref) (uint64, error) {
	fi, err := a.statFile(ref)
	if err != nil {
		return 0, err
	}
	if ino, ok := a.inode(fi); ok {
		return ino, nil
	}

	key := a.st.objKey(ref.Path)
	id, ok, err := a.st.lookupFileID(key)
	if err != nil {
		return 0, err
	}
	if ok {
		return id, nil
	}
	if a.st.readOnly {
		// 只读共享不许建旁路记录，也就发不出一个可持久的号。
		return 0, oscap.ErrNotSupported
	}
	return a.st.assignFileID(key)
}
