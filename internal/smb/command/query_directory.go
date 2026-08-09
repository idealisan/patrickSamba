package command

import (
	"errors"
	"io"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	register(wire.CommandQueryDirectory, true, true, handleQueryDirectory)
}

// handleQueryDirectory 处理 SMB2 QUERY_DIRECTORY（MS-SMB2 §3.3.5.18）。
//
// 语义要点（protocol-notes §9，非常容易踩错）：
//   - 搜索模式只在**首次**（或带 SMB2_RESTART_SCANS 时）携带，
//     后续调用沿用同一句柄上的模式，枚举位置也保存在句柄里；
//   - 首次调用就没有任何匹配 → STATUS_NO_SUCH_FILE；
//   - 之前吐过条目、这次没有了 → STATUS_NO_MORE_FILES；
//     两者搞混会让客户端要么少显示文件、要么无限循环。
//   - 输出必须 8 字节对齐并回填 NextEntryOffset，由 wire.DirEntryWriter 负责。
func handleQueryDirectory(ctx *Context) error {
	req, err := wire.ParseQueryDirectoryRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}
	if !open.IsDir {
		return status.InvalidParameter
	}
	if open.GrantedAccess&wire.FileReadData == 0 {
		// FILE_LIST_DIRECTORY 与 FILE_READ_DATA 是同一个位。
		return status.AccessDenied
	}
	if open.Handle == nil {
		return status.FileClosed
	}

	// 输出上限同时受客户端请求与协商出的 MaxTransactSize 约束
	// （AGENTS.md §8：目录枚举缓冲必须有上限）。
	maxOut := int(req.OutputBufferLength)
	if maxOut <= 0 {
		return status.InvalidParameter
	}
	if maxOut > int(ctx.Conn.MaxTransactSize) {
		maxOut = int(ctx.Conn.MaxTransactSize)
	}

	pattern, first := open.DirScan(req.FileName, req.Restart())

	// SMB2_RETURN_SINGLE_ENTRY：一次只要一条。
	batch := 0
	if req.SingleEntry() {
		batch = 1
	}

	entries, rerr := open.ReadDir(pattern, req.Restart(), batch)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return status.FromVFSError(rerr)
	}
	if len(entries) == 0 {
		return noMoreFiles(first)
	}

	// Apple 扩展：协商过 kAAPL_SUPPORTS_READ_DIR_ATTR 后，
	// FileIdBothDirectoryInformation 的 EaSize/ShortName 区改放 Apple 元数据
	// （见 aapl.go）。未协商时 src 为 nil，走标准布局。
	src := newAAPLDirAttrSource(ctx, open, req.FileInformationClass)

	w := wire.NewDirEntryWriter(req.FileInformationClass, maxOut)
	written := 0
	for i := range entries {
		de := dirEntry(&entries[i])
		if src != nil {
			// AAPL readdir_attr：把 Apple 扩展字段填进 de（经 wire 的
			// ShortNameRaw 编码），与标准字段一起由 DirEntryWriter 一次编码，
			// 不必事后打补丁。
			src.augmentDirEntry(&de, &entries[i])
		}
		ok, werr := w.Add(de)
		if werr != nil {
			// 不认识的 information class。
			return status.InvalidInfoClass
		}
		if !ok {
			// 缓冲放不下了：把没写进去的条目退回句柄，下一轮再来。
			open.UnreadDir(entries[i:])
			break
		}
		written++
	}

	if written == 0 {
		// 一条都放不下：客户端给的缓冲太小，连一个条目都装不了。
		return status.InfoLengthMismatch
	}

	resp := &wire.QueryDirectoryResponse{Buffer: w.Bytes()}
	out, aerr := resp.Append(ctx.Out)
	if aerr != nil {
		return status.InsuffServerResources
	}
	ctx.Out = out
	return nil
}

// noMoreFiles 区分「首次枚举就无匹配」与「枚举已结束」。
func noMoreFiles(first bool) error {
	if first {
		// 模式没匹配到任何东西。
		return status.NoSuchFile
	}
	return status.NoMoreFiles
}

// dirEntry 把 VFS 目录项翻译成线格式目录项。
func dirEntry(e *vfs.DirEntry) wire.DirEntry {
	return wire.DirEntry{
		CreationTime:   vfs.TimeToFiletime(e.Attr.CreateTime),
		LastAccessTime: vfs.TimeToFiletime(e.Attr.AccessTime),
		LastWriteTime:  vfs.TimeToFiletime(e.Attr.WriteTime),
		ChangeTime:     vfs.TimeToFiletime(e.Attr.ChangeTime),
		EndOfFile:      uint64(e.Attr.Size),
		AllocationSize: uint64(e.Attr.Alloc),
		FileAttributes: wire.FileAttributes(e.Attr.FileAttributes),
		FileID:         e.Attr.FileID,
		Name:           e.Name,
	}
}
