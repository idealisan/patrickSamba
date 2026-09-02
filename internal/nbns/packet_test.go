package nbns

import (
	"encoding/binary"
	"net"
	"testing"
)

// 本文件钉的是 NetBIOS 的编码与报文布局。
//
// 这些字节级细节一旦错了，表现是"网络里根本看不见这台机器"或
// "\\NAME 解析到错的 IP"，而服务端日志一切正常 —— 只能靠在这里钉死。
//
// 变异自检方向：
//   - 把一级编码的 'A'+nibble 改成 'a'+nibble → TestLevel1RoundTrip 变红；
//   - 把 wire() 的长度前缀 0x20 写成 32 的十进制字节 → TestWireNameLayout 变红；
//   - 把 buildNameResponse 的 RDLENGTH 写成 4 → TestBuildNameResponse 变红；
//   - 把 buildHostAnnouncement 的 DGM_LENGTH 改成从报文头算 →
//     TestBuildHostAnnouncement 变红（差 14 字节，Windows 会整包丢弃）；
//   - 把 namesEqual 的 EqualFold 改成 == → TestNamesEqualIgnoresCase 变红。

// ---- 名字编码 ----

func TestNameBytesPadding(t *testing.T) {
	n := Name{Label: "SRV", Suffix: SuffixFileServer}
	b := n.Bytes()
	if len(b) != NameSize {
		t.Fatalf("NetBIOS 名应定长 %d，实际 %d", NameSize, len(b))
	}
	if string(b[:3]) != "SRV" {
		t.Fatalf("名字部分应为 SRV，实际 %q", b[:3])
	}
	for i := 3; i < NameSize-1; i++ {
		if b[i] != ' ' {
			t.Fatalf("第 %d 字节应为空格填充，实际 %q", i, b[i])
		}
	}
	if b[NameSize-1] != SuffixFileServer {
		t.Fatalf("末字节应是后缀 %#x，实际 %#x", SuffixFileServer, b[NameSize-1])
	}
}

func TestLevel1RoundTrip(t *testing.T) {
	nb := Name{Label: "MYHOST", Suffix: SuffixFileServer}.Bytes()
	enc := encodeLevel1(nb)
	if len(enc) != NameSize*2 {
		t.Fatalf("一级编码后应为 %d 字节，实际 %d", NameSize*2, len(enc))
	}
	back, ok := decodeLevel1(enc)
	if !ok {
		t.Fatal("一级解码失败")
	}
	if back != nb {
		t.Fatalf("往返不一致：%x → %x", nb, back)
	}
}

// TestDecodeLevel1RejectsGarbage：非法字符不是 NetBIOS 名，不能糊弄过去。
func TestDecodeLevel1RejectsGarbage(t *testing.T) {
	if _, ok := decodeLevel1([]byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")); ok {
		t.Fatal("含非法字符的输入应被拒（一级编码只用 A..P）")
	}
	if _, ok := decodeLevel1([]byte("too short")); ok {
		t.Fatal("长度不是 32 的输入应被拒")
	}
}

func TestWireNameLayout(t *testing.T) {
	w := Name{Label: "SRV", Suffix: SuffixWorkstation}.wire()
	if len(w) != wireNameSize {
		t.Fatalf("报文里的 Name 字段应 %d 字节，实际 %d", wireNameSize, len(w))
	}
	if w[0] != 0x20 {
		t.Fatalf("首字节应是长度前缀 0x20，实际 %#x", w[0])
	}
	if w[len(w)-1] != 0x00 {
		t.Fatalf("末字节应是 0x00 终止符，实际 %#x", w[len(w)-1])
	}
}

func TestWireNameRoundTrip(t *testing.T) {
	want := Name{Label: "MYHOST", Suffix: SuffixFileServer}
	got, ok := parseWireName(want.wire())
	if !ok {
		t.Fatal("解析失败")
	}
	if !namesEqual(want, got) {
		t.Fatalf("往返不一致：%v → %v", want, got)
	}
}

func TestParseWireNameRejectsBadLengthPrefix(t *testing.T) {
	w := Name{Label: "SRV", Suffix: SuffixFileServer}.wire()
	w[0] = 0x10
	if _, ok := parseWireName(w); ok {
		t.Fatal("长度前缀不是 0x20 应被拒")
	}
}

func TestNamesEqualIgnoresCase(t *testing.T) {
	if !namesEqual(Name{Label: "myserver", Suffix: 0x20}, Name{Label: "MYSERVER", Suffix: 0x20}) {
		t.Fatal("NetBIOS 名比较应大小写不敏感")
	}
	if namesEqual(Name{Label: "A", Suffix: 0x20}, Name{Label: "A", Suffix: 0x00}) {
		t.Fatal("后缀不同应视为不同的名字")
	}
}

// ---- 查询解析 ----

// buildQuery 手工拼一条 NBNS 查询。
func buildQuery(id uint16, flags uint16, n Name, qtype uint16) []byte {
	b := make([]byte, 0, headerSize+wireNameSize+4)
	b = appendHeader(b, id, flags, 1, 0, 0, 0)
	b = append(b, n.wire()...)
	b = binary.BigEndian.AppendUint16(b, qtype)
	b = binary.BigEndian.AppendUint16(b, qClassIN)
	return b
}

