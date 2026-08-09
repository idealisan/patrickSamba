package wire

import "fmt"

// ---------------------------------------------------------------------------
// 持久句柄 create context
// （MS-SMB2 §2.2.13.2.3 / .4 / .11 / .12 请求侧，§2.2.14.2.3 / .12 响应侧）
//
// 一共四个名字、六种载荷，长度各不相同：
//
//	名字   方向  长度  结构                                       规范
//	DHnQ   →     16   SMB2_CREATE_DURABLE_HANDLE_REQUEST         §2.2.13.2.3
//	DHnQ   ←      8   SMB2_CREATE_DURABLE_HANDLE_RESPONSE        §2.2.14.2.3
//	DHnC   →     16   SMB2_CREATE_DURABLE_HANDLE_RECONNECT       §2.2.13.2.4
//	DH2Q   →     32   SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2      §2.2.13.2.11
//	DH2Q   ←      8   SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2     §2.2.14.2.12
//	DH2C   →     36   SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2    §2.2.13.2.12
//
// 两个坑：
//
//  1. **DHnQ 与 DH2Q 的请求/响应长度不同**（16↔8、32↔8）。名字相同、方向不同、
//     结构不同，判别时不能只看名字。
//  2. **RECONNECT 没有专属的响应 context**：客户端发 DHnC/DH2C 重连成功后，
//     服务端回的是 DHnQ（SMB2_CREATE_DURABLE_HANDLE_RESPONSE）。
//     §2.2.14.2 的响应名字表里根本没有 DHnC/DH2C。
//
// 报文体**全部小端**。
// ---------------------------------------------------------------------------

// 持久句柄各 create context 载荷的固定长度。
const (
	// DurableRequestSize 是 SMB2_CREATE_DURABLE_HANDLE_REQUEST（§2.2.13.2.3）。
	// 内容是 16 字节 DurableRequest，规范要求「MUST NOT be used，服务端忽略」。
	DurableRequestSize = 16
	// DurableResponseSize 是 SMB2_CREATE_DURABLE_HANDLE_RESPONSE（§2.2.14.2.3），
	// 8 字节 Reserved，同样全 0。
	DurableResponseSize = 8
	// DurableReconnectSize 是 SMB2_CREATE_DURABLE_HANDLE_RECONNECT（§2.2.13.2.4），
	// 内容是要重连的 SMB2_FILEID。
	DurableReconnectSize = 16
	// DurableRequestV2Size 是 SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2（§2.2.13.2.11）。
	DurableRequestV2Size = 32
	// DurableResponseV2Size 是 SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2（§2.2.14.2.12）。
	DurableResponseV2Size = 8
	// DurableReconnectV2Size 是 SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2（§2.2.13.2.12）。
	DurableReconnectV2Size = 36
)

// CreateGUIDSize 是 CreateGuid 字段的字节数。
//
// ⚠️ 它在 MS-SMB2 里是**不透明的 16 字节**，服务端只做逐字节相等比较来配对
// 「同一个 create 请求」。**不要**把它当 RFC 4122 UUID 去解析或格式化 ——
// 那会引入 time_low/time_mid/time_hi 三段小端、后 8 字节大端的混合字节序，
// 一旦按字符串往返就会把字节顺序搞乱，重连时配不上。
const CreateGUIDSize = 16

// DurableHandleFlags 是持久句柄 create context 的 Flags（MS-SMB2 §2.2.13.2.11）。
type DurableHandleFlags uint32

// SMB2_DHANDLE_FLAG_PERSISTENT：请求/授予的是**持久**句柄（比 durable 更强，
// 要求 share 具备 CONTINUOUS_AVAILABILITY 能力）。
//
// 注意取值是 0x02 而不是 0x01 —— §2.2.13.2.11 里只定义了这一个位，0x01 未定义。
const DurableHandlePersistent DurableHandleFlags = 0x00000002

// IsPersistent 报告是否请求/授予了持久句柄。
func (f DurableHandleFlags) IsPersistent() bool { return f&DurableHandlePersistent != 0 }

// ---------------------------------------------------------------------------
// v1：DHnQ 请求 / DHnQ 响应 / DHnC 重连
// ---------------------------------------------------------------------------

// ParseDurableRequest 校验 SMB2_CREATE_DURABLE_HANDLE_REQUEST（§2.2.13.2.3）。
//
// 载荷是 16 字节 DurableRequest，规范明确「MUST NOT be used and MUST be
// reserved」，因此**内容一律忽略**，只校验长度。没有可返回的字段，
// context 出现本身就是「客户端请求 durable open」这个信号。
func ParseDurableRequest(data []byte) error {
	if len(data) != DurableRequestSize {
		return fmt.Errorf("%w: DURABLE_HANDLE_REQUEST 载荷 %d 字节，期望 %d",
			ErrMalformed, len(data), DurableRequestSize)
	}
	return nil
}

