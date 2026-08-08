package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	register(wire.CommandQueryInfo, true, true, handleQueryInfo)
}

// 文件系统能力宣告（MS-FSCC §2.5.1 FileFsAttributeInformation）。
//
// 只宣告我们真正做得到的。谎报能力（例如宣告支持 ACL / 压缩 / 加密）
// 会让客户端发我们处理不了的请求；漏报则会让它保守降级。
const fsAttributes = wire.FileCasePreservedNames |
	wire.FileUnicodeOnDisk |
	wire.FileSupportsSparseFiles

// fsName 是回给客户端的文件系统名。
//
// 报 "NTFS" 是有意的兼容取舍：Windows 与 macOS 会根据这个字符串决定
// 是否启用 alternate data stream、长文件名等特性；报别的名字会让
// 客户端保守降级（Samba 默认也报 NTFS）。
const fsName = "NTFS"

// handleQueryInfo 处理 SMB2 QUERY_INFO（MS-SMB2 §3.3.5.20）。
func handleQueryInfo(ctx *Context) error {
	req, err := wire.ParseQueryInfoRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}

	var buf []byte
	switch req.InfoType {
	case wire.InfoTypeFile:
		buf, err = queryFileInfo(ctx, open, req)
	case wire.InfoTypeFileSystem:
		buf, err = queryFsInfo(ctx, req)
	case wire.InfoTypeSecurity:
		// 不实现安全描述符：授权完全由配置决定（AGENTS.md §1.1 C8），
		// 回一个假的 SD 只会误导客户端。
		return status.NotSupported
	default:
		return status.InvalidInfoClass
	}
	if err != nil {
		return err
	}

	// 缓冲不足：MS-SMB2 §3.3.5.20 要求回 STATUS_INFO_LENGTH_MISMATCH
	// 并在响应里带上所需长度（wire 层已按 Buffer 长度写好）。
	if uint32(len(buf)) > req.OutputBufferLength {
		return status.InfoLengthMismatch
	}

	resp := &wire.QueryInfoResponse{Buffer: buf}
	out, aerr := resp.Append(ctx.Out)
	if aerr != nil {
		return status.InsuffServerResources
	}
	ctx.Out = out
	return nil
}

// queryFileInfo 生成 InfoType=FILE 的响应载荷。
func queryFileInfo(ctx *Context, open *Open, req *wire.QueryInfoRequest) ([]byte, error) {
	// 管道句柄没有文件属性；只有极少数类有意义，其余一律拒绝。
	if open.IsPipe() {
		return nil, status.InvalidInfoClass
	}

	h := open.Handle
	if h == nil {
		return nil, status.FileClosed
	}
	attr, err := h.Stat()
	if err != nil {
		return nil, status.FromVFSError(err)
	}

	switch req.FileClass() {
	case wire.FileBasicInformation:
		return basicInfo(attr).Encode(), nil

	case wire.FileStandardInformation:
		return standardInfo(open, attr).Encode(), nil

	case wire.FileNetworkOpenInformation:
		return wire.FileNetworkOpenInfo{
			CreationTime:   vfs.TimeToFiletime(attr.CreateTime),
			LastAccessTime: vfs.TimeToFiletime(attr.AccessTime),
			LastWriteTime:  vfs.TimeToFiletime(attr.WriteTime),
			ChangeTime:     vfs.TimeToFiletime(attr.ChangeTime),
			AllocationSize: attr.Alloc,
			EndOfFile:      attr.Size,
			FileAttributes: wire.FileAttributes(attr.FileAttributes),
		}.Encode(), nil

	case wire.FileAllInformation:
		return wire.FileAllInfo{
			Basic:       basicInfo(attr),
			Standard:    standardInfo(open, attr),
			IndexNumber: attr.FileID,
			AccessFlags: open.GrantedAccess,
			Name:        wirePath(open.Path),
		}.Encode(), nil

	case wire.FileInternalInformation:
		return wire.EncodeFileInternalInfo(attr.FileID), nil

	case wire.FileEaInformation:
		// 我们不支持 EA，EaSize 恒为 0。
		return wire.EncodeFileEaInfo(0), nil

	case wire.FileAccessInformation:
		return wire.EncodeFileAccessInfo(open.GrantedAccess), nil

	case wire.FilePositionInformation:
		// SMB2 的读写都带显式偏移，句柄没有隐式游标，恒为 0。
		return wire.EncodeFilePositionInfo(0), nil

	case wire.FileModeInformation:
		return wire.EncodeFileModeInfo(0), nil

	case wire.FileAlignmentInformation:
		return wire.EncodeFileAlignmentInfo(wire.FileByteAlignment), nil

	case wire.FileAttributeTagInformation:
		// 我们不产生重解析点，ReparseTag 恒为 0。
		return wire.EncodeFileAttributeTagInfo(
			wire.FileAttributes(attr.FileAttributes), 0), nil

	case wire.FileIdInformation:
		return wire.EncodeFileIDInfo(volumeSerial(ctx), attr.FileID), nil

	case wire.FileNameInformation, wire.FileNormalizedNameInformation:
		return wire.EncodeFileNameInfo(wirePath(open.Path)), nil

	case wire.FileAlternateNameInformation:
		// 8.3 短名。我们不生成短名，回长名本身是安全的做法。
		return wire.EncodeFileNameInfo(baseName(open.Path)), nil

	case wire.FileStreamInformation:
		return streamInfo(ctx, open)

	default:
		ctx.Log.Debug("未实现的 file info class", "class", req.FileClass())
		return nil, status.InvalidInfoClass
	}
}

