package nbns

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
)

// ---------------------------------------------------------------------------
// NBNS / NBDS 报文（RFC 1002 §4.2、§4.3、§4.4）
//
// 只实现"让 Windows 与 macOS 能发现本机"所需的那一半：
//
//	NBNS（UDP 137）  应答**名字查询**（\\NAME 能不能解析）与**节点状态**
//	NBDS（UDP 138）  主动发**主机宣告**，把自己放进浏览列表
//
// 刻意**不做**的（Samba nmbd 的全量功能，范围远超本项目的目标）：
// WINS 服务器、浏览主控选举、域主控浏览、名称注册冲突仲裁、
// 备份列表请求。这些属于"接入一个 NetBIOS 工作组并参与其治理"，
// 而这里要的只是"让别人能看见我"。
// ---------------------------------------------------------------------------

// 端口（RFC 1002 §4.2.1）。两者都是**特权端口**，绑不上要给与
// SMB 445 同款的人话提示（由调用方负责，见 cmd 层）。
const (
	PortNameService = 137
	PortDatagram    = 138
)

// NBNS 头部：6 个 uint16，共 12 字节（RFC 1002 §4.2.1）。
const headerSize = 12

// 标志位（RFC 1002 §4.2.1.2）。
const (
	// flagResponse 置位表示这是响应（QR）。
	flagResponse uint16 = 0x8000
	// flagAuthoritative 置位表示应答的名字是本节点权威持有的（AA）。
	//
	// 必须置位：不置的话 Windows 会把这个应答当成缓存转述，
	// 在部分版本上表现为解析结果不被采信。
	flagAuthoritative uint16 = 0x0400
	// flagRecursionDesired 从请求里原样回显（RD）。
	flagRecursionDesired uint16 = 0x0100
	// flagBroadcast 表示请求是广播发出的（B）。
	flagBroadcast uint16 = 0x0010
	// opcodeMask 是 OPCODE 字段的掩码。
	opcodeMask uint16 = 0x7800
	// opcodeQuery 是「名字查询」操作码。
	opcodeQuery uint16 = 0x0000
)

// 查询类型（RFC 1002 §4.2.1.3）。
const (
	// qTypeNB 是普通的 NetBIOS 名字查询。
	qTypeNB uint16 = 0x0020
	// qTypeNBStat 是节点状态查询（nbtstat 用的）。
	qTypeNBStat uint16 = 0x0021
	// qClassIN 是 Internet 类，NBNS 里恒为 1。
	qClassIN uint16 = 0x0001
)

// NB 应答里 RDATA 的 NB_FLAGS（RFC 1002 §4.2.1.4）。
const (
	// nbFlagGroup 置位表示这是一个组名（G），否则是唯一名。
	nbFlagGroup uint16 = 0x8000
	// nbFlagOntBNode 是「所有者节点类型 = B 节点」，值为 0（不用或）。
	nbFlagOntBNode uint16 = 0x0000
)

// nameQuery 是解析出来的一次名字查询。
type nameQuery struct {
	// id 是事务 ID，应答里必须原样回显（客户端靠它把响应对上请求）。
	id uint16
	// name 是被查询的 NetBIOS 名。
	name Name
	// qtype 是查询类型（NB / NBSTAT）。
	qtype uint16
	// broadcast 表示请求是广播来的。
	broadcast bool
}

// parseQuery 解析一条 NBNS 查询。返回 nil 表示它不是需要应答的查询。
func parseQuery(b []byte) *nameQuery {
	if len(b) < headerSize+wireNameSize+4 {
		return nil
	}
	q := &nameQuery{id: binary.BigEndian.Uint16(b[0:])}
	flags := binary.BigEndian.Uint16(b[2:])
	// 响应报文不处理；只处理「查询」操作码。
	if flags&flagResponse != 0 {
		return nil
	}
	if flags&opcodeMask != opcodeQuery {
		return nil
	}
	q.broadcast = flags&flagBroadcast != 0

	qdcount := binary.BigEndian.Uint16(b[4:])
	if qdcount == 0 {
		return nil
	}

	name, ok := parseWireName(b[headerSize:])
	if !ok {
		return nil
	}
	q.name = name
	q.qtype = binary.BigEndian.Uint16(b[headerSize+wireNameSize:])
	return q
}

