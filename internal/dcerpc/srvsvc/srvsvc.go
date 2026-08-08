// Package srvsvc 实现 MS-SRVS（LanMan Server 服务）接口，运行在 DCERPC 之上、
// IPC$ 命名管道 \srvsvc 之内。最重要的是 NetShareEnumAll（opnum 15），它是
// `smbclient -L`、`net view`、Windows 资源管理器与 macOS Finder 浏览共享列表的必经之路。
//
// 本包只依赖 internal/dcerpc（报文/NDR 原语），**不 import internal/config**，
// 共享列表通过 dcerpc.ShareLister 注入，避免反向依赖。
package srvsvc

import (
	"encoding/binary"
	"fmt"

	"github.com/finalappstore/stupidsamba/internal/dcerpc"
)

// 接口 UUID 与版本（MS-SRVS §1.1 / §3.1.4）。
// 4b324fc8-1670-01d3-1278-5a47bf6ee188, v3.0。
var srvsvcUUID dcerpc.UUID

func init() {
	u, err := dcerpc.ParseUUID("4b324fc8-1670-01d3-1278-5a47bf6ee188")
	if err != nil {
		panic(fmt.Sprintf("srvsvc: 内部接口 UUID 非法: %v", err))
	}
	srvsvcUUID = u
}

// opnum（MS-SRVS §3.1.4）。
const (
	opnumNetShareEnumAll  uint16 = 15 // NetrShareEnum
	opnumNetShareGetInfo uint16 = 16 // NetrShareGetInfo
	opnumNetServerGetInfo uint16 = 21 // NetrServerGetInfo
)

// 共享类型位（MS-SRVS §2.2.2.1）。
const (
	stypeDiskTree = 0x00000000 // STYPE_DISKTREE
	stypeIPC      = 0x00000003 // STYPE_IPC
	stypeSpecial  = 0x80000000 // STYPE_SPECIAL（隐藏；IPC$ 带此位）
)

// WERROR 码（MS-ERREF / winerror.h / lmerr.h）。
const (
	werrOK               uint32 = 0x00000000
	werrNotSupported     uint32 = 0x00000032 // ERROR_NOT_SUPPORTED (50)
	nerrNetNameNotFound  uint32 = 0x00000906 // NERR_NetNameNotFound (2310)
)

// secAddr 是 bind_ack 回送的 secondary address（MS-RPCE §2.2.2.4），必须是 "\\PIPE\\srvsvc"。
const secAddr = "\x5C\x50\x49\x50\x45\x5C\x73\x72\x76\x73\x76\x63" // "\PIPE\srvsvc"

// Handler 实现 dcerpc.Handler，负责 srvsvc 接口的 PDU 分发。
type Handler struct {
	Lister dcerpc.ShareLister

	// 以下字段供 NetServerGetInfo（opnum 21）/ 调试使用，可由 server 层装配时设置。
	ServerName    string // sv101_name（NetServerGetInfo 返回；空串表示本地）
	ServerComment string // sv101_comment
	MajorVersion  uint32
	MinorVersion  uint32
}

// NewHandler 创建 srvsvc Handler。lister 提供要对外宣告的共享列表。
func NewHandler(lister dcerpc.ShareLister) *Handler {
	return &Handler{
		Lister:        lister,
		ServerName:    "",
		ServerComment: "stupidSamba",
		MajorVersion:  10,
		MinorVersion:  0,
	}
}

// Handle 实现 dcerpc.Handler：解析一个完整 PDU 并返回响应 PDU。
func (h *Handler) Handle(input []byte) ([]byte, error) {
	pdu, err := dcerpc.ParsePDU(input)
	if err != nil {
		return dcerpc.MarshalFault(0, dcerpc.NCAStatusProtocolError), nil
	}
	switch pdu.PType {
	case dcerpc.PTYPEBind, dcerpc.PTYPEAlterContext:
		return h.handleBind(pdu), nil
	case dcerpc.PTYPERequest:
		return h.handleRequest(pdu)
	default:
		return dcerpc.MarshalFault(pdu.CallID, dcerpc.NCAStatusProtocolError), nil
	}
}

