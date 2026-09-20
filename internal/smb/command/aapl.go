package command

import (
	"encoding/binary"
	"sync/atomic"
	"time"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
	"github.com/idealisan/patrickSamba/internal/vfs"
)

// aaplLE 是 AAPL 各字段的字节序：**小端**，与 SMB2 报文体一致
// （注意 vfs 里的 AfpInfo/AppleDouble 是大端，不要搞混）。
var aaplLE = binary.LittleEndian

// aapl.go —— Apple 的 `AAPL` create context（AGENTS.md §2 阶段二 / M6）。
//
// # 来源（已核对，非猜测）
//
//  1. Apple《Time Machine over SMB Specification》(developer.apple.com 归档文档)
//     给出了请求/响应两张字段表与一份**完整的十六进制示例**，
//     以及 kAAPL_SERVER_QUERY=1、kAAPL_SERVER_CAPS=0x1、kAAPL_VOLUME_CAPS=0x2、
//     kAAPL_MODEL_INFO=0x4、kAAPL_SUPPORTS_FULL_SYNC=0x4 五个常量的取值。
//     该文档**没有**定义 ServerCapabilities / VolumeCapabilities 的其余位，
//     也没有定义 ClientCapabilities 的位含义。
//  2. 其余位的取值取自 Samba `libcli/smb/smb2_constants.h` 的
//     SMB2_CRTCTX_AAPL_* 宏，行为对齐 Samba `source3/modules/vfs_fruit.c`
//     的 check_aapl()。Samba 是 macOS 事实上的对照实现。
//
// # 报文布局（字节序：**小端**，与 SMB2 报文体一致）
//
// 请求 Data 固定 24 字节：
//
//	 0 CommandCode(4)
//	 4 Reserved(4)
//	 8 RequestBitmap(8)
//	16 ClientCapabilities(8)
//
// 响应 Data：16 字节头 + 按 ReplyBitmap 逐段追加：
//
//	 0 CommandCode(4)        原样回显
//	 4 Reserved(4)
//	 8 ReplyBitmap(8)
//	16 ServerCapabilities(8)   仅当 ReplyBitmap 含 kAAPL_SERVER_CAPS
//	   VolumeCapabilities(8)   仅当 ReplyBitmap 含 kAAPL_VOLUME_CAPS
//	   Reserved(4)+Length(4)+ModelString(UTF-16LE)
//	                           仅当 ReplyBitmap 含 kAAPL_MODEL_INFO
//
// # 与 Apple 文档的两处刻意偏离（AGENTS.md §9：以真实客户端行为为准）
//
//   - Apple 文档称「非 Mac 服务器不得在回复里置 kAAPL_MODEL_INFO」，
//     但 Samba 的 fruit:model 正是靠这一位工作的，且这是社区里让 Finder
//     显示正确图标的**唯一**手段，实测 macOS 接受。这里跟随 Samba。
//   - ReplyBitmap 直接回显 RequestBitmap（Samba 的做法），而不是只置
//     "实际返回了数据" 的位 —— 因为每一个被请求的段我们都会返回。

// AAPL CommandCode（Apple 文档 Table 1-3 / Samba SMB2_CRTCTX_AAPL_*）。
const (
	// aaplServerQuery 是唯一被广泛使用的命令：查询服务端能力。
	aaplServerQuery uint32 = 1
	// aaplResolveID 请求把 64 位 file id 解析成路径。
	//
	// 只有在 VolumeCapabilities 里宣告过 kAAPL_SUPPORT_RESOLVE_ID 的服务端
	// 才会收到它。我们不宣告（见 aaplVolumeCapabilities），因此正常情况下
	// 永远不会走到；真收到就按 Samba 的做法回 INVALID_PARAMETER。
	aaplResolveID uint32 = 2
)

// RequestBitmap / ReplyBitmap 的位（Apple 文档 Table 1-3）。
const (
	aaplServerCaps uint64 = 0x1
	aaplVolumeCaps uint64 = 0x2
	aaplModelInfo  uint64 = 0x4
)

