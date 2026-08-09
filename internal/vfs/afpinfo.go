package vfs

// afpinfo.go —— `AFP_AfpInfo` 流的 60 字节 AfpInfo 结构编解码。
//
// # 来源（已核对，非猜测）
//
// 结构布局与常量取自 Samba 源码 `source3/include/MacExtensions.h`：
//
//	#define AFP_INFO_SIZE     0x3c        // 60
//	#define AFP_Signature     0x41465000  // "AFP\0"
//	#define AFP_Version       0x00000100
//	#define AFP_BackupTime    0x80000000
//	#define AFP_FinderSize    32
//	#define AFP_OFF_FinderInfo 16
//
//	typedef struct _AfpInfo {
//	    uint32_t afpi_Signature;      // 偏移 0
//	    uint32_t afpi_Version;        // 偏移 4
//	    uint32_t afpi_Reserved1;      // 偏移 8
//	    uint32_t afpi_BackupTime;     // 偏移 12
//	    uint8_t  afpi_FinderInfo[32]; // 偏移 16
//	    uint8_t  afpi_ProDosInfo[6];  // 偏移 48
//	    uint8_t  afpi_Reserved2[6];   // 偏移 54
//	} AfpInfo;                        // 合计 60
//
// # 字节序：大端（**这是最容易搞错的地方**）
//
// SMB2 报文体是小端，但 AfpInfo 这个 blob 是 Mac 血统的，里面的 uint32
// 一律**大端**。依据是 Samba `source3/lib/adouble.c` 的 afpinfo_pack/unpack
// 用的是 `RSIVAL`/`RIVAL`，而 `lib/util/byteorder.h` 里：
//
//	#define RIVAL(buf,pos)      PULL_BE_U32(buf, pos)
//	#define RSIVAL(buf,pos,val) PUSH_BE_U32(buf, pos, val)
//
// 注意 MacExtensions.h 里 afpi_Version 的**注释**写的是 "Must be 0x00010000"，
// 与紧邻的 `#define AFP_Version 0x00000100` 矛盾。代码里实际校验用的是宏，
// 所以以 0x00000100 为准，注释是过时的。
//
// # Samba 只序列化 4 个字段
//
// afpinfo_pack 只写 Signature/Version/BackupTime/FinderInfo，
// Reserved1、ProDosInfo、Reserved2 恒为零。我们保持一致 ——
// 这几个字段真实客户端不用，多写反而可能触发对端的校验。

import (
	"encoding/binary"
	"fmt"
)

// AfpInfo 相关常量。数值出处见文件头注释（Samba MacExtensions.h）。
const (
	// AfpInfoSize 是 AFP_AfpInfo 流的固定长度。
	AfpInfoSize = 60

	// afpSignature 是 "AFP\0" 的大端表示。
	afpSignature uint32 = 0x41465000
	// afpSigPrefix 是签名的前三个可打印字节。
	// Samba 的 fruit_pwrite_meta 用 memcmp(data, "AFP", 3) 做早期拦截，
	// 只比前三字节（第四字节是 NUL，写进 C 字符串字面量不便），照抄之。
	afpSigPrefix = "AFP"
	// afpVersion 是当前唯一被接受的版本号。
	afpVersion uint32 = 0x00000100
	// afpBackupTimeNever 表示「从未备份」。
	// 对应 Samba 的 AD_DATE_START（adouble.h），afpinfo_new 用它做初值。
	afpBackupTimeNever uint32 = 0x80000000

	// FinderInfoSize 是 FinderInfo 的长度（Samba ADEDLEN_FINDERI）。
	FinderInfoSize = 32
	// afpOffFinderInfo 是 FinderInfo 在 AfpInfo 里的偏移。
	afpOffFinderInfo = 16
)

// ErrBadAfpInfo 表示 AFP_AfpInfo 流的内容不是合法的 AfpInfo 结构。
//
// Samba 与 Apple 自己的 SMB 服务器都会在客户端写入这个流时校验头部，
// 校验失败就拒绝写入（见 `man vfs_fruit` 的 fruit:validate_afpinfo）。
//
// 包装 ErrInvalidArg 从而映射成 STATUS_INVALID_PARAMETER —— 对齐 Samba
// fruit_pwrite_meta() 校验失败时置 errno=EINVAL 的行为。
var ErrBadAfpInfo = fmt.Errorf("vfs: malformed AFP_AfpInfo: %w", ErrInvalidArg)

// AfpInfo 是 AFP_AfpInfo 流的解析结果。
//
// 只保留 Samba 会序列化的字段，其余（Reserved1/ProDosInfo/Reserved2）
// 恒为零，不做保留 —— 与 afpinfo_pack 行为一致。
type AfpInfo struct {
	// BackupTime 是备份时间戳，afpBackupTimeNever 表示从未备份。
	BackupTime uint32
	// FinderInfo 是 32 字节的 Finder 元数据：
	// 前 4 字节文件类型、接着 4 字节创建者代码、然后是 Finder 标志等。
	// VFS 层**不解释**它的内部结构，原样透传给客户端。
	FinderInfo [FinderInfoSize]byte
}

// NewAfpInfo 构造一个空的 AfpInfo（对应 Samba 的 afpinfo_new）。
func NewAfpInfo() *AfpInfo {
	return &AfpInfo{BackupTime: afpBackupTimeNever}
}

// Marshal 把 AfpInfo 编成 60 字节（对应 Samba afpinfo_pack）。
func (ai *AfpInfo) Marshal() []byte {
	buf := make([]byte, AfpInfoSize)
	// 大端 —— 见文件头注释。
	binary.BigEndian.PutUint32(buf[0:4], afpSignature)
	binary.BigEndian.PutUint32(buf[4:8], afpVersion)
	// buf[8:12] 是 Reserved1，恒为零。
	binary.BigEndian.PutUint32(buf[12:16], ai.BackupTime)
	copy(buf[afpOffFinderInfo:afpOffFinderInfo+FinderInfoSize], ai.FinderInfo[:])
	// buf[48:60] 是 ProDosInfo + Reserved2，恒为零。
	return buf
}

// ParseAfpInfo 解析 60 字节的 AfpInfo（对应 Samba afpinfo_unpack，validate=true）。
//
// 先校验长度再切片（AGENTS.md §5），外部输入解析失败返回错误而不是 panic。
func ParseAfpInfo(buf []byte) (*AfpInfo, error) {
	if len(buf) < AfpInfoSize {
		return nil, ErrBadAfpInfo
	}
	if binary.BigEndian.Uint32(buf[0:4]) != afpSignature ||
		binary.BigEndian.Uint32(buf[4:8]) != afpVersion {
		return nil, ErrBadAfpInfo
	}
	ai := &AfpInfo{BackupTime: binary.BigEndian.Uint32(buf[12:16])}
	copy(ai.FinderInfo[:], buf[afpOffFinderInfo:afpOffFinderInfo+FinderInfoSize])
	return ai, nil
}

// EmptyFinderInfo 判断 FinderInfo 是否全零。
//
// 这不是可有可无的工具函数：**客户端「删除 AFP_AfpInfo 流」的方式
// 就是往里写一个 FinderInfo 全零的 AfpInfo**，而不是发删除请求。
// 对应 Samba vfs_fruit.c 的 ai_empty_finderinfo()，
// 那里的注释提到有专门的回归用例 "delete AFP_AfpInfo by writing all 0"。
func (ai *AfpInfo) EmptyFinderInfo() bool {
	for _, b := range ai.FinderInfo {
		if b != 0 {
			return false
		}
	}
	return true
}
