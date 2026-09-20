package native

// migration.go —— oscap.MetadataMigration 的 native 实现：**无操作**。
//
// # 为什么是无操作，而不是「还没做」
//
// native 侧的全部元数据都长在**宿主对象本身**上，跟着目录项走：
//
//	能力            载体                          rename/unlink 时
//	xattr           inode 的扩展属性               内核自动跟随/随 inode 消失
//	named_stream    POSIX: xattr / NTFS: ADS      同上
//	sparse          无持久状态（SEEK_HOLE 现查）    没有账可迁
//	stable_file_id  inode / NTFS FileIndex         天然跨改名稳定
//	creation_time   statx BTIME / birthtime / NTFS 内核维护
//	dos_attributes  （仅 Windows）NTFS 属性字       随文件走
//
// Samba 的 user.DOSATTRIB 存在 inode 的 xattr 里，靠的正是同一性质
// （source3/smbd/dosmode.c set_ea_dos_attribute）。所以这里没有账可搬、
// 没有账可清 —— 返回 nil 是**如实陈述**，不是偷懒。
//
// # 边界
//
// 按「路径」记账的只有 builtin 的旁路库。若将来某项 native 实现引入了任何
// 按路径的持久状态，本文件的注释与实现必须一起重写 —— 别只改实现不改文档，
// 那会让下一个读者以为无操作仍然是正确答案。

import "github.com/idealisan/patrickSamba/internal/oscap"

type noopMigration struct{}

func (noopMigration) RenameMetadata(oldPath, newPath string) error { return nil }
func (noopMigration) DeleteMetadata(path string) error             { return nil }

var _ oscap.MetadataMigration = noopMigration{}
