package vfs

// stream_xattr.go —— **通用** named stream 的落盘后端（xattr）。
//
// 为什么必须支持：我们在 QUERY_FS_INFO 里宣告了 FILE_NAMED_STREAMS，
// 客户端就会真的去用。而 macOS 的 SMB 客户端把**扩展属性当作 ADS 发过来**
// （Samba `source3/modules/vfs_fruit.c` 文件头注释原话：
// "The OS X SMB client sends xattrs as ADS too"）—— `com.apple.metadata:*`、
// `com.apple.TimeMachine.*`、`com.apple.quarantine` 全走这条路。
// 只支持 AFP_AfpInfo/AFP_Resource 两个特例的话，这些一律吃
// STATUS_NOT_SUPPORTED，Time Machine 直接卡住。
//
// # 落盘格式（与 Samba vfs_streams_xattr 二进制兼容）
//
//	xattr 名   user.DosStream.<流名>:$DATA
//	xattr 值   <流数据><1 字节 marker>
//
// 名字前缀出处：`source3/include/smb.h:530`
//
//	#define SAMBA_XATTR_DOSSTREAM_PREFIX "user.DosStream."
//
// 名字里那个 `:$DATA` 后缀来自 `streams_xattr_get_name()`：它在
// `store_stream_type` 为真时把流类型一起拼进 xattr 名，而该选项
// **默认就是真**（`vfs_streams_xattr.c:1482`）。
//
// 末尾那个 marker 字节出自同一文件顶部的设计注释：xattr 的值不能为空，
// 所以哪怕零长度的流也要占一个字节；该字节同时用作「还有几个续存
// xattr」的计数（Samba 用它把超大流拆成多个 xattr）。
// **本实现只支持 marker==0 的单 xattr 形态**：续存机制是为了绕开
// xattr 大小上限，而超过上限的流在 SMB 场景里应当如实报错，
// 而不是悄悄拆成客户端看不见的多个片段。读到 marker!=0 时如实报
// ErrNotSupported —— 那是 Samba 写的、我们读不全的数据，谎称读到了
// 只会让客户端拿到截断的内容。
//
// 为什么用 xattr 而不是 `._` 那样的旁路文件：xattr 随文件一起被
// rename/unlink 带走，不会在目录里留下客户端看得见的垃圾条目，
// 也不会让 Time Machine 的 band 计数出错。代价是受 xattr 大小上限约束，
// 但通用 ADS 的真实用途（Finder 元数据、隔离标记）都只有几十到几百字节。

import (
	"strings"
)

const (
	// dosStreamPrefix 是通用流的 xattr 名前缀（不含 "user."，
	// Linux 上由 encodeName 补齐 —— 与 netatalkMetaXattr 同一套约定）。
	dosStreamPrefix = "DosStream."

	// dosStreamSuffix 是拼进 xattr 名的流类型后缀，见文件头。
	dosStreamSuffix = ":" + streamTypeData

	// maxXattrNameLen 是 Linux 对**完整** xattr 名（含命名空间前缀）的
	// 长度上限（XATTR_NAME_MAX，<linux/limits.h>）。macOS 的上限是 127，
	// 但取严的那个不会错 —— 宁可拒绝一个 macOS 上本可接受的超长流名，
	// 也不要在 Linux 上写出两个被内核截断成同名的不同流。
	maxXattrNameLen = 255

	// xattrUserNamespace 是 Linux 上非特权进程唯一可写的命名空间前缀。
	// 这里只用来算长度预算，实际拼接由 encodeName 负责（且它是 unix 专有的，
	// 不能在跨平台代码里调用）。
	xattrUserNamespace = "user."

	// maxDosStreamSize 是单个通用流的大小上限。
	//
	// ext4/xfs 的单个 xattr 值上限是 64 KiB，但真正能用多少取决于
	// inode 里剩余的空间，写大了会拿到 ENOSPC/E2BIG。这里取 64 KiB
	// 作为**协议层**的硬上限并如实报错，让客户端知道写不下，
	// 而不是让它以为写成功了。
	maxDosStreamSize = 64 * 1024
)

// maxDosStreamNameLen 是通用流名的最大字节数（算出来是 234）。
//
// 完整 xattr 名是 "user." + "DosStream." + <流名> + ":$DATA"，
// 必须整体不超过 maxXattrNameLen。
//
// **总是按最长的平台前缀（Linux 的 "user."）算预算**，即使在
// macOS/Windows 上前缀更短或不存在。理由：同一份数据可能在平台之间
// 迁移，如果各平台的上限不同，一个在 macOS 上写得进去的流名到 Linux
// 上就会突然写不进去 —— 那种「换个机器就坏」的行为极难排查。
//
// **绝不能静默截断**：截断会让两个不同的长流名映射到同一个 xattr，
// 后写的那个会把先写的悄悄覆盖掉。超长一律 ErrInvalidPath。
const maxDosStreamNameLen = maxXattrNameLen -
	len(xattrUserNamespace) - len(dosStreamPrefix) - len(dosStreamSuffix)

