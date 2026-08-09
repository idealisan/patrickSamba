package command

import (
	"encoding/binary"
	"sync/atomic"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
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
// TODO: command.Settings 目前没有这个字段，而 settings.go 是跨模块契约
// （由 server agent 维护），改它需要先通知。等接口稳定后从配置透传。
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
		aaplVolumeCapabilities(ctx), DefaultAppleModel)

	if readdirAttr {
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