// ServerCapabilities 的位（Samba SMB2_CRTCTX_AAPL_*，Apple 文档未定义）。
const (
	// aaplSupportsReadDirAttr：QUERY_DIRECTORY 的条目携带 Apple 扩展字段。
	aaplSupportsReadDirAttr uint64 = 0x1
	// aaplSupportsOSXCopyFile：服务端支持 macOS 的 copyfile 语义。未实现。
	aaplSupportsOSXCopyFile uint64 = 0x2
	// aaplUnixBased：服务端是 UNIX 系。Samba 无条件置位。
	aaplUnixBased uint64 = 0x4
	// aaplSupportsNFSAce：用 NFS 风格 ACE 传递 POSIX 权限。未实现。
	aaplSupportsNFSAce uint64 = 0x8
)

// VolumeCapabilities 的位。
const (
	// aaplSupportResolveID：支持 kAAPL_RESOLVE_ID。未实现 —— 需要一张
	// 全卷 FileId→路径表，create.go 里 FILE_OPEN_BY_FILE_ID 同样是拒绝的。
	aaplSupportResolveID uint64 = 0x1
	// aaplCaseSensitive：卷区分大小写。
	aaplCaseSensitive uint64 = 0x2
	// aaplSupportsFullSync：SMB2 FLUSH 具备 F_FULLFSYNC 语义。
	// **Time Machine 的硬性前提**（Apple 文档 Table 1-4）。
	aaplSupportsFullSync uint64 = 0x4
)

// aaplRequestSize 是 AAPL 请求 Data 的固定长度。
// Samba check_aapl() 对长度不符的请求一律回 INVALID_PARAMETER。
const aaplRequestSize = 24

// aaplResponseHeaderSize 是响应 Data 的固定头长度。
const aaplResponseHeaderSize = 16

// DefaultAppleModel 是 AAPL ModelString 的默认值。
//
// 取 Samba 的默认值 "MacSamba"（`fruit:model` 参数）—— 与 config 层的
// config.DefaultAppleModel 一致。macOS 用它决定 Finder 里的图标；
// 想显示 Time Capsule 图标可改成 "TimeCapsule8,119" 这类真实机型串。
//
// 已可从配置透传：Settings.AppleModel（cmd 层取 mdns.apple.model），
// 为空时回落到本常量，两处来源因此一致。
const DefaultAppleModel = "MacSamba"

// aaplState 是一条连接上的 Apple 扩展协商结果。
//
// 按**连接**存而不是按树存：Samba 的 global_fruit_config.nego_aapl 就是
// 连接级的（smbd 每连接一个进程），而 macOS 只在挂载时的第一个 CREATE 上
// 发一次 AAPL，后续对同一连接上其它树的枚举同样期望拿到扩展字段。
//
// 用 atomic 而不是裸 bool：协商发生在 CREATE、读取发生在 QUERY_DIRECTORY，
// 目前都在同一个读循环 goroutine 里，但异步命令（SMB2_FLAGS_ASYNC_COMMAND）
// 将来会从别的 goroutine 访问。
type aaplState struct {
	readdirAttr atomic.Bool
}

// AAPLReaddirAttr 报告本连接是否协商成功了 Apple 的 readdir_attr 扩展。
func (c *Conn) AAPLReaddirAttr() bool {
	return c != nil && c.aapl.readdirAttr.Load()
}

// aaplRequest 是解析后的 AAPL 请求。
type aaplRequest struct {
	Command       uint32
	RequestBitmap uint64
	ClientCaps    uint64
}

// parseAAPLRequest 解析 AAPL create context 的 Data。
//
// 先校验长度再切片（AGENTS.md §5），外部输入非法一律返回错误不 panic。
func parseAAPLRequest(b []byte) (*aaplRequest, error) {
	if len(b) != aaplRequestSize {
		return nil, status.InvalidParameter
	}
	return &aaplRequest{
		Command:       aaplLE.Uint32(b[0:4]),
		RequestBitmap: aaplLE.Uint64(b[8:16]),
		ClientCaps:    aaplLE.Uint64(b[16:24]),
	}, nil
}

