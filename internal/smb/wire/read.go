package wire

import (
	"fmt"
	"math"
	"slices"
)

// ---------------------------------------------------------------------------
// READ Request（MS-SMB2 §2.2.19）
//
//	 0 StructureSize(2) = 49
//	 2 Padding(1)                 客户端期望的数据偏移，服务端可忽略
//	 3 Flags(1)                   3.0.2+：SMB2_READFLAG_READ_UNBUFFERED
//	 4 Length(4)                  要读的字节数
//	 8 Offset(8)                  文件内偏移
//	16 FileId(16)
//	32 MinimumCount(4)            少于该值则回 STATUS_END_OF_FILE
//	36 Channel(4)                 非 RDMA 一律 0
//	40 RemainingBytes(4)
//	44 ReadChannelInfoOffset(2)
//	46 ReadChannelInfoLength(2)
//	48 Buffer                     至少 1 字节（StructureSize 49 = 48 + 1）
// ---------------------------------------------------------------------------

const (
	readRequestStructureSize = 49
	readRequestFixed         = 48

	readResponseStructureSize = 17
	readResponseFixed         = 16
)

// READ Request 的 Flags 位（MS-SMB2 §2.2.19）。
const (
	ReadFlagUnbuffered    uint8 = 0x01 // SMB2_READFLAG_READ_UNBUFFERED（3.0.2+）
	ReadFlagRequestCompr  uint8 = 0x02 // SMB2_READFLAG_REQUEST_COMPRESSED（3.1.1）
	ReadResponseFlagCompr uint8 = 0x01 // SMB2_READFLAG_RESPONSE_NONE 之外的压缩标志（3.1.1）
)

// ReadRequest 是 SMB2 READ Request（MS-SMB2 §2.2.19）。
type ReadRequest struct {
	Padding        uint8
	Flags          uint8
	Length         uint32
	Offset         uint64
	FileID         FileID
	MinimumCount   uint32
	Channel        uint32
	RemainingBytes uint32
	// ReadChannelInfo 只在 RDMA 通道下有意义，本项目不支持 RDMA，仅保留原样。
	ReadChannelInfo []byte
}

// ParseReadRequest 解析 READ Request。b 是完整消息（含 64 字节头）。
func ParseReadRequest(b []byte) (*ReadRequest, error) {
	body, err := msgBody(b, readRequestFixed, "READ Request")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, readRequestStructureSize); err != nil {
		return nil, fmt.Errorf("READ Request: %w", err)
	}
	r := &ReadRequest{
		Padding:        body[2],
		Flags:          body[3],
		Length:         le.Uint32(body[4:]),
		Offset:         le.Uint64(body[8:]),
		FileID:         parseFileID(body[16:]),
		MinimumCount:   le.Uint32(body[32:]),
		Channel:        le.Uint32(body[36:]),
		RemainingBytes: le.Uint32(body[40:]),
	}
	off := uint64(le.Uint16(body[44:]))
	length := uint64(le.Uint16(body[46:]))
	r.ReadChannelInfo, err = sliceAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("READ Request ReadChannelInfo: %w", err)
	}
	return r, nil
}

// Append 把 READ Request 报文体追加到 dst（供测试与 Go 客户端使用）。
func (r *ReadRequest) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, readRequestFixed)
	le.PutUint16(f[0:], readRequestStructureSize)
	f[2] = r.Padding
	f[3] = r.Flags
	le.PutUint32(f[4:], r.Length)
	le.PutUint64(f[8:], r.Offset)
	r.FileID.put(f[16:])
	le.PutUint32(f[32:], r.MinimumCount)
	le.PutUint32(f[36:], r.Channel)
	le.PutUint32(f[40:], r.RemainingBytes)

	if len(r.ReadChannelInfo) > 0 {
		n, err := u16(len(r.ReadChannelInfo), "ReadChannelInfo")
		if err != nil {
			return nil, err
		}
		le.PutUint16(f[44:], HeaderSize+readRequestFixed)
		le.PutUint16(f[46:], n)
		return append(dst, r.ReadChannelInfo...), nil
	}
	// StructureSize 49 = 固定 48 + 1 字节可变部分占位。
	dst, _ = grow(dst, 1)
	return dst, nil
}