// buildNameResponse 构造 NB 名字查询的肯定应答（RFC 1002 §4.2.2.1）。
//
// addr 是应答里给出的 IP；group 表示这是个组名（影响 NB_FLAGS）。
func buildNameResponse(q *nameQuery, addr net.IP, group bool) []byte {
	ip4 := addr.To4()
	if ip4 == nil {
		return nil
	}
	out := make([]byte, 0, headerSize+wireNameSize+10+6)
	out = appendHeader(out, q.id, flagResponse|flagAuthoritative|flagRecursionDesired, 0, 1, 0, 0)
	// Answer 段：Name + Type + Class + TTL + RDLENGTH + RDATA
	out = append(out, q.name.wire()...)
	out = binary.BigEndian.AppendUint16(out, qTypeNB)
	out = binary.BigEndian.AppendUint16(out, qClassIN)
	out = binary.BigEndian.AppendUint32(out, nameTTL)
	out = binary.BigEndian.AppendUint16(out, 6) // RDATA = NB_FLAGS(2) + IP(4)

	flags := nbFlagOntBNode
	if group {
		flags |= nbFlagGroup
	}
	out = binary.BigEndian.AppendUint16(out, flags)
	out = append(out, ip4...)
	return out
}

// nameTTL 是应答里给的 TTL（秒）。
//
// NetBIOS 名字是"随时可能变"的（机器上下线），nmbd 传统上给一个网络
// 分钟数（3 分钟）量级的值。太大会让客户端缓存住一台已经下线的机器。
const nameTTL uint32 = 300

// buildNodeStatusResponse 构造节点状态应答（RFC 1002 §4.2.3，nbtstat）。
//
// names 是本节点持有的名字表，全部回填给查询方。
func buildNodeStatusResponse(q *nameQuery, names []Name) []byte {
	if len(names) > 255 {
		names = names[:255]
	}
	// RDATA = NUM_NAMES(1) + 每条 18 字节 + 统计块 66 字节。
	rdata := make([]byte, 0, 1+len(names)*18+66)
	rdata = append(rdata, byte(len(names)))
	for _, n := range names {
		nb := n.Bytes()
		// 前 15 字节是名字（含填充空格），第 16 字节是后缀，
		// 然后 2 字节名字标志 —— 与一级编码无关，这里直接放原始字节。
		rdata = append(rdata, nb[:15]...)
		rdata = append(rdata, nb[15])
		rdata = binary.BigEndian.AppendUint16(rdata, nameFlagsFor(n))
	}
	// 统计块：UNIT_ID(6) + 60 字节计数器，全部填 0。
	// 我们不做 NetBIOS 层的收发统计，留空比编造数字诚实。
	rdata = append(rdata, make([]byte, 66)...)

	out := make([]byte, 0, headerSize+wireNameSize+10+len(rdata))
	out = appendHeader(out, q.id, flagResponse|flagAuthoritative|flagRecursionDesired, 0, 1, 0, 0)
	out = append(out, q.name.wire()...)
	out = binary.BigEndian.AppendUint16(out, qTypeNBStat)
	out = binary.BigEndian.AppendUint16(out, qClassIN)
	out = binary.BigEndian.AppendUint32(out, 0) // 节点状态不缓存
	out = binary.BigEndian.AppendUint16(out, uint16(len(rdata)))
	out = append(out, rdata...)
	return out
}

// nameFlagsFor 给出名字表里的名字标志（RFC 1002 §4.2.3.1）。
//
// 只区分唯一名与组名：其余位（正在注册/冲突/已删除）我们都没有对应状态。
func nameFlagsFor(n Name) uint16 {
	if n.Suffix == SuffixBrowserGroup {
		return nbFlagGroup
	}
	return 0
}

// appendHeader 写入 12 字节 NBNS 头。
func appendHeader(dst []byte, id, flags, qd, an, ns, ar uint16) []byte {
	dst = binary.BigEndian.AppendUint16(dst, id)
	dst = binary.BigEndian.AppendUint16(dst, flags)
	dst = binary.BigEndian.AppendUint16(dst, qd)
	dst = binary.BigEndian.AppendUint16(dst, an)
	dst = binary.BigEndian.AppendUint16(dst, ns)
	return binary.BigEndian.AppendUint16(dst, ar)
}

// ---------------------------------------------------------------------------
// NBDS 主机宣告（RFC 1002 §4.4 + Microsoft 浏览服务）
//
// 这是"主动告诉网络上所有人：我是台文件服务器"。光有 NBNS 应答只解决
// 别人**知道名字**时的解析，而宣告解决的是别人**还不知道有你**。
// ---------------------------------------------------------------------------