func (h *Handler) handleBind(pdu *dcerpc.PDU) []byte {
	if len(pdu.ContextElems) == 0 {
		return dcerpc.MarshalBindNak(pdu.CallID, 0x00000002) // provider reject
	}
	// 逐个 context 给结果：smbclient 会同时提出 NDR32 与
	// bind time feature negotiation 两个 context，p_result_list 必须一一对应，
	// 少一项客户端就会按 frag_length 继续读、读不到而报 NT_STATUS_BUFFER_TOO_SMALL。
	results, ok := dcerpc.NegotiateResults(pdu.ContextElems, srvsvcUUID)
	if !ok {
		return dcerpc.MarshalBindNak(pdu.CallID, 0x0002) // provider rejection
	}
	return dcerpc.MarshalBindAck(pdu.CallID, secAddr, results)
}

func (h *Handler) handleRequest(pdu *dcerpc.PDU) ([]byte, error) {
	switch pdu.Opnum {
	case opnumNetShareEnumAll:
		return h.netShareEnumAll(pdu)
	case opnumNetShareGetInfo:
		return h.netShareGetInfo(pdu)
	case opnumNetServerGetInfo:
		return h.netServerGetInfo(pdu)
	default:
		// 未实现的方法一律回 fault（nca_op_rng_error），绝不 panic。
		return dcerpc.MarshalFault(pdu.CallID, dcerpc.NCAStatusOpRangeError), nil
	}
}

// ---- NetrShareEnum（opnum 15） ----

func (h *Handler) netShareEnumAll(pdu *dcerpc.PDU) ([]byte, error) {
	req, err := decodeEnumAllReq(pdu.Stub)
	if err != nil {
		return dcerpc.MarshalFault(pdu.CallID, dcerpc.NCAStatusProtocolError), nil
	}
	if req.Level != 0 && req.Level != 1 {
		// 不支持的 level：返回 WERR_NOT_SUPPORTED，容器指针置空。
		return marshalEnumAllResp(pdu.CallID, req.Level, req.HasResume, nil, werrNotSupported), nil
	}
	all := h.Lister.Shares()
	entries := make([]dcerpc.ShareEntry, 0, len(all))
	for _, s := range all {
		e := s
		if req.Level == 0 {
			e.Type = 0
			e.Remark = ""
		}
		entries = append(entries, e)
	}
	return marshalEnumAllResp(pdu.CallID, req.Level, req.HasResume, entries, werrOK), nil
}

type enumAllReq struct {
	ServerName string
	Level      uint32
	Resume     uint32
	HasResume  bool
}

// decodeEnumAllReq 解析 NetrShareEnum 请求 stub（MS-SRVS §3.1.4.10）。
//
//	NET_API_STATUS NetrShareEnum(
//	  [in, string, unique] SRVSVC_HANDLE ServerName,
//	  [in, out] LPSHARE_ENUM_STRUCT InfoStruct,   // ref → 无 referent id
//	  [in] DWORD PreferedMaximumLength,
//	  [out] DWORD* TotalEntries,
//	  [in, out, unique] DWORD* ResumeHandle);
//
// SHARE_ENUM_STRUCT（§2.2.4.38）= Level(u32) + 封装联合。NDR 封装联合的线格式是
// switch 值(u32) 后跟选中分支，这里分支是 SHARE_INFO_x_CONTAINER 的 unique 指针。
func decodeEnumAllReq(stub []byte) (enumAllReq, error) {
	d := dcerpc.NewNdrDec(stub, binary.LittleEndian)
	var r enumAllReq

	d.Ptr(func() { r.ServerName = d.WString() })
	r.Level = d.U32()
	_ = d.U32() // union switch（与 Level 相同）
	d.Ptr(func() {
		// SHARE_INFO_x_CONTAINER：EntriesRead + Buffer 指针。
		// 请求里客户端总是传 0/NULL，读掉即可。
		_ = d.U32()
		d.Ptr(func() {
			_ = d.U32() // 数组 max_count
		})
	})
	_ = d.U32() // PreferedMaximumLength（本实现一次返回全部，忽略）
	d.Ptr(func() {
		r.Resume = d.U32()
		r.HasResume = true
	})
	if err := d.Err(); err != nil {
		return r, err
	}
	return r, nil
}

