package command

import (
	"errors"
	"io"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
	"github.com/idealisan/patrickSamba/internal/vfs"
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

	batch := queryDirBatch(maxOut, req.FileInformationClass, req.SingleEntry())

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

// queryDirBatch 算出这一轮最多向后端要多少条目录项。
//
// 返回 0 表示不限条数。**不限条数的代价是超线性的**：每次 QUERY_DIRECTORY
// 都会把剩余条目全部读出、写掉装得下的那几条、再把剩下的整份拷回句柄
// （Open.UnreadDir 每次都要拷贝剩余部分）。于是每一轮都要拷贝 O(剩余条目) 个
// 目录项，128k 个 band 的目录实测 4.44s / 2.76GB 分配 —— 也就是 O(N²)。
// 限了批之后，每轮只读「装得下的那些」，退回时的拷贝量恒定。
//
// 估算用**单条最小开销**（固定头 + 一个 UTF-16 码元）作分母，因此算出来的批
// 只会偏大不会偏小：偏大只是多读几条再退回去，代价恒定；偏小则会导致每轮
// 装不满输出缓冲、平白多跑几次往返。
func queryDirBatch(maxOut int, class wire.FileInfoClass, singleEntry bool) int {
	if singleEntry {
		// SMB2_RETURN_SINGLE_ENTRY：一次只要一条。
		return 1
	}
	fixed, ok := wire.DirInfoFixedSize(class)
	if !ok {
		// 不认识的信息类：后面 DirEntryWriter.Add 会直接报错，
		// 这里按旧行为返回 0，不改变错误路径上的表现。
		return 0
	}
	// +1 兜底：maxOut 小于单条最小开销时至少也要取一条，否则一轮都装不下，
	// 客户端会陷在「取 0 条、再取」的死循环里。
	return maxOut/(fixed+2) + 1
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