// EncodeDurableRequest 编码 SMB2_CREATE_DURABLE_HANDLE_REQUEST（全 0）。
func EncodeDurableRequest() []byte { return make([]byte, DurableRequestSize) }

// EncodeDurableResponse 编码 SMB2_CREATE_DURABLE_HANDLE_RESPONSE（§2.2.14.2.3）。
// 8 字节 Reserved，全 0。服务端授予 durable open 时回这个。
func EncodeDurableResponse() []byte { return make([]byte, DurableResponseSize) }

// ParseDurableResponse 校验 SMB2_CREATE_DURABLE_HANDLE_RESPONSE（供测试与客户端使用）。
func ParseDurableResponse(data []byte) error {
	if len(data) != DurableResponseSize {
		return fmt.Errorf("%w: DURABLE_HANDLE_RESPONSE 载荷 %d 字节，期望 %d",
			ErrMalformed, len(data), DurableResponseSize)
	}
	return nil
}

// ParseDurableReconnect 解析 SMB2_CREATE_DURABLE_HANDLE_RECONNECT（§2.2.13.2.4），
// 载荷就是要重连的 SMB2_FILEID。
func ParseDurableReconnect(data []byte) (FileID, error) {
	if len(data) != DurableReconnectSize {
		return FileID{}, fmt.Errorf("%w: DURABLE_HANDLE_RECONNECT 载荷 %d 字节，期望 %d",
			ErrMalformed, len(data), DurableReconnectSize)
	}
	return parseFileID(data), nil
}

// EncodeDurableReconnect 编码 SMB2_CREATE_DURABLE_HANDLE_RECONNECT。
func EncodeDurableReconnect(id FileID) []byte {
	b := make([]byte, DurableReconnectSize)
	id.put(b)
	return b
}

// ---------------------------------------------------------------------------
// v2：DH2Q 请求 / DH2Q 响应 / DH2C 重连
// ---------------------------------------------------------------------------

// DurableRequestV2 是 SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2（§2.2.13.2.11），
// 32 字节：
//
//	 0 Timeout(4)       毫秒；0 表示「用服务端默认值」
//	 4 Flags(4)
//	 8 Reserved(8)      必须为 0
//	16 CreateGuid(16)
type DurableRequestV2 struct {
	// Timeout 是失效转移后服务端为该句柄保留的毫秒数。
	// **0 不是"不超时"，而是"由服务端选默认值"**（§2.2.13.2.11）。
	Timeout    uint32
	Flags      DurableHandleFlags
	CreateGUID [CreateGUIDSize]byte
}

// ParseDurableRequestV2 解析 SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2。
func ParseDurableRequestV2(data []byte) (*DurableRequestV2, error) {
	if len(data) != DurableRequestV2Size {
		return nil, fmt.Errorf("%w: DURABLE_HANDLE_REQUEST_V2 载荷 %d 字节，期望 %d",
			ErrMalformed, len(data), DurableRequestV2Size)
	}
	r := &DurableRequestV2{
		Timeout: le.Uint32(data[0:]),
		Flags:   DurableHandleFlags(le.Uint32(data[4:])),
	}
	// data[8:16] Reserved，规范要求客户端置 0、服务端忽略。
	copy(r.CreateGUID[:], data[16:32])
	return r, nil
}

// Encode 编码 SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2。
func (r *DurableRequestV2) Encode() []byte {
	b := make([]byte, DurableRequestV2Size)
	le.PutUint32(b[0:], r.Timeout)
	le.PutUint32(b[4:], uint32(r.Flags))
	// b[8:16] Reserved 保持 0。
	copy(b[16:32], r.CreateGUID[:])
	return b
}

// DurableResponseV2 是 SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2（§2.2.14.2.12），
// 8 字节：
//
//	0 Timeout(4)   服务端**实际**采用的超时（毫秒）
//	4 Flags(4)
//
// 注意响应里的 Timeout 是服务端最终决定的值，不是回显客户端请求的值：
// 客户端请求 0 时服务端要在这里填自己的默认值。
type DurableResponseV2 struct {
	Timeout uint32
	Flags   DurableHandleFlags
}

// Encode 编码 SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2。
func (r *DurableResponseV2) Encode() []byte {
	b := make([]byte, DurableResponseV2Size)
	le.PutUint32(b[0:], r.Timeout)
	le.PutUint32(b[4:], uint32(r.Flags))
	return b
}