// negotiateAAPL 处理 CREATE 里可能携带的 AAPL create context。
//
// 返回要回给客户端的响应 Data；请求里没有 AAPL context 时返回 nil。
// 解析失败返回 NTSTATUS（与 Samba check_aapl 一致，整个 CREATE 失败）。
func negotiateAAPL(ctx *Context, req *wire.CreateRequest) ([]byte, error) {
	data, ok := wire.FindCreateContext(req.Contexts, wire.CreateContextAAPL)
	if !ok {
		return nil, nil
	}

	r, err := parseAAPLRequest(data)
	if err != nil {
		ctx.Log.Debug("AAPL create context 长度非法", "len", len(data))
		return nil, status.InvalidParameter
	}
	if r.Command != aaplServerQuery {
		// kAAPL_RESOLVE_ID 及未知命令：我们没宣告过对应能力，收到即为异常。
		ctx.Log.Debug("不支持的 AAPL CommandCode", "cmd", r.Command)
		return nil, status.InvalidParameter
	}

	serverCaps, readdirAttr := aaplServerCapabilities(ctx, r.ClientCaps)
	out := buildAAPLResponse(r.RequestBitmap, serverCaps,
		aaplVolumeCapabilities(ctx), ctx.Conn.Settings.appleModel())

	// 只有客户端**真的请求了 ServerCapabilities** 时才启用 readdir_attr。
	//
	// Samba check_aapl() 把 `config->readdir_attr_enabled = true` 写在
	// `if (req_bitmap & SMB2_CRTCTX_AAPL_SERVER_CAPS)` 分支**内部**。
	// 没请求这一段的客户端收不到 ServerCapabilities，也就无从得知我们支持
	// readdir_attr —— 此时若仍改写目录项布局，它会把 rfork_size 与压缩
	// FinderInfo 当成真正的 8.3 短名去解析。宁可不开也不能开错。
	if readdirAttr && r.RequestBitmap&aaplServerCaps != 0 {
		ctx.Conn.aapl.readdirAttr.Store(true)
	}
	ctx.Log.Debug("AAPL 协商",
		"req_bitmap", r.RequestBitmap, "client_caps", r.ClientCaps,
		"server_caps", serverCaps, "readdir_attr", readdirAttr)
	return out, nil
}

// aaplServerCapabilities 计算 ServerCapabilities，并报告是否启用 readdir_attr。
//
// 对齐 Samba check_aapl()：kAAPL_UNIX_BASED 无条件置位；readdir_attr 只在
// **客户端也声明支持**且后端能提供 Apple 元数据时才置位。
func aaplServerCapabilities(ctx *Context, clientCaps uint64) (caps uint64, readdirAttr bool) {
	caps = aaplUnixBased

	// Samba 的条件是 fs_capabilities & FILE_NAMED_STREAMS；对我们而言
	// 真正的前提是后端能拿出 FinderInfo 与资源派生大小，即 vfs.AppleMetadata。
	_, hasAppleMeta := ctx.Tree.FS().(vfs.AppleMetadata)
	if clientCaps&aaplSupportsReadDirAttr != 0 && hasAppleMeta {
		caps |= aaplSupportsReadDirAttr
		readdirAttr = true
	}

	// kAAPL_SUPPORTS_OSX_COPYFILE（FSCTL_SRV_COPYCHUNK 之外的 Apple 私有
	// 整文件复制）与 kAAPL_SUPPORTS_NFS_ACE（用 NFS ACE 传 POSIX 模式位）
	// 都未实现，不能宣告 —— 宣告了客户端就会真的去用。
	_, _ = aaplSupportsOSXCopyFile, aaplSupportsNFSAce
	return caps, readdirAttr
}

// aaplVolumeCapabilities 计算 VolumeCapabilities。
func aaplVolumeCapabilities(ctx *Context) uint64 {
	var caps uint64

	// kAAPL_SUPPORTS_FULL_SYNC：SMB2 FLUSH 必须具备 F_FULLFSYNC 语义。
	// handleFlush 调用的是 Handle.Sync(true)（强制刷盘），所以这个宣告是真的。
	// 由 TestFlushReallyCallsFullSync 钉住（Sync 被删掉或降级成 Sync(false) 会红）。
	// 注意这是**能力级**宣告，不是每请求开关：SMB2 FLUSH 报文里没有刷盘强度
	// 标志位（那两个 Reserved 字段只是保留字段，见 wire/flush.go）。
	// 只在标了 time_machine 的共享上置位（同 Samba 的 fruit:time machine）。
	if sh := ctx.Tree.Share; sh != nil && sh.TimeMachine {
		caps |= aaplSupportsFullSync
	}

	// kAAPL_CASE_SENSITIVE 以后端自报为准。本地磁盘后端报「不敏感」
	// （SMB 语义），所以通常不置位；置错会让 Finder 认为 "A" 与 "a"
	// 是两个不同的文件。
	if fs := ctx.Tree.FS(); fs != nil {
		if info, err := fs.StatFS(); err == nil && info.CaseSensitive {
			caps |= aaplCaseSensitive
		}
	}

	// kAAPL_SUPPORT_RESOLVE_ID 未实现，恒不置位。
	_ = aaplSupportResolveID
	return caps
}

