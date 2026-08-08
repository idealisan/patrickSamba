package mdns

import (
	"bytes"
	"net"
	"testing"
)

// mDNS 查询 "_services._dns-sd._udp.local. PTR IN"（RFC 6763 §9 的服务类型枚举）。
//
// 这是一段可以逐字节手工核对的固定向量：头部 12 字节全 0 只有 QDCOUNT=1，
// 随后是各 label 的 长度+ASCII，最后 QTYPE=0x000c(PTR) QCLASS=0x0001(IN)。
var goldenServiceEnumQuery = []byte{
	0x00, 0x00, // ID = 0（RFC 6762 §18.1：查询方 ID 应为 0）
	0x00, 0x00, // Flags = 0（QR=0 查询，OPCODE=0）
	0x00, 0x01, // QDCOUNT = 1
	0x00, 0x00, // ANCOUNT = 0
	0x00, 0x00, // NSCOUNT = 0
	0x00, 0x00, // ARCOUNT = 0
	0x09, '_', 's', 'e', 'r', 'v', 'i', 'c', 'e', 's',
	0x07, '_', 'd', 'n', 's', '-', 's', 'd',
	0x04, '_', 'u', 'd', 'p',
	0x05, 'l', 'o', 'c', 'a', 'l',
	0x00,
	0x00, 0x0c, // QTYPE = PTR
	0x00, 0x01, // QCLASS = IN
}

func TestPackServiceEnumQuery(t *testing.T) {
	m := &Message{
		Questions: []Question{{
			Name:  "_services._dns-sd._udp.local.",
			Type:  TypePTR,
			Class: ClassIN,
		}},
	}
	got, err := m.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if !bytes.Equal(got, goldenServiceEnumQuery) {
		t.Fatalf("编码不一致\n got: % x\nwant: % x", got, goldenServiceEnumQuery)
	}
}

func TestUnpackServiceEnumQuery(t *testing.T) {
	m, err := Unpack(goldenServiceEnumQuery)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if m.Flags.IsResponse() {
		t.Error("QR 位应为 0")
	}
	if len(m.Questions) != 1 {
		t.Fatalf("问题数 = %d, want 1", len(m.Questions))
	}
	q := m.Questions[0]
	if q.Name != "_services._dns-sd._udp.local." {
		t.Errorf("Name = %q", q.Name)
	}
	if q.Type != TypePTR || q.Class != ClassIN || q.Unicast {
		t.Errorf("Type/Class/QU = %v/%v/%v", q.Type, q.Class, q.Unicast)
	}
}

// QU 位（RFC 6762 §5.4）：qclass 最高位置 1 表示请求单播回应。
func TestQuestionUnicastBit(t *testing.T) {
	m := &Message{Questions: []Question{{
		Name: "x.local.", Type: TypeA, Class: ClassIN, Unicast: true,
	}}}
	b, err := m.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	// 最后两字节是 qclass。
	if got := b[len(b)-2:]; got[0] != 0x80 || got[1] != 0x01 {
		t.Fatalf("qclass = % x, want 80 01", got)
	}
	back, err := Unpack(b)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if !back.Questions[0].Unicast {
		t.Error("QU 位未被解析出来")
	}
	if back.Questions[0].Class != ClassIN {
		t.Errorf("Class = %v，QU 位应当被剥掉", back.Questions[0].Class)
	}
}

// cache-flush 位（RFC 6762 §10.2）：应答记录 rrclass 最高位置 1。
func TestRecordCacheFlushBit(t *testing.T) {
	m := &Message{
		Flags: FlagResponse | FlagAuthoritative,
		Answers: []Record{{
			Name:       "host.local.",
			Class:      ClassIN,
			CacheFlush: true,
			TTL:        120,
			Data:       A{IP: net.IPv4(192, 168, 1, 5)},
		}},
	}
	b, err := m.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	want := []byte{
		0x00, 0x00, // ID
		0x84, 0x00, // Flags: QR=1 AA=1
		0x00, 0x00, // QDCOUNT
		0x00, 0x01, // ANCOUNT
		0x00, 0x00, // NSCOUNT
		0x00, 0x00, // ARCOUNT
		0x04, 'h', 'o', 's', 't',
		0x05, 'l', 'o', 'c', 'a', 'l',
		0x00,
		0x00, 0x01, // TYPE = A
		0x80, 0x01, // CLASS = IN | cache-flush
		0x00, 0x00, 0x00, 0x78, // TTL = 120
		0x00, 0x04, // RDLENGTH
		192, 168, 1, 5,
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("编码不一致\n got: % x\nwant: % x", b, want)
	}

	back, err := Unpack(b)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	r := back.Answers[0]
	if !r.CacheFlush || r.Class != ClassIN || r.TTL != 120 {
		t.Errorf("cacheFlush/class/ttl = %v/%v/%d", r.CacheFlush, r.Class, r.TTL)
	}
	if a, ok := r.Data.(A); !ok || !a.IP.Equal(net.IPv4(192, 168, 1, 5)) {
		t.Errorf("rdata = %v", r.Data)
	}
}

