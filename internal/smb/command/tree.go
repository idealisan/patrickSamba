package command

import (
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

// Tree 是一个树连接（MS-SMB2 §3.3.1.10 TreeConnect）：会话 × 共享。
//
// TreeId 在**会话内**唯一。跨会话使用别人的 TreeId 必须失败 ——
// 这由 dispatch 中「先按 SessionId 定位 Session，再在 Session 内查 TreeId」
// 的查找顺序天然保证。
type Tree struct {
	// ID 是 TreeId，会话内唯一且非 0。
	ID uint32

	// Share 是本树连接指向的共享。
	Share *Share

	// Session 是所属会话。
	Session *Session
}

// FS 返回该树的文件系统后端。IPC$ 返回 nil。
func (t *Tree) FS() vfs.FileSystem {
	if t == nil || t.Share == nil {
		return nil
	}
	return t.Share.FS
}

// Writable 报告本树是否允许写操作。
func (t *Tree) Writable() bool {
	return t != nil && t.Share != nil && t.Share.WritableFor()
}

// IsIPC 报告本树是否连接到 IPC$。
func (t *Tree) IsIPC() bool { return t != nil && t.Share != nil && t.Share.IsIPC() }