// marshalEnumAllResp 构造 NetrShareEnum 响应 stub 并封装为 response PDU。
//
// 线格式（[out] 参数按声明顺序）：
//
//	InfoStruct : Level u32 | switch u32 | Container 指针 → { EntriesRead u32 | Buffer 指针 → 数组 }
//	TotalEntries : u32     （[out] 顶层指针是 ref，**不带 referent id**）
//	ResumeHandle : unique 指针 → u32（客户端传了才回）
//	返回值 WERROR : u32
func marshalEnumAllResp(callID, level uint32, hasResume bool,
	entries []dcerpc.ShareEntry, werr uint32) []byte {

	e := dcerpc.NewNdrEnc(binary.LittleEndian)
	e.U32(level) // SHARE_ENUM_STRUCT.Level
	e.U32(level) // 联合 switch 值

	if werr == werrOK {
		e.Ptr(func() { // SHARE_INFO_x_CONTAINER
			e.U32(uint32(len(entries))) // EntriesRead
			e.Ptr(func() {              // Buffer：conformant 数组
				e.U32(uint32(len(entries))) // max_count
				// 数组元素含指针：所有元素的固定部分先排完，
				// 字符串 referent 统一跟在数组之后（C706 §14.3.12.3）。
				e.Deferred(func() {
					for _, ent := range entries {
						ent := ent
						e.Ptr(func() { e.WString(ent.Name) })
						if level != 0 {
							e.U32(ent.Type)
							e.Ptr(func() { e.WString(ent.Remark) })
						}
					}
				})
			})
		})
	} else {
		e.Ptr(nil)
	}

	e.U32(uint32(len(entries))) // TotalEntries（ref 指针，直接给值）
	if hasResume {
		e.Ptr(func() { e.U32(0) }) // ResumeHandle：一次返回完，回显 0
	} else {
		e.Ptr(nil)
	}
	e.U32(werr) // NET_API_STATUS
	return dcerpc.MarshalResponse(callID, e.Bytes())
}

// ---- NetrShareGetInfo（opnum 16） ----

func (h *Handler) netShareGetInfo(pdu *dcerpc.PDU) ([]byte, error) {
	req, err := decodeGetInfoReq(pdu.Stub)
	if err != nil {
		return dcerpc.MarshalFault(pdu.CallID, dcerpc.NCAStatusProtocolError), nil
	}
	if req.Level != 0 && req.Level != 1 {
		return marshalGetInfoResp(pdu.CallID, req.Level, nil, werrNotSupported), nil
	}
	var found *dcerpc.ShareEntry
	for i := range h.Lister.Shares() {
		s := h.Lister.Shares()[i]
		if equalFold(s.Name, req.NetName) {
			f := s
			if req.Level == 0 {
				f.Type = 0
				f.Remark = ""
			}
			found = &f
			break
		}
	}
	if found == nil {
		return marshalGetInfoResp(pdu.CallID, req.Level, nil, nerrNetNameNotFound), nil
	}
	return marshalGetInfoResp(pdu.CallID, req.Level, found, werrOK), nil
}

type getInfoReq struct {
	ServerName string
	NetName    string
	Level      uint32
}

// decodeGetInfoReq 解析 NetrShareGetInfo 请求 stub（MS-SRVS §3.1.4.11）：
// ServerName(unique) | NetName(unique) | Level。
func decodeGetInfoReq(stub []byte) (getInfoReq, error) {
	d := dcerpc.NewNdrDec(stub, binary.LittleEndian)
	var r getInfoReq
	d.Ptr(func() { r.ServerName = d.WString() })
	d.Ptr(func() { r.NetName = d.WString() })
	r.Level = d.U32()
	if err := d.Err(); err != nil {
		return r, err
	}
	return r, nil
}

