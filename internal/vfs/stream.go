package vfs

// stream.go —— SMB alternate data stream（ADS）的路径语法解析。
//
// 为什么放在 VFS 层而不是 server 层：流名是**存储语义**的一部分 ——
// 同一个流名在 NTFS 上是原生 ADS、在 Linux 上要落到 xattr 或旁路文件，
// 只有 VFS 知道该怎么存。server 层只该把 SMB 的 '\' 转成 '/' 然后原样传下来。
//
// # 语法（MS-FSCC §2.1.5.3 File Streams / MS-SMB2 §2.2.13）
//
//	<文件路径>:<流名>:<流类型>
//
// 流类型对文件恒为 `$DATA`，对目录是 `$INDEX_ALLOCATION`。
// 主数据流有两种等价写法：`file.txt` 与 `file.txt::$DATA`。
//
// # 与 Samba vfs_fruit 的关系
//
// macOS 会用到两个特殊流（Samba `source3/include/MacExtensions.h`）：
//
//	:AFP_AfpInfo:$DATA    60 字节的 AfpInfo 结构，含 32 字节 FinderInfo
//	:AFP_Resource:$DATA   资源派生（resource fork），可以任意大
//
// 存储后端见 afpinfo.go / appledouble.go 的说明。

import (
	"fmt"
	"strings"
)

// 特殊流名。取自 Samba source3/include/MacExtensions.h 的
// AFPINFO_STREAM_NAME / AFPRESOURCE_STREAM_NAME（去掉前导冒号）。
const (
	// StreamAFPInfo 存放 60 字节 AfpInfo（含 FinderInfo）。
	StreamAFPInfo = "AFP_AfpInfo"
	// StreamAFPResource 存放资源派生。
	StreamAFPResource = "AFP_Resource"
)

// 流类型后缀。MS-FSCC §2.1.5.3。
const (
	streamTypeData  = "$DATA"
	streamTypeIndex = "$INDEX_ALLOCATION"
)

// DefaultStreamName 是主数据流在 FileStreamInformation 里的写法。
const DefaultStreamName = "::$DATA"

// StreamName 构造 FileStreamInformation 用的流名（形如 ":AFP_AfpInfo:$DATA"）。
// name 为空时返回主数据流写法。
func StreamName(name string) string {
	if name == "" {
		return DefaultStreamName
	}
	return ":" + name + ":" + streamTypeData
}

// SplitStreamPath 把可能带流名的路径拆成「基础路径 + 流名」。
//
// 接受的写法：
//
//	"dir/f.txt"                     → ("dir/f.txt", "",             nil)
//	"dir/f.txt::$DATA"              → ("dir/f.txt", "",             nil)   主数据流
//	"dir/f.txt:AFP_AfpInfo"         → ("dir/f.txt", "AFP_AfpInfo",  nil)   类型可省略
//	"dir/f.txt:AFP_AfpInfo:$DATA"   → ("dir/f.txt", "AFP_AfpInfo",  nil)
//
// **只在最后一个路径分量里找冒号** —— 目录名里的冒号本来就是非法字符，
// 交给 Resolver 去拒绝，这里不越权处理。
//
// 非法输入返回 ErrInvalidPath（而不是悄悄当成主数据流）：把
// "f.txt:bad:$DATA:extra" 当普通文件名会让客户端拿到一个它没请求的对象。
func SplitStreamPath(p string) (base, stream string, err error) {
	// 只看最后一个分量，避免把 "a:b/c" 里的冒号误判成流分隔符。
	slash := strings.LastIndexByte(p, '/')
	last := p[slash+1:]

	colon := strings.IndexByte(last, ':')
	if colon < 0 {
		return p, "", nil
	}

	base = p[:slash+1+colon]
	rest := last[colon+1:]

	// rest 现在是 "<流名>" 或 "<流名>:<类型>"。
	name, typ, hasType := strings.Cut(rest, ":")
	if hasType {
		if strings.ContainsRune(typ, ':') {
			// "f:s:$DATA:extra" —— 多余的冒号，非法。
			return "", "", ErrInvalidPath
		}
		if !strings.EqualFold(typ, streamTypeData) &&
			!strings.EqualFold(typ, streamTypeIndex) {
			return "", "", ErrInvalidPath
		}
	}

	if name == "" {
		if !hasType {
			// "f.txt:" —— 空流名且没有类型，是个残缺输入。
			return "", "", ErrInvalidPath
		}
		// "f.txt::$DATA" —— 主数据流的显式写法。
		return base, "", nil
	}

	if err := ValidateStreamName(name); err != nil {
		return "", "", err
	}
	return base, name, nil
}

// ValidateStreamName 校验流名本身是否合法。
//
// 流名不能为空、不能含路径分隔符与冒号，也不能含控制字符。
// 这些字符如果放行，在「流名 → xattr 名」或「流名 → ._ 旁路文件名」的
// 映射里会变成路径穿越（AGENTS.md §8）。
func ValidateStreamName(name string) error {
	if name == "" {
		return ErrInvalidPath
	}
	if len(name) > MaxComponentLen {
		return fmt.Errorf("%w: 流名过长 (%d 字节 > %d)", ErrInvalidPath, len(name), MaxComponentLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == '/' || c == '\\' || c == ':' || c == '"' ||
			c == '<' || c == '>' || c == '|' || c == '*' || c == '?' {
			return ErrInvalidPath
		}
	}
	// "." 和 ".." 作为流名会在旁路文件映射里变成目录引用。
	if name == "." || name == ".." {
		return ErrInvalidPath
	}
	return nil
}

// IsAFPStream 判断是否是 macOS 的两个特殊流之一。
//
// 流名比较**大小写不敏感**：SMB 的流名沿用文件名的大小写规则，
// 而 macOS 客户端在不同版本里大小写写法并不一致。
func IsAFPStream(name string) bool {
	return strings.EqualFold(name, StreamAFPInfo) ||
		strings.EqualFold(name, StreamAFPResource)
}

// StreamRemover 是「按流粒度删除」的可选能力。
//
// 为什么需要它：对命名流句柄做 delete-on-close（FILE_DELETE_ON_CLOSE 或
// SET_INFO/FileDispositionInformation）时，删除的对象是**这一个流**，
// 不是基础文件。若上层拿不到这个能力就只能退化为整文件删除——那会把
// 基础文件连同它上面的所有流一起删掉，是数据丢失级的事故。
//
// Samba 对照：删除按 fsp 粒度走，命名流 fsp 就是流本身；
// streams_xattr_unlinkat()（source3/modules/vfs_streams_xattr.c:1056-1110）
// 对命名流只清对应的 xattr，注释原话 "A stream can never be rmdir'ed"。
type StreamRemover interface {
	// RemoveStream 删除 base 对象上名为 stream 的命名流，基础对象不受影响。
	//
	//   - stream 必须非空：删主数据流/整个对象请走 FileSystem.Remove；
	//   - 流本来就不存在时返回 nil（幂等）：CLOSE 语境下的重复删除
	//     （例如两个句柄都标了 delete-on-close）不该让后关的那个报错。
	RemoveStream(base, stream string) error
}

// canonicalStreamName 把流名折叠成规范写法，便于后端用它做 key。
// 认得的特殊流返回 Samba/Apple 的标准拼写，其余原样返回。
func canonicalStreamName(name string) string {
	switch {
	case strings.EqualFold(name, StreamAFPInfo):
		return StreamAFPInfo
	case strings.EqualFold(name, StreamAFPResource):
		return StreamAFPResource
	}
	return name
}
