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
//
// # 流名里的「非法」字符：原样存，不做转换
//
// macOS 真实用到的流名长这样：`com.apple.metadata:kMDItemFinderComment`
// —— **含冒号**，而冒号是 SMB 流名的分隔符。这看似矛盾，实际不会冲突：
//
//	OS X maps NTFS illegal characters to the Unicode private range
//	in SMB requests.                        —— vfs_fruit(8) 手册
//
// 也就是说客户端在线上发的是 **U+F022**，不是裸冒号。
// 于是 SplitStreamPath 按裸冒号切分**依然是正确的**，
// ValidateStreamName 拒绝裸冒号也依然正确 —— 合法请求里根本不会有。
//
// ⚠️ 这张映射表**不是** `0xF000 + 字符`（那是老的 SFM 方案，会得出
// U+F03A）。macOS/Samba 用的是一张紧凑分配的表，后 8 个非法字符接着
// U+F01F 顺序往下排，与字符码点无关。出处
// `source3/lib/string_replace.c:186` 的 macos_string_replace_map：
//
//	0x01..0x1F 控制字符 → U+F001..U+F01F
//	0x22 "  → U+F020    0x2A *  → U+F021
//	0x3A :  → U+F022    0x3C <  → U+F023
//	0x3E >  → U+F024    0x3F ?  → U+F025
//	0x5C \  → U+F026    0x7C |  → U+F027
//
// 本实现原样透传、不做还原，所以**代码本身不依赖这张表**；
// 记在这里是因为写测试数据时必须用对，用错了测的就不是真实客户端行为。
//
// 我们把流名**原样**（连同私用区字符）拼进 xattr 名，不还原成 ASCII。
// 依据：Samba 的 `fruit:encoding` 默认值就是 `private`（保留私用区字符，
// 见 vfs_fruit.c:319 的 lp_parm_enum 默认参数），`native` 是可选项且
// 手册明确警告它 "is known to not fully work"。原样存能保证
// SMB 读写完美往返，也与 Samba 的默认配置在磁盘上一致。
//
// 注意私用区字符 UTF-8 编码占 3 字节，会更快吃掉下面的名字长度预算 ——
// 这是对的，xattr 名的上限本来就是按字节算的。

