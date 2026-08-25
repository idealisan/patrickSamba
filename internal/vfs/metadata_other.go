//go:build !windows

package vfs

// Linux/macOS 的宿主文件系统原生就有 uid/gid/mode，不需要旁路存储。
// 返回 nil 而不是一个 noop 实现：LocalFS 里所有调用点都判了 nil，
// 这样连接口方法调用的开销都没有（AGENTS.md §5 P7 要求零开销）。
//
// ⚠️ 注意：这里「不需要旁路」只针对 POSIX 属主/权限位（MetadataStore 的职责）。
// 创建时间 / DOS 属性位 / 稳定 FileID 这三项**不是**宿主原生一定能给的
// （POSIX 大多没有 btime、没有原生 DOS 位、inode 虽稳定但不是客户端要的语义），
// 它们现在统一走 oscap.Provider（builtin 档用旁路 KV 补出真实值，
// native 档用宿主能力）。这条路径与 meta 无关、始终可用，所以 non-Windows 上
// 这三项元数据**不再静默丢失**——AGENTS.md §1.2「已建成 ≠ 已生效」里点名的那块
// non-Windows 缺口已由 vfs-attr 接线闭合。
// instanceID 参数与 Windows 侧（metadata_windows.go）保持同一签名：
// 本平台用不到它，但签名不一致会让 LocalConfig.InstanceID 的下传在每个
// 平台文件里各写一遍调用点，容易漏。
func openMetadataStore(root, metadataPath, instanceID string) (MetadataStore, error) {
	return nil, nil
}
