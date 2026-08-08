//go:build windows

package vfs

// Windows 上 NTFS 没有 POSIX 的 uid/gid/mode，需要旁路存储
// （AGENTS.md §5 P7）。
//
// TODO(P1): 用纯 Go 的嵌入式 KV（go.etcd.io/bbolt，BSD 许可、无 CGO）实现，
// 数据库路径取自 config 的 metadata_path。在它落地之前先返回 nil ——
// 效果是 Windows 上属主固定显示为配置里的 UID/GID 标签、权限位为 0，
// 文件共享本身完全可用（授权由配置决定，不依赖这些字段，见 AGENTS.md §1.1）。
func openMetadataStore(root, metadataPath string) (MetadataStore, error) {
	return nil, nil
}
