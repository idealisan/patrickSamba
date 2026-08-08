package command

import (
	"errors"
	"io"
	"strings"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	register(wire.CommandSetInfo, true, true, handleSetInfo)
}

// handleSetInfo 处理 SMB2 SET_INFO（MS-SMB2 §3.3.5.21）。
func handleSetInfo(ctx *Context) error {
	req, err := wire.ParseSetInfoRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}

	switch req.InfoType {
	case wire.InfoTypeFile:
		err = setFileInfo(ctx, open, req)
	case wire.InfoTypeFileSystem:
		// 卷标/卷属性都不可改：共享的容量与标签来自宿主文件系统。
		err = status.AccessDenied
	case wire.InfoTypeSecurity:
		// 授权完全由配置决定（AGENTS.md §1.1 C8），不接受客户端下发的 ACL。
		// 回 ACCESS_DENIED 而不是 NOT_SUPPORTED：Windows 客户端在
		// "属性→安全" 里改权限时，前者会正常提示，后者会弹出诡异错误。
		err = status.AccessDenied
	default:
		err = status.InvalidInfoClass
	}
	if err != nil {
		return err
	}

	ctx.Out = (&wire.SetInfoResponse{}).Append(ctx.Out)
	return nil
}

// setFileInfo 处理 InfoType=FILE 的各信息类。
func setFileInfo(ctx *Context, open *Open, req *wire.SetInfoRequest) error {
	if open.IsPipe() {
		// 管道只有 FilePipeInformation 之类，我们不支持修改。
		return status.InvalidInfoClass
	}
	if err := ctx.RequireWritable(); err != nil {
		return err
	}
	h := open.Handle
	if h == nil {
		return status.FileClosed
	}

	switch req.FileClass() {
	case wire.FileBasicInformation:
		return setBasicInfo(open, h, req.Buffer)

	case wire.FileEndOfFileInformation:
		return setEndOfFile(open, h, req.Buffer)

	case wire.FileAllocationInformation:
		return setAllocation(open, h, req.Buffer)

	case wire.FileDispositionInformation:
		return setDisposition(open, req.Buffer)

	case wire.FileRenameInformation:
		return setRename(ctx, open, req.Buffer)

	case wire.FileLinkInformation:
		// 硬链接：VFS 没有这个能力，明确回不支持而不是假装成功。
		return status.NotSupported

	case wire.FilePositionInformation:
		// SMB2 读写都带显式偏移，句柄没有隐式游标。
		// 客户端（尤其是 Windows）仍会下发它，回成功即可，忽略取值。
		if len(req.Buffer) < 8 {
			return status.InfoLengthMismatch
		}
		return nil

	case wire.FileModeInformation:
		if len(req.Buffer) < 4 {
			return status.InfoLengthMismatch
		}
		return nil

	case wire.FileShortNameInformation:
		// 我们不维护 8.3 短名。
		return status.NotSupported

	case wire.FileValidDataLengthInformation:
		// 仅对非稀疏文件有意义，且需要 MANAGE_VOLUME 权限。
		return status.NotSupported

	case wire.FileFullEaInformation:
		return status.NotSupported

	default:
		ctx.Log.Debug("未实现的 set file info class", "class", req.FileClass())
		return status.InvalidInfoClass
	}
}

// setBasicInfo 处理 FileBasicInformation（MS-FSCC §2.4.7）。
//
// 时间字段有三种取值：
//   - 0                  —— 不修改；
//   - 0xFFFFFFFFFFFFFFFF —— 冻结该时间戳（后续 IO 不再更新），我们按"不修改"处理；
//   - 其他               —— 设置为该 FILETIME。
//
// FileAttributes 为 0 表示不修改属性。
func setBasicInfo(open *Open, h vfs.Handle, buf []byte) error {
	if len(buf) < wire.FileBasicInfoSize {
		return status.InfoLengthMismatch
	}
	info, err := wire.ParseFileBasicInfo(buf)
	if err != nil {
		return status.InvalidParameter
	}
	if open.GrantedAccess&wire.FileWriteAttributes == 0 {
		return status.AccessDenied
	}

	var (
		attr vfs.Attr
		mask vfs.AttrMask
	)
	if vfs.FiletimeIsSet(info.CreationTime) {
		attr.CreateTime = vfs.FiletimeToTime(info.CreationTime)
		mask |= vfs.AttrCreateTime
	}
	if vfs.FiletimeIsSet(info.LastAccessTime) {
		attr.AccessTime = vfs.FiletimeToTime(info.LastAccessTime)
		mask |= vfs.AttrAccessTime
	}
	if vfs.FiletimeIsSet(info.LastWriteTime) {
		attr.WriteTime = vfs.FiletimeToTime(info.LastWriteTime)
		mask |= vfs.AttrWriteTime
	}
	if vfs.FiletimeIsSet(info.ChangeTime) {
		attr.ChangeTime = vfs.FiletimeToTime(info.ChangeTime)
		mask |= vfs.AttrChangeTime
	}
	if info.FileAttributes != 0 {
		attr.FileAttributes = uint32(info.FileAttributes)
		mask |= vfs.AttrFileAttributes
	}
	if mask == 0 {
		// 全是"不修改"，客户端只是在探测能力。
		return nil
	}
	if err := h.SetAttr(&attr, mask); err != nil {
		return status.FromVFSError(err)
	}
	return nil
}

