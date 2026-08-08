package vfs

// metadata.go —— 「宿主文件系统表达不了的元数据」的旁路存储接口
// （AGENTS.md §5 P7）。
//
// 只有 **POSIX 属主/权限位（uid/gid/mode）** 需要旁路存储。
// 注意 NTFS 原生就支持 alternate data stream、稀疏文件、稳定 FileID、
// 真实创建时间和 DOS 属性位 —— 这些**不需要**旁路，直接用原生能力。
//
// Linux/macOS 上 openMetadataStore 返回 nil，LocalFS 里所有相关调用点
// 都先判 nil，编译后是零开销（AGENTS.md §5 P7 要求「只在 Windows 编译进来」）。

// Metadata 是一个对象的旁路元数据。
type Metadata struct {
	UID  uint32
	GID  uint32
	Mode uint32 // POSIX 权限位（低 12 位）
}

// MetadataStore 是旁路元数据的持久化后端。
//
// key 是**共享内相对路径**（'/' 分隔，不以 '/' 开头）。
// 用路径而不是 FileID 做 key 的理由：NTFS 的 FileID 虽然稳定，
// 但要拿到它必须先打开文件，而 Remove/Rename 之后文件已经不在了，
// 没法用它去清理陈旧记录。路径 key 让 Rename/Delete 能同步维护。
//
// 实现必须并发安全。
type MetadataStore interface {
	Get(key string) (Metadata, bool)
	Put(key string, md Metadata) error
	Delete(key string) error
	// Rename 把 oldKey（及其子树，如果是目录）迁移到 newKey。
	Rename(oldKey, newKey string) error
	Close() error
}