// ---------------------------------------------------------------------------
// READ Response（MS-SMB2 §2.2.20）
//
//	 0 StructureSize(2) = 17
//	 2 DataOffset(1)     相对 SMB2 头起点（典型值 0x50 = 64+16）
//	 3 Reserved(1)
//	 4 DataLength(4)
//	 8 DataRemaining(4)
//	12 Reserved2(4)      3.1.1：Flags
//	16 Buffer
// ---------------------------------------------------------------------------

// ReadResponse 是 SMB2 READ Response（MS-SMB2 §2.2.20）。
type ReadResponse struct {
	DataRemaining uint32
	Flags         uint32
	Data          []byte
}

// Append 把 READ Response 报文体追加到 dst。
//
// 大载荷（普通文件 READ）建议改用 ReserveReadResponse + Commit：
// 那条路径让文件内容直接落进最终缓冲，省掉本函数 unavoidable 的
// 整载荷二次拷贝（v0.5 读路径去双拷贝）。
func (r *ReadResponse) Append(dst []byte) ([]byte, error) {
	dst, f := grow(dst, readResponseFixed)
	le.PutUint16(f[0:], readResponseStructureSize)
	// DataOffset 是 1 字节，因此数据必须紧跟固定部分：64 + 16 = 80 = 0x50。
	f[2] = HeaderSize + readResponseFixed
	n, err := u32(len(r.Data), "READ Data")
	if err != nil {
		return nil, err
	}
	le.PutUint32(f[4:], n)
	le.PutUint32(f[8:], r.DataRemaining)
	le.PutUint32(f[12:], r.Flags)
	if n == 0 {
		// 1 字节可变部分占位（StructureSize 17 = 16 + 1）。
		dst, _ = grow(dst, 1)
		return dst, nil
	}
	return append(dst, r.Data...), nil
}