// buildAAPLResponse 按 replyBitmap 编码响应 Data。
//
// 各字段一律**小端**；ModelString 是 UTF-16LE、不带 NUL 结尾，
// 前面有 4 字节保留 0 与 4 字节字节长度（Samba check_aapl 的写法）。
func buildAAPLResponse(replyBitmap, serverCaps, volumeCaps uint64, model string) []byte {
	// 只保留我们**真的会提供字节**的那些位。
	//
	// 原样回显请求位是不安全的：客户端一旦请求了未知位（例如 0x8），回复里
	// 置了位却不带对应字节，而客户端是按"有位就有段"去解析的 —— 后面的
	// 字段会整体错位，表现为难以定位的乱码而不是一个明确的错误。
	//
	// 掩码必须做在这里而不是交给调用方：下面算长度与写字节用的是同一个值，
	// 两处一旦不同步就是畸形报文，而调用方只看到入参、看不到这个不变式。
	replyBitmap &= aaplServerCaps | aaplVolumeCaps | aaplModelInfo

	var modelBytes []byte
	if replyBitmap&aaplModelInfo != 0 {
		modelBytes = wire.EncodeUTF16LE(model)
	}

	size := aaplResponseHeaderSize
	if replyBitmap&aaplServerCaps != 0 {
		size += 8
	}
	if replyBitmap&aaplVolumeCaps != 0 {
		size += 8
	}
	if replyBitmap&aaplModelInfo != 0 {
		size += 8 + len(modelBytes)
	}

	buf := make([]byte, size)
	aaplLE.PutUint32(buf[0:4], aaplServerQuery)
	// buf[4:8] Reserved 恒为 0。
	aaplLE.PutUint64(buf[8:16], replyBitmap)

	off := aaplResponseHeaderSize
	if replyBitmap&aaplServerCaps != 0 {
		aaplLE.PutUint64(buf[off:off+8], serverCaps)
		off += 8
	}
	if replyBitmap&aaplVolumeCaps != 0 {
		aaplLE.PutUint64(buf[off:off+8], volumeCaps)
		off += 8
	}
	if replyBitmap&aaplModelInfo != 0 {
		// buf[off:off+4] 保留，恒为 0。
		aaplLE.PutUint32(buf[off+4:off+8], uint32(len(modelBytes)))
		copy(buf[off+8:], modelBytes)
	}
	return buf
}