func TestParseQuery(t *testing.T) {
	n := Name{Label: "MYHOST", Suffix: SuffixFileServer}
	q := parseQuery(buildQuery(0x1234, flagBroadcast|flagRecursionDesired, n, qTypeNB))
	if q == nil {
		t.Fatal("应能解析出查询")
	}
	if q.id != 0x1234 {
		t.Fatalf("事务 ID 应为 0x1234，实际 %#x", q.id)
	}
	if !namesEqual(q.name, n) {
		t.Fatalf("名字应为 %v，实际 %v", n, q.name)
	}
	if q.qtype != qTypeNB {
		t.Fatalf("查询类型应为 NB，实际 %#x", q.qtype)
	}
	if !q.broadcast {
		t.Fatal("应识别出这是广播查询")
	}
}

// TestParseQueryIgnoresResponses：响应报文不是查询，不能拿去应答 ——
// 否则两个 NetBIOS 节点会互相应答对方的响应，形成回环。
func TestParseQueryIgnoresResponses(t *testing.T) {
	n := Name{Label: "MYHOST", Suffix: SuffixFileServer}
	if parseQuery(buildQuery(1, flagResponse, n, qTypeNB)) != nil {
		t.Fatal("响应报文不应被当成查询")
	}
}

func TestParseQueryIgnoresNonQueryOpcode(t *testing.T) {
	n := Name{Label: "MYHOST", Suffix: SuffixFileServer}
	// OPCODE = 5（注册）不是查询。
	if parseQuery(buildQuery(1, 5<<11, n, qTypeNB)) != nil {
		t.Fatal("非查询操作码应被忽略")
	}
}

func TestParseQueryIgnoresEmptyQuestion(t *testing.T) {
	b := appendHeader(nil, 1, flagBroadcast, 0, 0, 0, 0)
	if parseQuery(b) != nil {
		t.Fatal("没有问题段的报文应被忽略")
	}
}

func TestParseQueryRejectsTruncated(t *testing.T) {
	if parseQuery([]byte{0x00, 0x01}) != nil {
		t.Fatal("过短的报文应被拒")
	}
}

// ---- 响应构造 ----

func TestBuildNameResponse(t *testing.T) {
	n := Name{Label: "MYHOST", Suffix: SuffixFileServer}
	ip := net.IPv4(192, 168, 1, 10)
	q := parseQuery(buildQuery(0xabcd, flagBroadcast, n, qTypeNB))
	resp := buildNameResponse(q, ip, false)
	if resp == nil {
		t.Fatal("构造失败")
	}

	// 头：响应 + 权威 + ANCOUNT=1。
	flags := binary.BigEndian.Uint16(resp[2:])
	if flags&flagResponse == 0 {
		t.Fatal("应答必须置 QR 位")
	}
	if flags&flagAuthoritative == 0 {
		t.Fatal("应答必须置 AA 位，否则 Windows 可能不采信")
	}
	if got := binary.BigEndian.Uint16(resp[0:]); got != 0xabcd {
		t.Fatalf("事务 ID 应原样回显 0xabcd，实际 %#x", got)
	}
	if got := binary.BigEndian.Uint16(resp[6:]); got != 1 {
		t.Fatalf("ANCOUNT 应为 1，实际 %d", got)
	}

	// Answer 段：Name(34) + Type(2) + Class(2) + TTL(4) + RDLENGTH(2) + RDATA(6)
	off := headerSize
	if got := binary.BigEndian.Uint16(resp[off+wireNameSize:]); got != qTypeNB {
		t.Fatalf("RR 类型应为 NB，实际 %#x", got)
	}
	rdlen := binary.BigEndian.Uint16(resp[off+wireNameSize+8:])
	if rdlen != 6 {
		t.Fatalf("RDLENGTH 应为 6（NB_FLAGS + IPv4），实际 %d", rdlen)
	}
	rdata := resp[off+wireNameSize+10:]
	if nbFlags := binary.BigEndian.Uint16(rdata); nbFlags&nbFlagGroup != 0 {
		t.Fatalf("唯一名不应置 G（组）位，实际 %#x", nbFlags)
	}
	if !net.IP(rdata[2:6]).Equal(ip) {
		t.Fatalf("RDATA 里的 IP 应为 %v，实际 %v", ip, net.IP(rdata[2:6]))
	}
}

func TestBuildNameResponseGroupSetsGroupFlag(t *testing.T) {
	g := Name{Label: "WORKGROUP", Suffix: SuffixWorkstation}
	q := parseQuery(buildQuery(1, flagBroadcast, g, qTypeNB))
	resp := buildNameResponse(q, net.IPv4(10, 0, 0, 1), true)
	rdata := resp[headerSize+wireNameSize+10:]
	if nbFlags := binary.BigEndian.Uint16(rdata); nbFlags&nbFlagGroup == 0 {
		t.Fatalf("组名应答应置 G 位，实际 %#x", nbFlags)
	}
}

