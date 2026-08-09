package command

import (
	"crypto/sha256"
	"encoding/binary"

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
//
// FILE_NAMED_STREAMS 与 FILE_SUPPORTS_SPARSE_FILES 是 Time Machine 的前置条件：
// macOS 只有看到这两位才会去查 FileStreamInformation（AFP_Resource/AFP_AfpInfo）
// 与发 FSCTL_SET_SPARSE / FSCTL_QUERY_ALLOCATED_RANGES（.sparsebundle 的 band 回收）。
//
// FILE_CASE_SENSITIVE_SEARCH 按卷的真实能力在 queryFsInfo 里动态清除。
const fsAttributes = wire.FileCaseSensitiveSearch |
	wire.FileCasePreservedNames |
	wire.FileUnicodeOnDisk |
	wire.FileSupportsSparseFiles |
	wire.FileNamedStreams

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

	case wire.FileFsObjectIDInformation:
		return wire.FsObjectIDInfo{ObjectID: volumeObjectID(ctx, info)}.Encode(), nil

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

// volumeObjectID 生成本共享的卷 object id（FileFsObjectIdInformation 的前 16 字节）。
//
// ⚠️ 规范与真实实现在这里是分叉的，选择依据是 AGENTS.md §9「以真实客户端/
// 服务端行为为准」：
//
//   - MS-FSA §2.1.5.13.8 规定：对象存储不实现 object id 时回
//     STATUS_INVALID_PARAMETER；卷支持但未设置 id 时回 STATUS_VOLUME_NOT_UPGRADED
//     / STATUS_OBJECT_NAME_NOT_FOUND。
//   - Samba 不走这条路：它在 `SMB_FS_OBJECTID_INFORMATION` 上**总是**回 64 字节，
//     object id 由 `create_volume_objectid()` 现造（SambaWiki "UNIX Extensions"
//     记录了这一行为与那 48 字节扩展信息）。绝大多数客户端是照着 Samba 的行为
//     写的，所以我们也回成功。
//
// id 由卷序列号 + 共享名哈希而来，保证：同一共享重启后不变（客户端会缓存它
// 做卷身份识别），不同共享互不相同。ExtendedInfo 保持全零 —— MS-FSCC §2.5.6
// 明确「客户端不得解释其内容」，Samba 往里塞版本号是它自己的 UNIX 扩展协商
// 手段，我们不是 Samba，不冒充。
func volumeObjectID(ctx *Context, info *vfs.FSInfo) [16]byte {
	h := sha256.New()
	// 域分隔前缀，避免和别的哈希用途撞上。
	h.Write([]byte("stupidsamba/volume-object-id\x00"))
	var serial [4]byte
	binary.LittleEndian.PutUint32(serial[:], info.VolumeSerial)
	h.Write(serial[:])
	h.Write([]byte{0})
	h.Write([]byte(ctx.Tree.Share.Name))

	var id [16]byte
	copy(id[:], h.Sum(nil))
	return id
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