// ---------------------------------------------------------------------------
// AAPL readdir_attr —— QUERY_DIRECTORY 目录项的 Apple 扩展布局
// ---------------------------------------------------------------------------
//
// # 来源（**唯一权威是 Samba 源码**，MS-FSCC/MS-SMB2 里没有这套语义）
//
// 已逐字核对 samba.git master：
//
//   - `source3/smbd/smb2_trans2.c` `smbd_marshall_dir_entry()` 的
//     `case SMB_FIND_ID_BOTH_DIRECTORY_INFO:` 分支（约 L1506-1595）。
//   - `source3/lib/readdir_attr.h` 的 `struct aapl`：
//     `{ uint64_t rfork_size; char finder_info[16]; uint32_t max_access; mode_t unix_mode; }`
//     —— **finder_info 就是 16 字节，不是 32**。
//   - `source3/modules/vfs_fruit.c` 的 `readdir_attr_meta_finderi()`（L1012）
//     与 `readdir_attr_macmeta()`（L1153）—— 决定这 16 字节怎么压缩出来。
//   - `source3/lib/adouble.h`：`AD_DATE_DELTA = 946684800`、`AD_DATE_START = 0x80000000`。
//
// # 与常规 FileIdBothDirectoryInformation 的差异
//
// 只影响 `FileIdBothDirectoryInformation`(37) 这一个 information class，
// 且只在 AAPL 协商出 kAAPL_SUPPORTS_READ_DIR_ATTR 之后生效。
// 偏移量相对**条目起点**（MS-FSCC §2.4.17）：
//
//	64 EaSize(4)            → max_access（小端）
//	68 ShortNameLength(1)   → 24
//	69 Reserved(1)          → 0
//	70 ShortName[0:8]       → rfork_size（**小端 uint64**）
//	78 ShortName[8:24]      → 压缩 FinderInfo（16 字节）
//	94 Reserved2(2)         → unix_mode；我们恒填 0，见下
//	96 FileId(8)            不变
//
// Samba 在 68 处写的是 `SSVAL(p, 0, 24)`，即 ShortNameLength=24、Reserved=0。
// 源码注释写明：「按文档 short_name_len 应当为 0，但抓包显示客户端把它置成 24」
// —— 以真实客户端行为为准（AGENTS.md §9），我们跟随 Samba 写 24。
//
// Reserved2 处 Samba 写的是 POSIX mode，但那受 `fruit:nfs_aces` 开关控制，
// 且同一个开关决定是否在 ServerCapabilities 里宣告 kAAPL_SUPPORTS_NFS_ACE。
// 我们不实现 NFS ACE（见 aaplSupportsNFSAce），所以这里**必须**留 0，
// 否则客户端会按 NFS ACE 语义解读一个我们并不支持的字段。
const (
	// aaplFinderInfoSize 是压缩 FinderInfo 的字节数（struct aapl.finder_info[16]）。
	aaplFinderInfoSize = 16
)

// AppleDouble 的日期基准（source3/lib/adouble.h）。
const (
	// aaplDateDelta 是 AD_DATE_DELTA：2000-01-01T00:00:00Z 的 Unix 秒。
	// AppleDouble 的日期以 2000 年为纪元。
	aaplDateDelta = 946684800
	// aaplDateStart 是 AD_DATE_START，代表「日期未知」。
	// readdir_attr_macmeta() 先无条件写它兜底，取到真实元数据后才覆盖。
	aaplDateStart uint32 = 0x80000000
)

// aaplDirAttr 是一条目录项的 Apple 扩展数据，对应 Samba 的 `struct aapl`。
type aaplDirAttr struct {
	// MaxAccess 是该项的最大访问掩码，占用 EaSize 字段。
	MaxAccess uint32
	// RsrcSize 是资源派生（AFP_Resource）的字节数。
	RsrcSize uint64
	// FinderInfo 是压缩后的 16 字节 FinderInfo。
	FinderInfo [aaplFinderInfoSize]byte
}

// augmentDirEntry 把 Apple 扩展字段填进 wire.DirEntry，交给 wire 层在编码时
// 通过 ShortNameRaw 原样写进 ShortName 的 24 字节（见 wire.putShortName）。
//
// 取代早期「编码后按 start 偏移打补丁」的做法（本 agent 早期版本在
// query_directory.go 里自算条目起点再 patchIDBothDirEntry，属 P1 违反：报文层
// 与状态层没分离，且 start 算错会静默把字段写进上一条目录项的尾巴）。
// wire 的 DirEntryWriter 现已原生支持 AAPL 字段（ShortNameRaw），这条路径更干净
// 也更不易错。布局与 Samba vfs_fruit `readdir_attr` 一致（校验见 golden test）：
//
//	ShortName[0:8]   ← rfork_size（**小端** uint64）
//	ShortName[8:24]  ← 压缩 FinderInfo（16 字节）
//	ShortNameLength ← len(ShortNameRaw) = 24（Samba: SSVAL(p,0,24)）
//	EaSize           ← MaxAccess
func (s *aaplDirAttrSource) augmentDirEntry(de *wire.DirEntry, e *vfs.DirEntry) {
	a := s.entry(e)
	de.EaSize = a.MaxAccess

	var raw [24]byte
	aaplLE.PutUint64(raw[0:8], a.RsrcSize)
	copy(raw[8:24], a.FinderInfo[:])
	de.ShortNameRaw = raw[:]
}

