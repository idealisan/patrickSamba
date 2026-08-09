package dcerpc

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// ShareEntry 是 srvsvc 暴露给客户端的单个共享描述。
// 字段语义见 MS-SRVS §2.2.2.1（SHARE_INFO_1）。
type ShareEntry struct {
	Name   string // shi1_netname（不含结尾 NUL）
	Type   uint32 // shi1_type：STYPE_DISKTREE=0 / STYPE_IPC=3，隐藏位 STYPE_SPECIAL=0x80000000
	Remark string // shi1_remark
}

// ShareLister 是获取共享列表的注入点。
//
// 设计约束（来自任务要求）：dcerpc 包**不 import internal/config**，
// 以避免反向依赖。由 server/cmd 层把 config 适配成这个极简接口传进来。
type ShareLister interface {
	// Shares 返回当前要对外宣告的全部共享（含 IPC$ 等）。
	Shares() []ShareEntry
}

// Handler 处理某一命名管道（如 srvsvc）上的全部 DCERPC 流量。
// 输入是客户端写进管道的一个完整 PDU 字节，输出是服务端要回给客户端的 PDU 字节。
// 实现方负责 PDU 解析、接口分发与响应封装。
type Handler interface {
	Handle(input []byte) (output []byte, err error)
}

// Pipe 模拟一个 SMB 命名管道端点。server 层负责把 SMB2 WRITE/READ 与
// FSCTL_PIPE_TRANSCEIVE(0x0011C017) 都接到这一接口上。
//
// 两种用法：
//   - **推荐**：直接用 Transact 做一次请求/响应交换（IOCTL 与 WRITE+READ 都适用），
//     截断产生的剩余字节由 Pipe 自己保管，后续 Transact/Read 继续取。
//   - 流式：Write 塞入请求、Read 分批取走响应。Read 在缓冲取空后返回 io.EOF，
//     因此可以安全地用 io.ReadAll。
type Pipe interface {
	// Transact 写入 in（可为 nil，表示只取剩余响应），返回至多 maxOut 字节的响应。
	// maxOut <= 0 表示不限。响应被截断时返回 ErrMoreData，**剩余部分不会丢失**。
	Transact(in []byte, maxOut int) ([]byte, error)

	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	Close() error
}

// ErrMoreData 表示响应超出了调用方给的 maxOut，返回的是截断后的前缀，
// 剩余字节仍留在管道里，可由后续 Transact/Read 取走。
// SMB 层应把它映射为 STATUS_BUFFER_OVERFLOW（MS-SMB2 §3.3.5.10）。
var ErrMoreData = errors.New("dcerpc: 管道响应超出输出缓冲")

// NCA 状态码（MS-RPCE §3.1.1.1 / C706 §13.2.4.1），用于 fault PDU。
const (
	NCAStatusOpRangeError  uint32 = 0x1c010002 // nca_op_rng_error：不支持的 opnum
	NCAStatusProtocolError uint32 = 0x1c010003
	NCAStatusFault         uint32 = 0x1c000001 // nca_fault
)

type pipe struct {
	mu      sync.Mutex
	name    string
	handler Handler
	inBuf   []byte // 已收到的输入，待按 frag_length 切分 PDU
	outBuf  []byte // 已生成的响应，待 Read 取走
	closed  bool
}

// OpenPipe 为命名管道 name 创建一个 Pipe，handler 负责解释该管道的 DCERPC 流量。
// 例：OpenPipe("srvsvc", srvsvc.NewHandler(lister))。
func OpenPipe(name string, h Handler) (Pipe, error) {
	if h == nil {
		return nil, errors.New("dcerpc: handler 不能为 nil")
	}
	return &pipe{name: name, handler: h}, nil
}

// Write 接收客户端写进管道的数据，按 PDU frag_length 切出完整 PDU 并交给 handler，
// 把响应缓存到 outBuf 供 Read 取走。
func (p *pipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writeLocked(b)
}

func (p *pipe) writeLocked(b []byte) (int, error) {
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	p.inBuf = append(p.inBuf, b...)

	for len(p.inBuf) >= headerSize {
		// frag_length 采用公共头字节序（packed_drep[0] 高 4 位决定）。
		var order binary.ByteOrder = binary.LittleEndian
		if len(p.inBuf) > 4 && (p.inBuf[4]>>4)&0x0F == 0 {
			order = binary.BigEndian
		}
		if len(p.inBuf) < 10 {
			break
		}
		fl := int(order.Uint16(p.inBuf[8:10]))
		if fl < headerSize || fl > len(p.inBuf) {
			break // 该 PDU 还没收齐，等下一次 Write
		}
		pdu := make([]byte, fl)
		copy(pdu, p.inBuf[:fl])
		out, err := p.handler.Handle(pdu)
		if err != nil {
			// 兜底：解析彻底失败时回一个 fault（call_id 取 0，不致命）。
			out = MarshalFault(0, NCAStatusFault)
		}
		if len(out) > 0 {
			p.outBuf = append(p.outBuf, out...)
		}
		p.inBuf = p.inBuf[fl:]
	}
	return len(b), nil
}

// Transact 做一次请求/响应交换：写入 in，取回至多 maxOut 字节的响应。
//
// 这是 server 层该用的入口。与 Write+io.ReadAll 相比，它保证
//   - 截断时剩余字节留在管道内（后续 Transact(nil, n) 或 Read 继续取），
//   - 不会因为「暂时没数据」而空转。
func (p *pipe) Transact(in []byte, maxOut int) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(in) > 0 {
		if _, err := p.writeLocked(in); err != nil {
			return nil, err
		}
	} else if p.closed {
		return nil, io.ErrClosedPipe
	}

	n := len(p.outBuf)
	if maxOut > 0 && n > maxOut {
		n = maxOut
	}
	out := make([]byte, n)
	copy(out, p.outBuf[:n])
	p.outBuf = p.outBuf[n:]

	if len(p.outBuf) > 0 {
		return out, ErrMoreData
	}
	return out, nil
}

// Read 从 outBuf 取走响应数据。缓冲取空后返回 io.EOF ——
// DCERPC over SMB 是消息模式，一次 Transact 的响应取完就是结束；
// 返回 (0, nil) 会让 io.ReadAll 这类调用方**死循环**。
func (p *pipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.outBuf) == 0 {
		return 0, io.EOF
	}
	n := copy(b, p.outBuf)
	p.outBuf = p.outBuf[n:]
	return n, nil
}

func (p *pipe) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}