func TestSRVAndTXTRoundTrip(t *testing.T) {
	m := &Message{
		Flags: FlagResponse | FlagAuthoritative,
		Answers: []Record{
			{
				Name: "MYBOX._smb._tcp.local.", Class: ClassIN, CacheFlush: true, TTL: 120,
				Data: SRV{Priority: 0, Weight: 0, Port: 445, Target: "MYBOX.local."},
			},
			{
				Name: "MYBOX._smb._tcp.local.", Class: ClassIN, CacheFlush: true, TTL: 4500,
				Data: TXT{Strings: []string{"model=MacSamba", "sys=waMa=0"}},
			},
		},
	}
	b, err := m.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	back, err := Unpack(b)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if len(back.Answers) != 2 {
		t.Fatalf("答案数 = %d", len(back.Answers))
	}
	srv, ok := back.Answers[0].Data.(SRV)
	if !ok {
		t.Fatalf("第一条不是 SRV: %T", back.Answers[0].Data)
	}
	if srv.Port != 445 || srv.Target != "MYBOX.local." {
		t.Errorf("SRV = %+v", srv)
	}
	txt, ok := back.Answers[1].Data.(TXT)
	if !ok {
		t.Fatalf("第二条不是 TXT: %T", back.Answers[1].Data)
	}
	if len(txt.Strings) != 2 || txt.Strings[0] != "model=MacSamba" || txt.Strings[1] != "sys=waMa=0" {
		t.Errorf("TXT = %v", txt.Strings)
	}
}

// RFC 6763 §6.1：空 TXT 必须编码成一个长度为 0 的 character-string。
func TestEmptyTXTIsSingleZeroByte(t *testing.T) {
	b, err := TXT{}.pack(nil)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if len(b) != 1 || b[0] != 0 {
		t.Fatalf("空 TXT 编码 = % x, want 00", b)
	}
}

func TestNSECRoundTrip(t *testing.T) {
	m := &Message{
		Flags: FlagResponse | FlagAuthoritative,
		Answers: []Record{{
			Name: "MYBOX.local.", Class: ClassIN, CacheFlush: true, TTL: 120,
			Data: NSEC{NextDomain: "MYBOX.local.", Types: []Type{TypeA, TypeAAAA}},
		}},
	}
	b, err := m.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	back, err := Unpack(b)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	n, ok := back.Answers[0].Data.(NSEC)
	if !ok {
		t.Fatalf("不是 NSEC: %T", back.Answers[0].Data)
	}
	if len(n.Types) != 2 || n.Types[0] != TypeA || n.Types[1] != TypeAAAA {
		t.Fatalf("types = %v", n.Types)
	}
	// A=1、AAAA=28 都落在 window 0：window(1) + len(1) + 4 字节位图。
	// bit 1 -> 0x40（第 0 字节），bit 28 -> 第 3 字节的 0x08。
	wantBitmap := []byte{0x00, 0x04, 0x40, 0x00, 0x00, 0x08}
	if !bytes.Contains(b, wantBitmap) {
		t.Fatalf("位图不符，报文 = % x", b)
	}
}

// 压缩指针（RFC 1035 §4.1.4）：编码端不压缩，但解码端必须支持。
func TestUnpackCompressionPointer(t *testing.T) {
	// 手工构造：一条 PTR 记录，其 target 用指针指回问题里的名字。
	msg := []byte{
		0x00, 0x00, 0x84, 0x00,
		0x00, 0x01, // QDCOUNT = 1
		0x00, 0x01, // ANCOUNT = 1
		0x00, 0x00,
		0x00, 0x00,
		// 偏移 12: "_smb._tcp.local."
		0x04, '_', 's', 'm', 'b',
		0x04, '_', 't', 'c', 'p',
		0x05, 'l', 'o', 'c', 'a', 'l',
		0x00,
		0x00, 0x0c, 0x00, 0x01, // PTR IN
		// 答案：名字用指针指向偏移 12
		0xc0, 0x0c,
		0x00, 0x0c, 0x00, 0x01, // PTR IN
		0x00, 0x00, 0x11, 0x94, // TTL = 4500
		0x00, 0x08, // RDLENGTH = 8
		0x05, 'M', 'Y', 'B', 'O', 'X', 0xc0, 0x0c, // "MYBOX" + 指针
	}
	m, err := Unpack(msg)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if m.Questions[0].Name != "_smb._tcp.local." {
		t.Errorf("问题名 = %q", m.Questions[0].Name)
	}
	if m.Answers[0].Name != "_smb._tcp.local." {
		t.Errorf("答案名 = %q", m.Answers[0].Name)
	}
	ptr, ok := m.Answers[0].Data.(PTR)
	if !ok {
		t.Fatalf("不是 PTR: %T", m.Answers[0].Data)
	}
	if ptr.Target != "MYBOX._smb._tcp.local." {
		t.Errorf("target = %q", ptr.Target)
	}
}