// aaplCompressFinderInfo 把 32 字节的完整 FinderInfo 压成 readdir_attr 用的 16 字节。
//
// 严格对齐 vfs_fruit.c `readdir_attr_meta_finderi()`：
//
//	[0:4]   Finder 类型码      仅普通文件；目录留 0
//	[4:8]   Finder 创建者码    仅普通文件；目录留 0
//	[8:10]  Finder flags       = FinderInfo[8:10]
//	[10:12] 扩展 Finder flags  = FinderInfo[24:26]  ← 注意跨到后 16 字节
//	[12:16] date added         **大端** uint32(btime - AD_DATE_DELTA)
//
// date added 用 `RSIVAL`（大端）写入，而条目里其它所有字段都是小端 ——
// 这是本函数最容易写错的一处，AppleDouble 自身就是大端格式。
//
// hasMeta 为 false（对象没有 AppleDouble/AFP_AfpInfo）时，Samba 的
// readdir_attr_meta_finderi 会提前返回，只留下 macmeta 写的兜底值：
// 整块为零、[12:16] 为 AD_DATE_START。这里保持一致。
func aaplCompressFinderInfo(full [vfs.FinderInfoSize]byte, hasMeta, isDir bool, btime time.Time) [aaplFinderInfoSize]byte {
	var out [aaplFinderInfoSize]byte
	if !hasMeta {
		binary.BigEndian.PutUint32(out[12:16], aaplDateStart)
		return out
	}
	if !isDir {
		copy(out[0:4], full[0:4])
		copy(out[4:8], full[4:8])
	}
	copy(out[8:10], full[8:10])
	copy(out[10:12], full[24:26])
	binary.BigEndian.PutUint32(out[12:16], aaplDateAdded(btime))
	return out
}

// aaplDateAdded 把创建时间换算成 AppleDouble 纪元的秒数。
//
// 对应 vfs_fruit.c 的 `convert_time_t_to_uint32_t(btime.tv_sec - AD_DATE_DELTA)`：
// 直接截断成 32 位无符号，2000 年之前的时间会绕回成很大的值 —— 这与 Samba 行为
// 一致，Finder 只把它当不透明的排序键。
func aaplDateAdded(btime time.Time) uint32 {
	if btime.IsZero() {
		return aaplDateStart
	}
	return uint32(btime.Unix() - aaplDateDelta)
}

// aaplDirAttrSource 在一次 QUERY_DIRECTORY 内为每条目录项取 Apple 元数据。
//
// 每条目录项都要向后端多问一次，这是 readdir_attr 的固有代价
// （Samba 同样是逐条 fetch，而且比我们贵：它对每个条目完整 CREATE 一个
// AFP_AfpInfo 流）。所以它**只在协商成功后**才启用，
// 普通 Windows/Linux 客户端一分钱不付。
type aaplDirAttrSource struct {
	// handle 是目录句柄上的快路径：后端已经持有宿主机路径与目录快照，
	// 不必为每个条目重新做一次「从共享根逐级 lstat」的路径解析。
	// 后端不提供时为 nil，回退到 fs。
	handle vfs.DirAppleMetadata
	// fs 是文件系统级的慢路径（按相对共享根的完整路径查）。
	fs vfs.AppleMetadata
	// dir 是被枚举目录相对共享根的路径（'/' 分隔，空串表示共享根）。
	dir string
	// maxAccess 是树级别的最大访问掩码。
	//
	// Samba 用 smbd_calculate_access_mask_fsp(SEC_FLAG_MAXIMUM_ALLOWED) 逐文件算，
	// 那是基于宿主 ACL 的。本项目的授权依据只有配置文件（AGENTS.md §1.1 C8：
	// 不读宿主 ACL 做访问判定），因此同一棵树上所有条目取同一个值。
	maxAccess uint32
}