// queryFsInfo 生成 InfoType=FILESYSTEM 的响应载荷。
func queryFsInfo(ctx *Context, req *wire.QueryInfoRequest) ([]byte, error) {
	fs := ctx.Tree.FS()
	if fs == nil {
		return nil, status.InvalidDeviceRequest
	}
	info, err := fs.StatFS()
	if err != nil {
		return nil, status.FromVFSError(err)
	}

	switch req.FsClass() {
	case wire.FileFsVolumeInformation:
		return wire.FsVolumeInfo{
			VolumeSerialNumber: info.VolumeSerial,
			Label:              info.VolumeLabel,
		}.Encode(), nil

	case wire.FileFsSizeInformation:
		return wire.FsSizeInfo{
			TotalAllocationUnits:     int64(info.TotalBlocks),
			AvailableAllocationUnits: int64(info.AvailBlocks),
			SectorsPerAllocationUnit: 1,
			BytesPerSector:           info.BlockSize,
		}.Encode(), nil

	case wire.FileFsFullSizeInformation:
		// Time Machine 依赖它正确上报容量（AGENTS.md §2 阶段二）。
		return wire.FsFullSizeInfo{
			TotalAllocationUnits:           int64(info.TotalBlocks),
			CallerAvailableAllocationUnits: int64(info.AvailBlocks),
			ActualAvailableAllocationUnits: int64(info.FreeBlocks),
			SectorsPerAllocationUnit:       1,
			BytesPerSector:                 info.BlockSize,
		}.Encode(), nil

	case wire.FileFsAttributeInformation:
		attrs := uint32(fsAttributes)
		if !info.CaseSensitive {
			attrs &^= uint32(wire.FileCaseSensitiveSearch)
		}
		if ctx.Tree.Share.ReadOnly {
			attrs |= uint32(wire.FileReadOnlyVolume)
		}
		return wire.FsAttributeInfo{
			Attributes:                 attrs,
			MaximumComponentNameLength: int32(info.MaxComponentLen),
			FileSystemName:             fsName,
		}.Encode(), nil

	case wire.FileFsDeviceInformation:
		return wire.FsDeviceInfo{
			DeviceType:      wire.FileDeviceDisk,
			Characteristics: wire.FileDeviceIsMounted,
		}.Encode(), nil

	case wire.FileFsSectorSizeInformation:
		return wire.FsSectorSizeInfo{
			LogicalBytesPerSector:                                 info.BlockSize,
			PhysicalBytesPerSectorForAtomicity:                    info.BlockSize,
			PhysicalBytesPerSectorForPerformance:                  info.BlockSize,
			FileSystemEffectivePhysicalBytesPerSectorForAtomicity: info.BlockSize,
		}.Encode(), nil

	default:
		ctx.Log.Debug("未实现的 fs info class", "class", req.FsClass())
		return nil, status.InvalidInfoClass
	}
}

// volumeSerial 返回本树所在卷的序列号，取不到时回 0。
//
// FileIdInformation 要求 VolumeSerialNumber + FileId 组合起来全局唯一；
// 取不到序列号时回 0 是安全的（客户端只用它做相等比较）。
func volumeSerial(ctx *Context) uint64 {
	fs := ctx.Tree.FS()
	if fs == nil {
		return 0
	}
	info, err := fs.StatFS()
	if err != nil {
		return 0
	}
	return uint64(info.VolumeSerial)
}

// streamInfo 生成 FileStreamInformation 响应。
func streamInfo(ctx *Context, open *Open) ([]byte, error) {
	fs := ctx.Tree.FS()
	if fs == nil {
		return nil, status.InvalidDeviceRequest
	}
	streams, err := fs.Streams(open.Path)
	if err != nil {
		return nil, status.FromVFSError(err)
	}
	if len(streams) == 0 {
		// 目录没有数据流：MS-FSCC 允许回空缓冲。
		return nil, nil
	}

	out := make([]wire.FileStreamInfo, 0, len(streams))
	for _, s := range streams {
		out = append(out, wire.FileStreamInfo{
			Name:                 s.Name,
			StreamSize:           s.Size,
			StreamAllocationSize: s.Alloc,
		})
	}
	return wire.AppendStreamInfoChain(nil, out), nil
}

// basicInfo 把 VFS 属性翻译成 FileBasicInformation。
func basicInfo(attr *vfs.Attr) wire.FileBasicInfo {
	return wire.FileBasicInfo{
		CreationTime:   vfs.TimeToFiletime(attr.CreateTime),
		LastAccessTime: vfs.TimeToFiletime(attr.AccessTime),
		LastWriteTime:  vfs.TimeToFiletime(attr.WriteTime),
		ChangeTime:     vfs.TimeToFiletime(attr.ChangeTime),
		FileAttributes: wire.FileAttributes(attr.FileAttributes),
	}
}

// standardInfo 把 VFS 属性翻译成 FileStandardInformation。
func standardInfo(open *Open, attr *vfs.Attr) wire.FileStandardInfo {
	nlink := attr.NLink
	if nlink == 0 {
		nlink = 1
	}
	return wire.FileStandardInfo{
		AllocationSize: attr.Alloc,
		EndOfFile:      attr.Size,
		NumberOfLinks:  nlink,
		DeletePending:  open.DeleteOnClose(),
		Directory:      open.IsDir,
	}
}

// wirePath 把共享内相对路径转成线格式（反斜杠分隔、带前导反斜杠）。
func wirePath(p string) string {
	out := make([]byte, 0, len(p)+1)
	out = append(out, '\\')
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, '\\')
			continue
		}
		out = append(out, p[i])
	}
	return string(out)
}

// baseName 返回路径的最后一个分量。
func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