// TestBuildNameResponseNeedsIPv4：NetBIOS over TCP/IP 的地址字段是 4 字节，
// 给 IPv6 造不出合法应答，宁可不应答也不要截断。
func TestBuildNameResponseNeedsIPv4(t *testing.T) {
	n := Name{Label: "MYHOST", Suffix: SuffixFileServer}
	q := parseQuery(buildQuery(1, 0, n, qTypeNB))
	if buildNameResponse(q, net.ParseIP("2001:db8::1"), false) != nil {
		t.Fatal("IPv6 地址不应产生 NB 应答")
	}
}

func TestBuildNodeStatusResponse(t *testing.T) {
	q := parseQuery(buildQuery(7, 0, Name{Label: "MYHOST", Suffix: SuffixFileServer}, qTypeNBStat))
	names := []Name{
		{Label: "MYHOST", Suffix: SuffixFileServer},
		{Label: "WORKGROUP", Suffix: SuffixBrowserGroup},
	}
	resp := buildNodeStatusResponse(q, names)

	off := headerSize
	if got := binary.BigEndian.Uint16(resp[off+wireNameSize:]); got != qTypeNBStat {
		t.Fatalf("类型应为 NBSTAT，实际 %#x", got)
	}
	rdata := resp[off+wireNameSize+10:]
	if got := int(rdata[0]); got != len(names) {
		t.Fatalf("NUM_NAMES 应为 %d，实际 %d", len(names), got)
	}
	// 每条名字 18 字节：15 名字 + 1 后缀 + 2 标志。
	entry := rdata[1 : 1+18]
	if string(entry[:6]) != "MYHOST" {
		t.Fatalf("第一条名字应为 MYHOST，实际 %q", entry[:6])
	}
	if entry[15] != SuffixFileServer {
		t.Fatalf("第 16 字节应是后缀，实际 %#x", entry[15])
	}
	// 组名的标志位应带 G。
	groupEntry := rdata[1+18 : 1+36]
	if f := binary.BigEndian.Uint16(groupEntry[16:]); f&nbFlagGroup == 0 {
		t.Fatalf("组名条目应置 G 位，实际 %#x", f)
	}
}

// ---- 主机宣告 ----

func TestBuildHostAnnouncement(t *testing.T) {
	src := net.IPv4(192, 168, 1, 10)
	from := Name{Label: "MYHOST", Suffix: SuffixFileServer}
	to := Name{Label: "WORKGROUP", Suffix: SuffixBrowserGroup}
	pkt, err := buildHostAnnouncement(src, from, to, "MYHOST", "hello")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	if pkt[0] != msgTypeBroadcast {
		t.Fatalf("MSG_TYPE 应为广播(0x12)，实际 %#x", pkt[0])
	}
	if !net.IP(pkt[4:8]).Equal(src) {
		t.Fatalf("SOURCE_IP 应为 %v，实际 %v", src, net.IP(pkt[4:8]))
	}
	if got := binary.BigEndian.Uint16(pkt[8:]); got != PortDatagram {
		t.Fatalf("SOURCE_PORT 应为 138，实际 %d", got)
	}

	// DGM_LENGTH 从 PACKET_OFFSET 字段起算：2 + 34*2 + SMB 体。
	dgmLen := int(binary.BigEndian.Uint16(pkt[10:]))
	if dgmLen != len(pkt)-12 {
		t.Fatalf("DGM_LENGTH 应从 PACKET_OFFSET 起算（= %d），实际 %d", len(pkt)-12, dgmLen)
	}

	// 名字字段位置：14(头) + 2(PACKET_OFFSET) 起两个 34 字节名字。
	body := pkt[12:]
	if got, ok := parseWireName(body[2:]); !ok || !namesEqual(got, from) {
		t.Fatalf("源名字不符：%v", got)
	}
	if got, ok := parseWireName(body[2+wireNameSize:]); !ok || !namesEqual(got, to) {
		t.Fatalf("目的名字不符：%v", got)
	}

	// SMB 体首字节是「主机宣告」命令。
	smb := body[2+wireNameSize*2:]
	if smb[0] != smbHostAnnounce {
		t.Fatalf("SMB 命令应为主机宣告(0x01)，实际 %#x", smb[0])
	}
}

func TestBuildHostAnnouncementNeedsIPv4(t *testing.T) {
	if _, err := buildHostAnnouncement(net.ParseIP("2001:db8::1"),
		Name{Label: "A", Suffix: 0x20}, Name{Label: "B", Suffix: 0x1e}, "A", ""); err == nil {
		t.Fatal("IPv6 不应产生主机宣告（NetBIOS 广播没有 IPv6 概念）")
	}
}

// TestPaddedNameTruncates：SMB 体里的服务器名是定长 16 字节（含 NUL），
// 超长名字必须截断，不能把字段撑爆。
func TestPaddedNameTruncates(t *testing.T) {
	out := paddedName("ABCDEFGHIJKLMNOPQRST")
	if len(out) != 16 {
		t.Fatalf("应定长 16，实际 %d", len(out))
	}
	if string(out[:15]) != "ABCDEFGHIJKLMNO" {
		t.Fatalf("应截断到 15 字符，实际 %q", out[:15])
	}
}
