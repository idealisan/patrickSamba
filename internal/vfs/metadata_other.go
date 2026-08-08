//go:build !windows

package vfs

// Linux/macOS 的宿主文件系统原生就有 uid/gid/mode，不需要旁路存储。
// 返回 nil 而不是一个 noop 实现：LocalFS 里所有调用点都判了 nil，
// 这样连接口方法调用的开销都没有（AGENTS.md §5 P7 要求零开销）。
func openMetadataStore(root, metadataPath string) (MetadataStore, error) {
	return nil, nil
}