// 数据报类型（RFC 1002 §4.4.1）。
const (
	msgTypeBroadcast uint8 = 0x12
)

// SMB 主机宣告的字段（Microsoft 浏览服务规范）。
const (
	// smbHostAnnounce 是「主机宣告」命令。
	smbHostAnnounce uint8 = 0x01
	// announcementPeriodicity 是宣告周期（毫秒）。
	//
	// 12 分钟是 Samba 与 Windows 的常规取值：够让新上线的机器在一两分钟内
	// 被看见，又不至于把局域网刷满。
	announcementPeriodicity uint32 = 12 * 60 * 1000
	// browserSignature 是宣告报文的固定签名（0xAA55）。
	browserSignature uint16 = 0xAA55
)

// ServerType 是宣告里的服务器类型位（Microsoft 浏览服务）。
const (
	svTypeWorkstation uint32 = 0x00000001
	svTypeServer      uint32 = 0x00000002
	svTypeNT          uint32 = 0x00001000
	svTypeServerNT    uint32 = 0x00008000
)

// DefaultServerType 是本服务宣告的服务器类型。
//
// 工作站 | 服务器 | NT 工作站 | NT 服务器 —— 不声明自己是主控浏览器，
// 因为我们**不参加选举**（见文件头「刻意不做」那段）。谎报主控浏览器会
// 让别的机器真的来问我们要浏览列表，而我们答不上来。
const DefaultServerType = svTypeWorkstation | svTypeServer | svTypeNT | svTypeServerNT

// buildHostAnnouncement 构造一条主机宣告数据报。
//
// src 是本机的 IPv4；destName 是目的地 NetBIOS 名 —— 通常是
// <工作组><0x1e> 或 __MSBROWSE__，两者都要发（见 Responder）。
func buildHostAnnouncement(src net.IP, sourceName, destName Name, serverName, comment string) ([]byte, error) {
	ip4 := src.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("宣告需要 IPv4 地址，实际 %v", src)
	}

	// SMB 体：命令 + 更新计数 + 周期 + 服务器名(16) + 版本(2) + 类型(4)
	//        + 浏览器版本(2) + 签名(2) + 注释（以 NUL 结尾）。
	var smb []byte
	smb = append(smb, smbHostAnnounce)
	smb = append(smb, 0) // UpdateCount，本实现不做增量更新
	smb = binary.BigEndian.AppendUint32(smb, announcementPeriodicity)
	smb = append(smb, paddedName(serverName)...)
	smb = append(smb, 1, 0) // OS 版本：不做版本宣称，给 1.0
	smb = binary.BigEndian.AppendUint32(smb, DefaultServerType)
	smb = append(smb, 1, 0) // 浏览器版本 1.0
	smb = binary.BigEndian.AppendUint16(smb, browserSignature)
	smb = append(smb, []byte(comment)...)
	smb = append(smb, 0) // 注释必须以 NUL 结尾

	// NBDS 头（RFC 1002 §4.4.3）。
	//
	// DGM_LENGTH 从 **PACKET_OFFSET 字段**开始算（不是从报文开头），
	// 这是最容易记错的一处：它包含 Packet Offset 自己(2) + 两个 34 字节名字
	// + SMB 体。
	body := make([]byte, 0, 2+wireNameSize*2+len(smb))
	body = binary.BigEndian.AppendUint16(body, 0) // PACKET_OFFSET，本实现不分片
	body = append(body, sourceName.wire()...)
	body = append(body, destName.wire()...)
	body = append(body, smb...)

	out := make([]byte, 0, 14+len(body))
	out = append(out, msgTypeBroadcast)
	// FLAGS：低 4 位是 SNT（源节点类型），2 = B 节点。
	out = append(out, 0x02)
	out = binary.BigEndian.AppendUint16(out, uint16(rand.Intn(0x10000))) // DGM_ID
	out = append(out, ip4...)
	out = binary.BigEndian.AppendUint16(out, PortDatagram)
	out = binary.BigEndian.AppendUint16(out, uint16(len(body)))
	out = append(out, body...)
	return out, nil
}

// paddedName 把服务器名截断/填充到宣告报文要求的定长 16 字节。
//
// 与 NetBIOS 名的 16 字节不同：这里是 SMB 体里的字段，**不**做一级编码，
// 直接放原始字符并以 NUL 结尾。
func paddedName(s string) []byte {
	out := make([]byte, 16)
	if len(s) > 15 {
		s = s[:15]
	}
	copy(out, s)
	return out
}