// 恶意输入：指针环路、越界、非法 label 类型都必须返回错误而不是 panic 或死循环。
func TestUnpackNameHostileInput(t *testing.T) {
	cases := []struct {
		name string
		msg  []byte
		off  int
	}{
		{"指向自身的指针", []byte{0xc0, 0x00}, 0},
		{"向前指的指针（会成环）", []byte{0x00, 0x00, 0xc0, 0x04, 0xc0, 0x02}, 2},
		{"指针指向报文之外", []byte{0x00, 0x00, 0xc0, 0x20}, 2},
		{"label 长度越界", []byte{0x10, 'a', 'b'}, 0},
		{"缺少结尾根标签", []byte{0x01, 'a'}, 0},
		{"指针字节不完整", []byte{0xc0}, 0},
		{"非法 label 类型 0x40", []byte{0x40, 0x00}, 0},
		{"非法 label 类型 0x80", []byte{0x80, 0x00}, 0},
		{"空报文", []byte{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := unpackName(tc.msg, tc.off); err == nil {
				t.Fatal("期望返回错误，实际成功了")
			}
		})
	}
}

// 超过 255 字节的域名必须被拒绝。
func TestNameTooLong(t *testing.T) {
	// 每个 label 63 字节 + 1 字节长度 = 64；5 个就是 320 > 255。
	var msg []byte
	for i := 0; i < 5; i++ {
		msg = append(msg, 63)
		msg = append(msg, bytes.Repeat([]byte{'a'}, 63)...)
	}
	msg = append(msg, 0)
	if _, _, err := unpackName(msg, 0); err != ErrNameTooLong {
		t.Fatalf("err = %v, want ErrNameTooLong", err)
	}

	long := ""
	for i := 0; i < 5; i++ {
		long += string(bytes.Repeat([]byte{'a'}, 63)) + "."
	}
	if _, err := packName(nil, long); err == nil {
		t.Fatal("packName 应拒绝超长域名")
	}
}

// 伪造的巨大计数字段不能导致大块分配或越界。
func TestUnpackBogusCounts(t *testing.T) {
	msg := []byte{
		0x00, 0x00, 0x00, 0x00,
		0xff, 0xff, // QDCOUNT = 65535
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}
	if _, err := Unpack(msg); err == nil {
		t.Fatal("期望返回错误")
	}
}

// 短于报文头的输入。
func TestUnpackShortMessage(t *testing.T) {
	for n := 0; n < headerLen; n++ {
		if _, err := Unpack(make([]byte, n)); err != ErrTruncated {
			t.Fatalf("len=%d err=%v, want ErrTruncated", n, err)
		}
	}
}

// 域名转义（RFC 1035 §5.1）：DNS-SD 实例名允许含点与空格（RFC 6763 §4.1.1）。
func TestNameEscaping(t *testing.T) {
	name := EscapeLabel("My.Box #1") + "._smb._tcp.local."
	b, err := packName(nil, name)
	if err != nil {
		t.Fatalf("packName: %v", err)
	}
	// 第一个 label 应当是原始的 9 字节 "My.Box #1"。
	if b[0] != 9 || string(b[1:10]) != "My.Box #1" {
		t.Fatalf("首个 label = %q", b[1:1+int(b[0])])
	}
	back, _, err := unpackName(b, 0)
	if err != nil {
		t.Fatalf("unpackName: %v", err)
	}
	if back != name {
		t.Fatalf("round-trip 不一致: %q != %q", back, name)
	}
}

func TestEqualName(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"MYBOX.local.", "mybox.local.", true},
		{"MYBOX.local", "mybox.local.", true},
		{"_smb._tcp.local.", "_SMB._TCP.local.", true},
		{"a.local.", "b.local.", false},
	}
	for _, tc := range cases {
		if got := EqualName(tc.a, tc.b); got != tc.want {
			t.Errorf("EqualName(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRecordMatches(t *testing.T) {
	r := Record{Name: "MYBOX.local.", Class: ClassIN, Data: A{IP: net.IPv4(10, 0, 0, 1)}}
	cases := []struct {
		q    Question
		want bool
	}{
		{Question{Name: "mybox.local.", Type: TypeA, Class: ClassIN}, true},
		{Question{Name: "mybox.local.", Type: TypeANY, Class: ClassIN}, true},
		{Question{Name: "mybox.local.", Type: TypeA, Class: ClassANY}, true},
		{Question{Name: "mybox.local.", Type: TypeAAAA, Class: ClassIN}, false},
		{Question{Name: "other.local.", Type: TypeA, Class: ClassIN}, false},
	}
	for _, tc := range cases {
		if got := r.Matches(tc.q); got != tc.want {
			t.Errorf("Matches(%v) = %v, want %v", tc.q, got, tc.want)
		}
	}
}

func TestRecordEqualIgnoresTTL(t *testing.T) {
	a := Record{Name: "x.local.", Class: ClassIN, TTL: 120, Data: SRV{Port: 445, Target: "h.local."}}
	b := Record{Name: "X.LOCAL.", Class: ClassIN, TTL: 4500, CacheFlush: true, Data: SRV{Port: 445, Target: "H.local."}}
	if !a.Equal(b) {
		t.Error("同内容不同 TTL/大小写的记录应当相等")
	}
	c := b
	c.Data = SRV{Port: 446, Target: "h.local."}
	if a.Equal(c) {
		t.Error("端口不同的 SRV 不应相等")
	}
}