import (
	"errors"
	"io"
	"strings"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

const (
	// dosStreamPrefix 是通用流的 xattr 名前缀（不含 "user."，
	// Linux 上由 encodeName 补齐 —— 与 netatalkMetaXattr 同一套约定）。
	dosStreamPrefix = "DosStream."

	// dosStreamSuffix 是拼进 xattr 名的流类型后缀，见文件头。
	dosStreamSuffix = ":" + streamTypeData

	// maxXattrNameLen 是**宿主内核**对完整 xattr 名（含命名空间前缀）的
	// 长度上限，按平台取严：darwin 127 / 其余 255（见 xattrlimit_*.go）。
	//
	// 为什么不一律用 Linux 的 255：这个预算是拿来**事先回答客户端**
	// 「这个名字能不能用」的。取宽会让 macOS 上「预算内」的流名到真正落盘
	// 时被内核拒（ENAMETOOLONG）—— 预算与内核真实能力对不上，等于假承诺。
	// 代价是同一个长流名在 macOS 与 Linux 上的可用范围不同；那是平台事实，
	// 由预算如实反映比由我们这边假装它不存在好。
	maxXattrNameLen = platformXattrNameMax

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

// maxDosStreamNameLen 是通用流名的最大字节数，随平台上限变化
// （linux 234 / darwin 106）。
//
// 完整 xattr 名是 "user." + "DosStream." + <流名> + ":$DATA"，
// 必须整体不超过 maxXattrNameLen。
//
// 前缀一律按最长的 "user." 计（即使某些平台并不加这个前缀）：
// 各平台用同一个算式，少一个「前缀长度不同、差几个字节」的隐藏变量；
// 代价是 darwin 上比内核真上限再保守几个字节，安全侧。
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

// resolveDosStreamName 把请求的通用流名解析成磁盘上的真实拼写。
//
// 精确命中优先，未命中且共享配置为大小写不敏感时再 EqualFold 兜底一次 ——
// 与路径查找的口径一致（Resolver.caseFallback：先精确 Lstat，miss 才全扫）。
//
// 这是 bh5 F10：Samba 在不区分大小写的共享上对流名做同样的事，
// filename.c 在精确匹配 NOT_FOUND 后调 get_real_stream_name()
// （bh5 报告引证 :444-476，调用点 :983-999）做大小写不敏感匹配再开。
// 修复前 xattr 名按字节精确匹配，先写 ":Meta" 后读 ":meta" 会凭空
// 多出一条空流而不是读到旧内容；delete-on-close 则会静默删到空气。
//
// 没有命中返回 ("", false)，调用方按「流不存在」继续走自己的路径
// （FILE_OPEN 回 NotFound / 创建语义建新流）。枚举失败同样视为未命中：
// 「列不出就当没有」与 dosStreamsOf 的容错口径一致。
func (l *LocalFS) resolveDosStreamName(host, stream string) (string, bool) {
	infos, err := l.caps.Streams().ListStreams(l.streamRef(host))
	if err != nil {
		return "", false
	}
	for _, si := range infos {
		if si.Name == stream {
			return si.Name, true
		}
	}
	if !l.cfg.CaseInsensitive {
		return "", false
	}
	for _, si := range infos {
		if strings.EqualFold(si.Name, stream) {
			return si.Name, true
		}
	}
	return "", false
}

// readDosStream 读出一个通用流的内容。
//
// 流不存在返回 ErrNotFound。**不会**再返回「本平台不支持」——
// 宿主没有扩展属性时由 builtin 适配器用旁路存储承载同一份语义。
func (l *LocalFS) readDosStream(host, stream string) ([]byte, error) {
	h, err := l.caps.Streams().OpenStream(l.streamRef(host), stream, oscap.StreamRead)
	if err != nil {
		return nil, mapOscapError(err)
	}
	defer func() { _ = h.Close() }()

	size, err := h.Size()
	if err != nil {
		return nil, mapOscapError(err)
	}
	if size <= 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := h.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		// io.EOF 在这里是正常的：Size 与 ReadAt 之间流被别人改短了，
		// 读到多少算多少。别的错误如实上报。
		return nil, mapOscapError(err)
	}
	return buf[:n], nil
}

// writeDosStream 覆盖写入一个通用流。
func (l *LocalFS) writeDosStream(host, stream string, data []byte) error {
	if len(data) > maxDosStreamSize {
		// 上限在这一层就拦住，而不是等适配器报 ENOSPC：SMB 侧要的是
		// STATUS_FILE_SYSTEM_LIMITATION（ErrTooLarge），语义比「磁盘满」准确。
		return ErrTooLarge
	}
	// Truncate：覆盖写的语义是「流内容变成 data」，不是「前 len(data) 字节
	// 被替换」。少了它，把一个长流写短会在尾部留下旧数据。
	h, err := l.caps.Streams().OpenStream(l.streamRef(host), stream,
		oscap.StreamWrite|oscap.StreamCreate|oscap.StreamTruncate)
	if err != nil {
		return mapOscapError(err)
	}
	defer func() { _ = h.Close() }()

	if len(data) == 0 {
		return nil
	}
	if _, err := h.WriteAt(data, 0); err != nil {
		return mapOscapError(err)
	}
	return nil
}

// removeDosStream 删除一个通用流。流本来就不存在时返回 nil。
func (l *LocalFS) removeDosStream(host, stream string) error {
	err := l.caps.Streams().RemoveStream(l.streamRef(host), stream)
	if err != nil && !errors.Is(err, oscap.ErrNotFound) {
		return mapOscapError(err)
	}
	return nil
}

// dosStreamsOf 列出对象上所有通用流，供 FileStreamInformation 使用。
//
// 枚举失败返回空列表而不是错误：「这个对象没有任何通用流」是一个完全正确的
// 答案，不该让整个 QUERY_INFO 失败。
func (l *LocalFS) dosStreamsOf(host string) []StreamInfo {
	infos, err := l.caps.Streams().ListStreams(l.streamRef(host))
	if err != nil {
		return nil
	}
	out := make([]StreamInfo, 0, len(infos))
	for _, si := range infos {
		out = append(out, StreamInfo{
			Name:  StreamName(si.Name),
			Size:  si.Size,
			Alloc: allocSizeFallback(si.Size),
		})
	}
	return out
}

// streamRef 构造命名流操作用的 oscap.Ref。
//
// 一律不带 Handle：命名流的宿主对象句柄与流句柄是两回事，把基础文件的 fd
// 传下去会让 native 侧对着**主数据流**的 fd 做 fsetxattr —— 在 POSIX 上
// 恰好等价，在 Windows 的 ADS 实现上就不是了。不传更保险，代价是一次路径解析。
func (l *LocalFS) streamRef(host string) oscap.Ref {
	return oscap.Ref{Path: host}
}