// ParseReadResponse 解析 READ Response（供测试与 Go 客户端使用）。
func ParseReadResponse(b []byte) (*ReadResponse, error) {
	body, err := msgBody(b, readResponseFixed, "READ Response")
	if err != nil {
		return nil, err
	}
	if err := checkStructureSize(body, readResponseStructureSize); err != nil {
		return nil, fmt.Errorf("READ Response: %w", err)
	}
	r := &ReadResponse{
		DataRemaining: le.Uint32(body[8:]),
		Flags:         le.Uint32(body[12:]),
	}
	off := uint64(body[2])
	length := uint64(le.Uint32(body[4:]))
	r.Data, err = sliceAt(b, off, length)
	if err != nil {
		return nil, fmt.Errorf("READ Response Data: %w", err)
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 「先占位、后填数」组装路径（v0.5 读路径去双拷贝）
//
// 旧路径 READ 响应要过两次整载荷内存：handler 先把文件读进临时缓冲，
// Append 再把它拷进响应缓冲。ReserveReadResponse 把响应缓冲里的数据
// 窗口直接交给调用方，文件内容一步写进最终位置 —— 整条路径只剩
// 响应缓冲自身这一次分配。本类型不持有任何协议状态，只是一段缓冲的
// 字段布局视图，wire 的无状态纯编解码定位（AGENTS.md §5 P1）不变。
// ---------------------------------------------------------------------------

// ReservedRead 是 ReserveReadResponse 预留出的一条 READ Response 组装现场。
//
// 典型用法（且仅限此序）：
//
//	out, rsv, err := wire.ReserveReadResponse(ctx.Out, n)
//	k, err := file.ReadAt(rsv.Data, off)   // 数据直接落进最终缓冲
//	out, err = rsv.Commit(out, k)          // 回填 DataLength 并截掉多余窗口
//
// 以值类型返回、方法用值接收者：组装现场留在调用方栈上，
// 不给热路径添一次堆分配。
type ReservedRead struct {
	// Data 是预留的数据窗口（长度等于申请的字节数，内容已清零）。
	// 调用方把文件内容直接读进该窗口。
	Data []byte

	// fixedOff 是 16 字节固定部分在底层缓冲中的绝对下标；
	// fullLen 是 Commit 时 out 应当具有的总长度（前缀 + 预留区），
	// 用于钉住「期间没有追加字节」。两者都由 ReserveReadResponse 记录。
	fixedOff int
	fullLen  int
}

// ReserveReadResponse 在 dst 尾部预留一条最多容纳 maxData 字节数据的
// READ Response（MS-SMB2 §2.2.20）：一次扩容同时写下 16 字节固定部分
// （DataLength 暂记为 0，Commit 回填）与数据窗口。
//
// 与 Append 的差别：Append 要求数据先在别处就位再整体拷入 dst；
// 本函数让调用方把数据**直接产生**在 dst 里 —— 单次分配、零二次拷贝。
// 代价是调用方必须随后调用 (*ReservedRead).Commit 定稿；中途放弃时
// 直接返回错误即可（调用方会把缓冲回滚到响应头）。
//
// DataRemaining/Flags 非 0 的场景（命名管道 READ）不适用本 API，
// 继续用 Append。maxData 必须在 [0, MaxUint32]。
func ReserveReadResponse(dst []byte, maxData int) ([]byte, ReservedRead, error) {
	if maxData < 0 || uint64(maxData) > math.MaxUint32 {
		return nil, ReservedRead{}, fmt.Errorf("%w: READ 数据窗口 %d 字节非法", ErrMalformed, maxData)
	}
	// 窗口之外至少多留 1 字节：n==0 定稿时按 StructureSize 17 = 16 + 1
	// 要有 1 字节占位（与 Append 一致），此时它不属于数据窗口。
	need := readResponseFixed + maxData
	if maxData == 0 {
		need++
	}
	// 用 slices.Grow 而不是 grow()：后者是 make+append 两段式，
	// 大载荷会变成两次分配 —— 与「单次分配」的目标正好相反。
	base := len(dst)
	dst = slices.Grow(dst, need)[:base+need]
	f := dst[base:]
	clear(f)
	le.PutUint16(f[0:], readResponseStructureSize)
	// DataOffset：1 字节，因此数据必须紧跟固定部分（64+16=80=0x50），同 Append。
	f[2] = HeaderSize + readResponseFixed
	return dst, ReservedRead{
		Data:     f[readResponseFixed : readResponseFixed+maxData],
		fixedOff: base,
		fullLen:  len(dst),
	}, nil
}

// Commit 在数据窗口填充完毕后定稿：把实际字节数 n 写进 DataLength
// （体内偏移 4，小端），并把 out 截断到响应的真实末尾 —— 多余的预留
// 窗口丢弃；n == 0 时按 StructureSize 17 = 16 + 1 保留 1 字节占位
// （窗口首字节保持 Reserve 时的清零状态，与 Append 的输出逐字节一致）。
//
// out 必须仍是 ReserveReadResponse 返回的那个切片（末尾恰好在数据窗口
// 结尾处），两次调用之间不得向它追加或截除任何字节 —— 本方法按长度
// 精确校验这一点，违反即报错而不是产出错位报文。
func (r ReservedRead) Commit(out []byte, n int) ([]byte, error) {
	switch {
	case n < 0 || n > len(r.Data):
		return nil, fmt.Errorf("%w: READ 实际数据 %d 超出预留窗口 %d", ErrMalformed, n, len(r.Data))
	case len(out) != r.fullLen:
		return nil, fmt.Errorf("%w: 缓冲长度 %d ≠ 预留时的 %d（Reserve 与 Commit 之间不得改动缓冲）",
			ErrMalformed, len(out), r.fullLen)
	}
	le.PutUint32(out[r.fixedOff:][4:], uint32(n))
	keep := n
	if n == 0 {
		keep = 1
	}
	return out[:r.fixedOff+readResponseFixed+keep], nil
}