// ParseDurableResponseV2 解析 SMB2_CREATE_DURABLE_HANDLE_RESPONSE_V2
// （供测试与 Go 客户端使用）。
func ParseDurableResponseV2(data []byte) (*DurableResponseV2, error) {
	if len(data) != DurableResponseV2Size {
		return nil, fmt.Errorf("%w: DURABLE_HANDLE_RESPONSE_V2 载荷 %d 字节，期望 %d",
			ErrMalformed, len(data), DurableResponseV2Size)
	}
	return &DurableResponseV2{
		Timeout: le.Uint32(data[0:]),
		Flags:   DurableHandleFlags(le.Uint32(data[4:])),
	}, nil
}

// DurableReconnectV2 是 SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2（§2.2.13.2.12），
// 36 字节：
//
//	 0 FileId(16)
//	16 CreateGuid(16)
//	32 Flags(4)
//
// 字段顺序与 REQUEST_V2 不同（那边 Flags 在前、CreateGuid 在后），
// 这里是 FileId → CreateGuid → Flags，别照抄。
type DurableReconnectV2 struct {
	FileID     FileID
	CreateGUID [CreateGUIDSize]byte
	Flags      DurableHandleFlags
}

// ParseDurableReconnectV2 解析 SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2。
func ParseDurableReconnectV2(data []byte) (*DurableReconnectV2, error) {
	if len(data) != DurableReconnectV2Size {
		return nil, fmt.Errorf("%w: DURABLE_HANDLE_RECONNECT_V2 载荷 %d 字节，期望 %d",
			ErrMalformed, len(data), DurableReconnectV2Size)
	}
	r := &DurableReconnectV2{
		FileID: parseFileID(data),
		Flags:  DurableHandleFlags(le.Uint32(data[32:])),
	}
	copy(r.CreateGUID[:], data[16:32])
	return r, nil
}

// Encode 编码 SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2。
func (r *DurableReconnectV2) Encode() []byte {
	b := make([]byte, DurableReconnectV2Size)
	r.FileID.put(b)
	copy(b[16:32], r.CreateGUID[:])
	le.PutUint32(b[32:], uint32(r.Flags))
	return b
}

// ---------------------------------------------------------------------------
// 链上查找
// ---------------------------------------------------------------------------

// DurableIntent 概括客户端在一次 CREATE 里表达的持久句柄意图，
// 供 server 层的 create handler 一次性判别。
type DurableIntent struct {
	// RequestV1 为 true 表示出现了 DHnQ（§2.2.13.2.3）。
	RequestV1 bool
	// RequestV2 非 nil 表示出现了 DH2Q（§2.2.13.2.11）。
	RequestV2 *DurableRequestV2
	// ReconnectV1 非 nil 表示出现了 DHnC（§2.2.13.2.4），值是要重连的 FileId。
	ReconnectV1 *FileID
	// ReconnectV2 非 nil 表示出现了 DH2C（§2.2.13.2.12）。
	ReconnectV2 *DurableReconnectV2
}

// Any 报告客户端是否表达了任何持久句柄意图。
func (d *DurableIntent) Any() bool {
	return d.RequestV1 || d.RequestV2 != nil || d.ReconnectV1 != nil || d.ReconnectV2 != nil
}

// IsReconnect 报告这是一次重连（DHnC 或 DH2C）。
func (d *DurableIntent) IsReconnect() bool {
	return d.ReconnectV1 != nil || d.ReconnectV2 != nil
}

// FindDurableIntent 扫描 create context 链，解析出全部持久句柄相关 context。
//
// 不做**语义**校验（例如同时出现 DHnQ 与 DH2C 属于非法组合、
// 重连必须落在同一个 share 上等），那些是 §3.3.5.9.7/§3.3.5.9.12 的处理规则，
// 属于 server 层职责。本函数只负责「字节 → 结构体」。
func FindDurableIntent(ctxs []CreateContext) (*DurableIntent, error) {
	var d DurableIntent
	if data, ok := FindCreateContext(ctxs, CreateContextDHnQ); ok {
		if err := ParseDurableRequest(data); err != nil {
			return nil, err
		}
		d.RequestV1 = true
	}
	if data, ok := FindCreateContext(ctxs, CreateContextDHnC); ok {
		id, err := ParseDurableReconnect(data)
		if err != nil {
			return nil, err
		}
		d.ReconnectV1 = &id
	}
	if data, ok := FindCreateContext(ctxs, CreateContextDH2Q); ok {
		v, err := ParseDurableRequestV2(data)
		if err != nil {
			return nil, err
		}
		d.RequestV2 = v
	}
	if data, ok := FindCreateContext(ctxs, CreateContextDH2C); ok {
		v, err := ParseDurableReconnectV2(data)
		if err != nil {
			return nil, err
		}
		d.ReconnectV2 = v
	}
	return &d, nil
}
