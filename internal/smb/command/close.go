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
			return nil, status.FileClosed
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
	// vfsOwnsDelete 时底层句柄已在 open.close() 里删过，命令层不再删。
	delPath, doDelete := open.Path, open.DeleteOnClose() && open.Handle != nil && !open.vfsOwnsDelete
	open.close()

	// delete-on-close 的实际删除必须在句柄关闭之后做
	// （Windows 上还持有 fd 时删不掉）。
	if doDelete {
		if err := ctx.deleteOnClose(delPath); err != nil {
			// 删除失败不影响 CLOSE 本身成功 —— 客户端已经认为句柄没了，
			// 回错误只会让它困惑。记日志即可。
			ctx.Log.Warn("delete-on-close 删除失败", "path", delPath, "err", err)
		}
	}

	ctx.Out = resp.Append(ctx.Out)
	return nil
}

// deleteOnClose 执行 FILE_DELETE_ON_CLOSE / FileDispositionInformation
// 约定的删除动作。
func (c *Context) deleteOnClose(path string) error {
	if err := c.RequireWritable(); err != nil {
		return err
	}
	fs := c.Tree.FS()
	if fs == nil {
		return status.NetworkNameDeleted
	}
	return fs.Remove(path)
}
