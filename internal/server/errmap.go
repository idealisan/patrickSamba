package server

import (
	"errors"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 错误 → NTSTATUS 的集中映射（AGENTS.md §5 P5：禁止在 handler 里裸写魔数）。
//
// 分工：
//   - vfs 哨兵错误与标准库错误 → internal/smb/status.FromVFSError；
//   - wire 报文解析错误 → 本文件的 StatusFromWireError；
//   - 两者都覆盖不到的 → STATUS_UNSUCCESSFUL（不向客户端泄漏内部细节）。

// StatusFromWireError 把 wire 包的解码错误映射为 NTSTATUS。
//
// 报文解析失败一律是客户端的问题：截断、长度撒谎、StructureSize 不对，
// 都归到 STATUS_INVALID_PARAMETER —— 这是 Windows 服务端的行为。
func StatusFromWireError(err error) status.Status {
	if err == nil {
		return status.Success
	}
	switch {
	case errors.Is(err, wire.ErrTruncated),
		errors.Is(err, wire.ErrStructureSize),
		errors.Is(err, wire.ErrInvalidOffset),
		errors.Is(err, wire.ErrMalformed):
		return status.InvalidParameter
	case errors.Is(err, wire.ErrProtocolID):
		// 魔数都不对，说明根本不是 SMB2 报文。
		return status.InvalidParameter
	}
	return status.FromVFSError(err)
}