// setEndOfFile 处理 FileEndOfFileInformation（MS-FSCC §2.4.13）：截断或扩展。
func setEndOfFile(open *Open, h vfs.Handle, buf []byte) error {
	if len(buf) < wire.FileEndOfFileInfoSize {
		return status.InfoLengthMismatch
	}
	eof, err := wire.ParseFileEndOfFileInfo(buf)
	if err != nil {
		return status.InvalidParameter
	}
	if eof < 0 {
		return status.InvalidParameter
	}
	if open.IsDir {
		return status.InvalidParameter
	}
	// 改长度算写操作：MS-FSCC 要求 FILE_WRITE_DATA。
	if open.GrantedAccess&(wire.FileWriteData|wire.FileAppendData) == 0 {
		return status.AccessDenied
	}
	if err := h.Truncate(eof); err != nil {
		return status.FromVFSError(err)
	}
	return nil
}

// setAllocation 处理 FileAllocationInformation（MS-FSCC §2.4.4）。
//
// 语义与 EndOfFile 不同：它只调整**分配**长度。
// 收缩到小于当前 EOF 时才需要真的截断；扩大是"预分配提示"，
// 交给 VFS 的 AttrAlloc（本地实现会做 fallocate，失败则忽略）。
func setAllocation(open *Open, h vfs.Handle, buf []byte) error {
	if len(buf) < wire.FileAllocationInfoSize {
		return status.InfoLengthMismatch
	}
	alloc, err := wire.ParseFileAllocationInfo(buf)
	if err != nil {
		return status.InvalidParameter
	}
	if alloc < 0 {
		return status.InvalidParameter
	}
	if open.IsDir {
		return status.InvalidParameter
	}
	if open.GrantedAccess&(wire.FileWriteData|wire.FileAppendData) == 0 {
		return status.AccessDenied
	}

	attr, serr := h.Stat()
	if serr != nil {
		return status.FromVFSError(serr)
	}
	if alloc < attr.Size {
		// 分配长度小于逻辑长度：EOF 必须跟着缩。
		if err := h.Truncate(alloc); err != nil {
			return status.FromVFSError(err)
		}
		return nil
	}
	if err := h.SetAttr(&vfs.Attr{Alloc: alloc}, vfs.AttrAlloc); err != nil {
		if errors.Is(err, vfs.ErrNotSupported) {
			// 预分配只是优化，后端不支持不算失败。
			return nil
		}
		return status.FromVFSError(err)
	}
	return nil
}

// setDisposition 处理 FileDispositionInformation（MS-FSCC §2.4.11）：
// 置位表示"关闭时删除"。
func setDisposition(open *Open, buf []byte) error {
	if len(buf) < wire.FileDispositionInfoSize {
		return status.InfoLengthMismatch
	}
	del, err := wire.ParseFileDispositionInfo(buf)
	if err != nil {
		return status.InvalidParameter
	}
	if del {
		if open.GrantedAccess&wire.Delete == 0 {
			return status.AccessDenied
		}
		// 非空目录不能删：这里就要拒绝，等到 CLOSE 才发现就来不及回错了
		// （CLOSE 的响应里没有位置报告删除失败）。
		if open.IsDir {
			if err := dirIsEmpty(open); err != nil {
				return err
			}
		}
	}
	open.SetDeleteOnClose(del)
	return nil
}

// dirIsEmpty 在目录非空时返回 STATUS_DIRECTORY_NOT_EMPTY。
func dirIsEmpty(open *Open) error {
	h := open.Handle
	if h == nil {
		return status.FileClosed
	}
	// 从头扫一条即可判断；扫完后复位枚举状态，避免污染
	// 客户端正在进行的 QUERY_DIRECTORY 游标。
	entries, err := h.ReadDir("*", true, 1)
	open.ResetDirScan()
	if err != nil && !errors.Is(err, io.EOF) {
		return status.FromVFSError(err)
	}
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		return status.DirectoryNotEmpty
	}
	return nil
}

// setRename 处理 FileRenameInformation（MS-FSCC §2.4.37.2）。
func setRename(ctx *Context, open *Open, buf []byte) error {
	info, err := wire.ParseFileRenameInfo(buf)
	if err != nil {
		return status.InvalidParameter
	}
	if info.RootDirectory != 0 {
		// SMB2 里 RootDirectory 必须为 0（MS-FSCC §2.4.37.2）。
		return status.InvalidParameter
	}
	if open.GrantedAccess&wire.Delete == 0 {
		return status.AccessDenied
	}

	dst, terr := renameTarget(info.FileName)
	if terr != nil {
		return status.ObjectPathSyntaxBad
	}
	if dst == "" {
		return status.ObjectNameInvalid
	}
	if dst == open.Path {
		// 改名到自己：直接成功，别去动文件系统。
		return nil
	}

	fs := ctx.Tree.FS()
	if fs == nil {
		return status.NetworkNameDeleted
	}
	if err := fs.Rename(open.Path, dst, info.ReplaceIfExists); err != nil {
		return status.FromVFSError(err)
	}

	// 句柄在改名后仍然有效，后续 QUERY_INFO 要能回新路径。
	open.Path = dst
	return nil
}

// renameTarget 把线格式的目标名规范化成共享内相对路径。
//
// 线格式用反斜杠分隔、相对共享根、不含前导反斜杠（但真实客户端会带，
// 所以要容忍）。规范化与越界校验交给 vfs.CleanPath（AGENTS.md §8）。
func renameTarget(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimPrefix(name, "/")
	if strings.IndexByte(name, ':') >= 0 {
		// 带 stream 名的改名（重命名 ADS）我们不支持，拒绝而不是误删主流。
		return "", vfs.ErrNotSupported
	}
	return vfs.CleanPath(name)
}
