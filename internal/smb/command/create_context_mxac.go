package command

import (
	"encoding/binary"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
	"github.com/finalappstore/stupidsamba/internal/vfs"
)

func init() {
	registerCreateContext(createContextSpec{
		Names: []string{wire.CreateContextMxAc},
		Order: createContextOrderMxAc,
		New: func(req *wire.CreateRequest) createContextHandler {
			return &mxAcHandler{req: req}
		},
	})
}

// mxAcHandler 处理 "MxAc"（SMB2_CREATE_QUERY_MAXIMAL_ACCESS，
// MS-SMB2 §2.2.13.2.5 请求 / §2.2.14.2.5 响应）。
type mxAcHandler struct {
	createContextBase
	req *wire.CreateRequest
	r   mxAcRequest
}

// Parse 校验请求载荷。畸形长度让整个 CREATE 失败（与 Samba 一致）。
func (h *mxAcHandler) Parse(ctx *Context, _ string, _ []byte) error {
	r, err := parseMxAcRequest(h.req)
	if err != nil {
		ctx.Log.Debug("MxAc create context 长度非法")
		return err
	}
	h.r = r
	return nil
}

// Respond 回报**真实**的 MaximalAccess，见 mxAcMaximalAccess。
func (h *mxAcHandler) Respond(ctx *Context, _ *Open, resp *wire.CreateResponse, attr *vfs.Attr) error {
	if !h.r.wantResponse(vfs.TimeToFiletime(attr.WriteTime)) {
		return nil
	}
	resp.Contexts = append(resp.Contexts, wire.CreateContext{
		Name: wire.CreateContextMxAc,
		Data: wire.MaximalAccessContext{
			// 我们的 MaximalAccess 是纯配置推导，不会失败，恒回
			// STATUS_SUCCESS。规范允许在算不出来时回非零状态 ——
			// 那时客户端会忽略掩码，好过给它一个错的（AGENTS.md §8）。
			QueryStatus:   uint32(status.Success),
			MaximalAccess: mxAcMaximalAccess(ctx.Tree, attr),
		}.Encode(),
	})
	return nil
}

// mxAcRequest 是解析后的 MxAc **请求**（MS-SMB2 §2.2.13.2.5）。
type mxAcRequest struct {
	// Present 表示请求里确实带了 MxAc context。
	Present bool
	// Timestamp 是客户端上次取得 MaximalAccess 时该文件的 LastWriteTime
	// （FILETIME）。context 的 Data 为空时是 0。
	Timestamp uint64
}

// parseMxAcRequest 校验并解析 MxAc 请求 context。
//
// 请求 Data 只允许两种长度（Samba `source3/smbd/smb2_create.c:1608`
// 逐字核对）：
//
//	0 字节  —— 无条件查询，Timestamp 视为 0
//	8 字节  —— Timestamp(8) FILETIME，小端
//
// 其余长度**让整个 CREATE 失败**回 STATUS_INVALID_PARAMETER，与 Samba 一致。
// 注意「空 Data」是完全合法的常见形态，绝不能当成畸形输入
// （Windows 客户端大多就发空的）。
func parseMxAcRequest(req *wire.CreateRequest) (mxAcRequest, error) {
	data, ok := wire.FindCreateContext(req.Contexts, wire.CreateContextMxAc)
	if !ok {
		return mxAcRequest{}, nil
	}
	switch len(data) {
	case 0:
		return mxAcRequest{Present: true}, nil
	case 8:
		return mxAcRequest{Present: true, Timestamp: mxAcLE.Uint64(data)}, nil
	default:
		return mxAcRequest{}, status.InvalidParameter
	}
}

// mxAcLE：MxAc 各字段与 SMB2 报文体一致，**小端**。
var mxAcLE = binary.LittleEndian

// wantResponse 报告是否需要在响应里回 MxAc context。
//
// Samba `smb2_create.c:1875` 的条件是 `last_write_time != max_access_time`：
// 客户端把「我上次查 MaximalAccess 时这个文件的 LastWriteTime」带上来，
// 若文件自那以后没被改过，客户端手里的缓存仍然有效，服务端就**不回**这个
// context 以省掉 8 字节与一次计算。这不是可有可无的优化 —— 它是 Windows
// 的实际线上行为，客户端据此判断"没回 = 沿用缓存"。
//
// Data 为空（Timestamp=0）时恒回：0 不可能等于任何真实文件的 FILETIME。
func (m mxAcRequest) wantResponse(lastWrite uint64) bool {
	return m.Present && lastWrite != m.Timestamp
}

// mxAcStripWrite 是只读场景要从 MaximalAccess 里剥掉的写位。
// FILE_READ_DATA / FILE_EXECUTE / FILE_READ_EA / READ_CONTROL / SYNCHRONIZE
// 与只读共享兼容，不在其列（MS-DTYP §2.4.3 访问掩码定义）。
const mxAcStripWrite = uint32(
	wire.FileWriteData | wire.FileAppendData |
		wire.FileWriteEA | wire.FileWriteAttributes | wire.Delete)

// mxAcMaximalAccess 计算 MxAc create context 应回报的 MaximalAccess
// （MS-SMB2 §2.2.14.2.5）。它必须是**真实**授权结果，绝不能无脑回
// 0x001F01FF（全权限），否则只读共享在 Finder/Explorer 里会显示成「可写」、
// 用户点了写才报错（AGENTS.md §8：不要为连上而放宽语义）。
//
// 授权依据只有配置（§1.1 C8，不读宿主 ACL），因此：
//   - 只读共享：maximalAccessFor 已回 0x00120089（不含上述写位）；
//   - 普通文件自身带 DOS 只读位：再剥掉写位（目录的只读位语义不同，不剥）。
func mxAcMaximalAccess(t *Tree, attr *vfs.Attr) uint32 {
	mx := uint32(maximalAccessFor(t))
	if attr.FileAttributes&vfs.FileAttributeReadonly != 0 &&
		attr.FileAttributes&vfs.FileAttributeDirectory == 0 {
		mx &^= mxAcStripWrite
	}
	return mx
}