// dosStreamXattrName 把流名映射成 xattr 名（不含命名空间前缀，
// 由 encodeName 在实际调用时补齐）。
func dosStreamXattrName(stream string) string {
	return dosStreamPrefix + stream + dosStreamSuffix
}

// dosStreamFromXattrName 是逆映射。第二个返回值表示这个 xattr 是不是
// 一个通用流。
//
// 名字比较**大小写敏感**：xattr 名在内核里就是字节串，
// 我们自己写进去的一定是 dosStreamPrefix 的原样拼写。
func dosStreamFromXattrName(name string) (string, bool) {
	if !strings.HasPrefix(name, dosStreamPrefix) {
		return "", false
	}
	rest := name[len(dosStreamPrefix):]
	// 后缀可以缺省：Samba 在 store_stream_type=false 时不写它。
	// 两种都认，写出去时统一带后缀。
	rest = strings.TrimSuffix(rest, dosStreamSuffix)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// validateDosStreamName 在通用流名进入 xattr 名之前做校验。
//
// 这是**安全边界**（AGENTS.md §8）：流名会被直接拼进 xattr 名，
// 放行分隔符或冒号会让客户端能写到别的命名空间去。
func validateDosStreamName(stream string) error {
	// 先过通用流名规则（拒空、控制字符、路径分隔符、冒号、通配符、"."/".."）。
	if err := ValidateStreamName(stream); err != nil {
		return err
	}
	if len(stream) > maxDosStreamNameLen {
		// 不截断，如实拒绝 —— 见 maxDosStreamNameLen 的说明。
		return ErrInvalidPath
	}
	return nil
}

// readDosStream 读出一个通用流的内容。
//
// 流不存在返回 ErrNotFound；宿主文件系统不支持 xattr 返回 ErrNotSupported。
func (l *LocalFS) readDosStream(host, stream string) ([]byte, error) {
	x, err := newXattrAccessor(host, nil)
	if err != nil {
		return nil, err
	}
	raw, err := x.Get(dosStreamXattrName(stream))
	if err != nil {
		return nil, err
	}
	return stripStreamMarker(raw)
}

// stripStreamMarker 去掉 Samba 格式末尾的 marker 字节。
func stripStreamMarker(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		// 空值不该出现（marker 字节保证至少 1 字节）。当成空流处理
		// 而不是报错：可能是别的工具直接写的 xattr。
		return nil, nil
	}
	if marker := raw[len(raw)-1]; marker != 0 {
		// Samba 的多 xattr 续存形态，我们读不全，见文件头说明。
		return nil, ErrNotSupported
	}
	return raw[:len(raw)-1], nil
}

// writeDosStream 覆盖写入一个通用流。
func (l *LocalFS) writeDosStream(host, stream string, data []byte) error {
	if len(data) > maxDosStreamSize {
		return ErrTooLarge
	}
	x, err := newXattrAccessor(host, nil)
	if err != nil {
		return err
	}
	// 末尾补 marker=0：既满足「xattr 值不能为空」，也让 Samba 读得懂。
	buf := make([]byte, len(data)+1)
	copy(buf, data)
	return x.Set(dosStreamXattrName(stream), buf)
}

// removeDosStream 删除一个通用流。流本来就不存在时返回 nil。
func (l *LocalFS) removeDosStream(host, stream string) error {
	x, err := newXattrAccessor(host, nil)
	if err != nil {
		return err
	}
	if err := x.Remove(dosStreamXattrName(stream)); err != nil && err != ErrNotFound {
		return err
	}
	return nil
}

// dosStreamsOf 列出对象上所有通用流，供 FileStreamInformation 使用。
//
// 宿主文件系统不支持 xattr 时返回空列表而不是错误：那种情况下
// 「没有任何通用流」是完全正确的答案，不该让整个 QUERY_INFO 失败。
func (l *LocalFS) dosStreamsOf(host string) []StreamInfo {
	x, err := newXattrAccessor(host, nil)
	if err != nil {
		return nil
	}
	names, err := x.List()
	if err != nil {
		return nil
	}

	out := make([]StreamInfo, 0, 4)
	for _, raw := range names {
		stream, ok := dosStreamFromXattrName(raw)
		if !ok {
			continue
		}
		// 长度要报**流数据**的长度，不含 marker 字节。
		size := int64(0)
		if v, err := x.Get(raw); err == nil {
			if data, err := stripStreamMarker(v); err == nil {
				size = int64(len(data))
			} else {
				// 读不全的多 xattr 流：列出来但报 0，
				// 总比让客户端完全看不到它好。
				continue
			}
		}
		out = append(out, StreamInfo{
			Name:  StreamName(stream),
			Size:  size,
			Alloc: allocSizeFallback(size),
		})
	}
	return out
}
