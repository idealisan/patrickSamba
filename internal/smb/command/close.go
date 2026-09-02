package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	register(wire.CommandClose, true, true, handleClose)
}

// resolveOpen 按 FileId 定位句柄，处理复合链里的「复用上一个句柄」语义。
//
// MS-SMB2 §3.3.5.2.7：复合链中 FileId 为全 0xFF 时，表示复用本链中
// 上一条 CREATE 返回的句柄。这是 Windows 客户端「CREATE+QUERY+CLOSE
// 打包成一帧」的常用写法，不实现的话 Explorer 会非常慢。
func (c *Context) resolveOpen(fid wire.FileID) (*Open, error) {
	if fid.IsCompound() {
		if c.Chain.LastOpen == nil {
			// MS-SMB2 §3.3.5.2.7.2 原文："When the current operation
			// requires a FileId, and if the previous operation neither
			// contains nor generates a FileId, the server MUST fail the
			// current operation and all subsequent operations with
			// STATUS_INVALID_HANDLE."
			//
			// 注意这里**不是** STATUS_FILE_CLOSED —— 那是 §3.3.5.2.10
			// 针对"给了具体 FileId 但句柄已失效"的错误码。
			return nil, status.InvalidHandle
		}
		return c.Chain.LastOpen, nil
	}
	if c.Session == nil {
		return nil, status.UserSessionDeleted
	}
	o := c.Session.Open(fid.Persistent, fid.Volatile)
	if o == nil || o.Closed() {
		// MS-SMB2 §3.3.5.2.10：无效句柄回 STATUS_FILE_CLOSED。
		return nil, status.FileClosed
	}
	// 句柄属于别的树时不得跨用。
	if c.Tree != nil && o.Tree != c.Tree {
		return nil, status.InvalidParameter
	}
	return o, nil
}

// handleClose 处理 SMB2 CLOSE（MS-SMB2 §3.3.5.10）。
func handleClose(ctx *Context) error {
	req, err := wire.ParseCloseRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}

	resp := &wire.CloseResponse{}
	if req.PostQueryAttrib() {
		// 属性必须在关闭**之前**取，关掉就没了。
		resp.Flags = wire.CloseFlagPostQueryAttrib
		if h := open.Handle; h != nil {
			if attr, aerr := h.Stat(); aerr == nil {
				resp.CreationTime = vfs.TimeToFiletime(attr.CreateTime)
				resp.LastAccessTime = vfs.TimeToFiletime(attr.AccessTime)
				resp.LastWriteTime = vfs.TimeToFiletime(attr.WriteTime)
				resp.ChangeTime = vfs.TimeToFiletime(attr.ChangeTime)
				resp.AllocationSize = uint64(attr.Alloc)
				resp.EndOfFile = uint64(attr.Size)
				resp.FileAttributes = wire.FileAttributes(attr.FileAttributes)
			}
		}
	}

	// 从句柄表摘除并释放底层资源。
	ctx.Session.RemoveOpen(open.Volatile)
	if ctx.Chain.LastOpen == open {
		ctx.Chain.LastOpen = nil
	}
	// 句柄关闭即释放它持有的全部字节范围锁（MS-SMB2 §3.3.5.10）。
	// 不放会让锁表泄漏，并且永久挡住其他客户端。
	if open.Tree != nil && open.Tree.Share != nil {
		open.Tree.Share.locks.releaseAll(open.Path, open)
	}
	// vfsOwnsDelete 时底层句柄已在 open.close() 里删过，命令层不再删。
	delPath, doDelete := open.Path, open.DeleteOnClose() && open.Handle != nil && !open.vfsOwnsDelete
	// Stream 字段在 close 之后仍然可读（close 只清 Handle/Pipe），
	// 但这里先取快照更直观：删除对象由「打开时是什么」决定。
	delStream := open.Stream
	// vfsOwnsDelete 时删除发生在 open.close() 内部的 vfs 句柄关闭里，
	// 命令层不参与；为了让 CHANGE_NOTIFY 也覆盖这条路，这里单独记一笔。
	vfsDeleted := open.DeleteOnClose() && open.Handle != nil && open.vfsOwnsDelete
	isDir := open.IsDir
	open.close()

	// delete-on-close 的实际删除必须在句柄关闭之后做
	// （Windows 上还持有 fd 时删不掉）。
	if doDelete {
		if err := ctx.deleteOnClose(delPath, delStream); err != nil {
			// 删除失败不影响 CLOSE 本身成功 —— 客户端已经认为句柄没了，
			// 回错误只会让它困惑。记日志即可。
			ctx.Log.Warn("delete-on-close 删除失败", "path", delPath, "stream", delStream, "err", err)
		} else if delStream == "" {
			// 变更记账（CHANGE_NOTIFY）：只记**基础对象**的删除。
			// 删一个命名流不构成目录项变更，FILE_NOTIFY_INFORMATION 里
			// 也没有"删流"对应的 Action（那属于 STREAM_NAME 过滤位的范畴，
			// 而删除流的可见后果是文件内容变了，不是目录里少一项）。
			ctx.notifyHub().notifyRemoved(delPath, isDir)
		}
	}
	if vfsDeleted && delStream == "" {
		// vfs 侧的删除没有失败通路可查（vfsOwnsDelete 时命令层不参与删除），
		// 按"已删"记账：FILE_DELETE_ON_CLOSE 的语义就是无条件删。
		ctx.notifyHub().notifyRemoved(delPath, isDir)
	}

	ctx.Out = resp.Append(ctx.Out)
	return nil
}

// deleteOnClose 执行 FILE_DELETE_ON_CLOSE / FileDispositionInformation
// 约定的删除动作。
//
// 粒度按打开的对象定：流句柄（stream != ""）只删那一个流，基础文件不动。
// Samba 对照：streams_xattr_unlinkat()
// （source3/modules/vfs_streams_xattr.c:1056-1110）对命名流只清对应 xattr；
// 对 ADS 句柄设 FileDispositionInformation（MS-FSCC §2.4.11）在 Windows 上
// 同样只删该流。历史上这里曾一律 fs.Remove(open.Path)，客户端删一个流
// 会把整个基础文件连带所有流一起删掉。
func (c *Context) deleteOnClose(path, stream string) error {
	if err := c.RequireWritable(); err != nil {
		return err
	}
	fs := c.Tree.FS()
	if fs == nil {
		return status.NetworkNameDeleted
	}
	if stream != "" {
		sr, ok := fs.(vfs.StreamRemover)
		if !ok {
			return status.NotSupported
		}
		return sr.RemoveStream(path, stream)
	}
	return fs.Remove(path)
}
