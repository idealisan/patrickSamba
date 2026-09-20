package builtin

// migration.go —— oscap.MetadataMigration 的 builtin 实现（B5）。
//
// # 为什么需要这个能力
//
// builtin 的一切元数据都按 pathKey 记在旁路库里，而宿主的 rename/unlink
// 对此一无所知。缺了迁移/清理，后果有两层：
//   - 改名后客户端设置过的创建时间与 DOS 位「丢」（读路径按新路径查不到）；
//   - **更糟**：旧路径的残留记录会安到之后新建在该路径上的无关文件头上
//     （跨对象元数据泄漏，bh3 F3）。
//
// Samba 把这些元数据存在 inode 的 user.DOSATTRIB xattr 里，rename(2) 天然
// 跟随、unlink 随 inode 消失，不存在这个问题。本包用「迁移/清理前缀键」
// 达成同样的可观测语义。

import (
	"github.com/idealisan/patrickSamba/internal/oscap"
)

// RenameMetadata 实现 oscap.MetadataMigration：把 oldPath 名下全部旁路记录
// （六个 bucket、含具名子项与整棵子树）原子地搬进 newPath。
func (a *adapter) RenameMetadata(oldPath, newPath string) error {
	return a.st.renameKeys(oldPath, newPath)
}

// DeleteMetadata 实现 oscap.MetadataMigration：清掉 path 名下全部旁路记录。
// 幂等 —— 记录本来就不存在时返回 nil 而不是 ErrNotFound：
// 调用方刚在宿主上完成 unlink，旁路里没账是常态而非异常。
func (a *adapter) DeleteMetadata(path string) error {
	return a.st.deleteKeys(path)
}

var _ oscap.MetadataMigration = (*adapter)(nil)