// newAAPLDirAttrSource 在本连接协商过 readdir_attr、且后端能提供 Apple 元数据时
// 返回取值器；否则返回 nil，调用方据此退化为标准布局。
func newAAPLDirAttrSource(ctx *Context, open *Open, class wire.FileInfoClass) *aaplDirAttrSource {
	// Samba 只在 SMB_FIND_ID_BOTH_DIRECTORY_INFO 上改布局，其余 class 原样。
	// macOS 用的正是这个 class。
	if class != wire.FileIdBothDirectoryInformation {
		return nil
	}
	if !ctx.Conn.AAPLReaddirAttr() {
		return nil
	}

	s := &aaplDirAttrSource{
		dir:       open.Path,
		maxAccess: uint32(maximalAccessFor(ctx.Tree)),
	}
	s.handle, _ = open.Handle.(vfs.DirAppleMetadata)
	s.fs, _ = ctx.Tree.FS().(vfs.AppleMetadata)
	if s.handle == nil && s.fs == nil {
		return nil
	}
	return s
}

// entry 取一条目录项的 Apple 扩展数据。后端出错时退化成「没有元数据」，
// 绝不让一个坏文件毁掉整批枚举（同 vfs_fruit：非致命错误照样返回该条目）。
func (s *aaplDirAttrSource) entry(e *vfs.DirEntry) aaplDirAttr {
	a := aaplDirAttr{MaxAccess: s.maxAccess}
	isDir := e.Attr.FileAttributes&vfs.FileAttributeDirectory != 0

	full, rsrc, ok := s.appleInfo(e.Name)
	var hasMeta bool
	if ok {
		// vfs 对「没有 Apple 元数据」与「有但全零」返回同样的结果，
		// 只能用全零近似判定。差异仅体现在 date added 是真实时间还是
		// AD_DATE_START，Finder 不据此做任何功能性决策。
		//
		// TODO: 待 vfs 层能区分（多返回一个 hasMeta，或缺元数据时返回
		// ErrNotFound）后改为精确判定。
		hasMeta = full != [vfs.FinderInfoSize]byte{}
		// 目录没有资源派生；Samba 也只对文件取 rfork_size。
		if !isDir && rsrc > 0 {
			a.RsrcSize = uint64(rsrc)
		}
	}
	a.FinderInfo = aaplCompressFinderInfo(full, hasMeta, isDir, e.Attr.CreateTime)
	return a
}

// appleInfo 取单个目录项的 Apple 元数据，优先走目录句柄快路径。
//
// ".." 一律跳过 —— 它可能指到共享根之外，而 Finder 从不看 ".." 的
// Apple 元数据（AGENTS.md §8：不给路径穿越留口子）。
// "." 指向被枚举目录自身，只能走按路径查的慢路径
// （DirAppleMetadata.AppleInfoAt 明确拒收 "." 与 ".."）。
func (s *aaplDirAttrSource) appleInfo(name string) (fi [vfs.FinderInfoSize]byte, rsrc int64, ok bool) {
	switch name {
	case "..":
		return fi, 0, false
	case ".":
		return s.appleInfoByPath(s.dir)
	}
	if s.handle != nil {
		// 这里刻意用逐条的 AppleInfoAt 而不是 AppleInfoAtBatch：
		// 一页能装多少条是由 DirEntryWriter 边写边定的（放不下的条目会被
		// UnreadDir 退回句柄），而 open.ReadDir 一次可能吐回整个几万条的
		// 目录 —— 提前整批取会为绝大多数根本进不了本页的条目白做元数据。
		// 且按 internal/vfs/apple_bench_test.go 的实测，批量省下的只是
		// 每条一次的加锁与读缓冲分配，真正的成本 getxattr 无法批量。
		f, r, err := s.handle.AppleInfoAt(name)
		if err == nil {
			return f, r, true
		}
		// 快路径失败（例如后端在这个条目上不支持）就退回慢路径，
		// 而不是直接把这条目录项的 Apple 字段丢空。
	}
	return s.appleInfoByPath(s.childPath(name))
}

func (s *aaplDirAttrSource) appleInfoByPath(p string) (fi [vfs.FinderInfoSize]byte, rsrc int64, ok bool) {
	if s.fs == nil {
		return fi, 0, false
	}
	f, r, err := s.fs.AppleInfo(p)
	if err != nil {
		return fi, 0, false
	}
	return f, r, true
}

// childPath 拼出目录项相对共享根的路径。
func (s *aaplDirAttrSource) childPath(name string) string {
	if s.dir == "" {
		return name
	}
	return s.dir + "/" + name
}