// marshalGetInfoResp 构造 NetrShareGetInfo 响应 stub（MS-SRVS §3.1.4.11）。
//
// [out, switch_is(Level), ref] LPSHARE_INFO InfoStruct → 线格式是
// switch 值(u32) + 分支的 unique 指针 → SHARE_INFO_x；最后是返回值 WERROR。
func marshalGetInfoResp(callID, level uint32, entry *dcerpc.ShareEntry, werr uint32) []byte {
	e := dcerpc.NewNdrEnc(binary.LittleEndian)
	e.U32(level) // 联合 switch 值
	if entry != nil {
		e.Ptr(func() {
			// SHARE_INFO_x 是结构体：内部指针延迟到结构体固定部分之后。
			e.Deferred(func() {
				e.Ptr(func() { e.WString(entry.Name) })
				if level != 0 {
					e.U32(entry.Type)
					e.Ptr(func() { e.WString(entry.Remark) })
				}
			})
		})
	} else {
		e.Ptr(nil) // 共享不存在或 level 不支持
	}
	e.U32(werr)
	return dcerpc.MarshalResponse(callID, e.Bytes())
}

// ---- NetrServerGetInfo（opnum 21） ----

func (h *Handler) netServerGetInfo(pdu *dcerpc.PDU) ([]byte, error) {
	req, err := decodeServerGetInfoReq(pdu.Stub)
	if err != nil {
		return dcerpc.MarshalFault(pdu.CallID, dcerpc.NCAStatusProtocolError), nil
	}
	if req.Level != 101 {
		return marshalServerGetInfoResp(pdu.CallID, req.Level, nil, werrNotSupported), nil
	}
	info := &serverInfo{
		PlatformID:    500, // PLATFORM_ID_NT
		Name:          h.ServerName,
		VersionMajor:  h.MajorVersion,
		VersionMinor:  h.MinorVersion,
		Type:          0x00800002, // SV_TYPE_NT | SV_TYPE_SERVER
		Comment:       h.ServerComment,
	}
	return marshalServerGetInfoResp(pdu.CallID, 101, info, werrOK), nil
}

type serverInfo struct {
	PlatformID    uint32
	Name          string
	VersionMajor  uint32
	VersionMinor  uint32
	Type          uint32
	Comment       string
}

type serverGetInfoReq struct {
	ServerName string
	Level      uint32
}

// decodeServerGetInfoReq 解析 NetrServerGetInfo 请求 stub（MS-SRVS §3.1.4.17）：
// ServerName(unique) | Level。
func decodeServerGetInfoReq(stub []byte) (serverGetInfoReq, error) {
	d := dcerpc.NewNdrDec(stub, binary.LittleEndian)
	var r serverGetInfoReq
	d.Ptr(func() { r.ServerName = d.WString() })
	r.Level = d.U32()
	if err := d.Err(); err != nil {
		return r, err
	}
	return r, nil
}

// marshalServerGetInfoResp 构造 NetrServerGetInfo 响应 stub
// （MS-SRVS §3.1.4.17，SERVER_INFO_101 见 §2.2.4.44）。
// 与 NetrShareGetInfo 同形：switch 值 + 分支指针 + WERROR。
func marshalServerGetInfoResp(callID, level uint32, info *serverInfo, werr uint32) []byte {
	e := dcerpc.NewNdrEnc(binary.LittleEndian)
	e.U32(level)
	if info != nil && level == 101 {
		e.Ptr(func() {
			e.Deferred(func() {
				e.U32(info.PlatformID) // sv101_platform_id
				e.Ptr(func() { e.WString(info.Name) })
				e.U32(info.VersionMajor)
				e.U32(info.VersionMinor)
				e.U32(info.Type)
				e.Ptr(func() { e.WString(info.Comment) })
			})
		})
	} else {
		e.Ptr(nil)
	}
	e.U32(werr)
	return dcerpc.MarshalResponse(callID, e.Bytes())
}

// equalFold 是 ASCII 不区分大小写的相等比较（共享名均为 ASCII）。
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
