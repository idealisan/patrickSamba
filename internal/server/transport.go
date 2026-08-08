// Package server 实现 SMB 服务端的连接接入与生命周期编排。
//
// 分层（AGENTS.md §5）：Server → Connection → Session → Tree → Open。
// 本文件只负责最底下的**传输层帧**，不理解任何 SMB 语义。
package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Direct TCP 传输（MS-SMB2 §2.1 / RFC 1002 的简化形态）。
//
// 4 字节帧头：
//
//	偏移 0：Zero(1)   —— 必须为 0x00
//	偏移 1：Length(3) —— **大端**，不含这 4 字节头本身
//
// ⚠️ 字节序陷阱：这里的长度前缀是**大端**，而 SMB2 报文体一律**小端**。
// 只监听 445；139/NBSS 不做 —— MS-SMB2 §2.1 明确 SMB 3.1.1 不允许走 NetBIOS，
// 且所有方言都支持 Direct TCP。
const (
	// TransportHeaderSize 是 Direct TCP 帧头长度。
	TransportHeaderSize = 4

	// MaxDirectTCPLength 是 3 字节长度字段能表达的上限（16 MiB - 1）。
	MaxDirectTCPLength = 0x00FFFFFF

	// DefaultMaxFrameSize 是本服务端接受的单帧上限。
	//
	// = MaxTransactSize(1 MiB) + 余量 512 B（SMB2 头 + 命令体 + 复合链填充）。
	// 超过即断连（AGENTS.md §8 资源限制）。
	DefaultMaxFrameSize = 1<<20 + 512
)

// 传输层错误。
var (
	// ErrFrameTooLarge 表示对端声明的帧长度超过本端上限，必须断连。
	ErrFrameTooLarge = errors.New("smb transport: frame too large")
	// ErrBadStreamHeader 表示 Direct TCP 头首字节不是 0。
	ErrBadStreamHeader = errors.New("smb transport: bad direct tcp stream header")
	// ErrEmptyFrame 表示长度为 0 的帧（无意义，视为协议错误）。
	ErrEmptyFrame = errors.New("smb transport: zero-length frame")
)

// Transport 在一条 TCP 连接上收发 Direct TCP 帧。
//
// 读侧与写侧各自串行：ReadFrame 只能由连接的读 goroutine 调用；
// WriteFrame 可能被多个 goroutine 调用（异步响应），因此由调用方
// 通过 Connection 的写队列串行化，Transport 本身不加锁。
type Transport struct {
	conn net.Conn

	// maxFrame 是接受的单帧上限，0 表示用 DefaultMaxFrameSize。
	maxFrame int

	// readTimeout / writeTimeout 为 0 表示不设超时。
	readTimeout  time.Duration
	writeTimeout time.Duration

	// hdr 是复用的读头缓冲，避免每帧分配。
	hdr [TransportHeaderSize]byte
	// whdr 是复用的写头缓冲。
	whdr [TransportHeaderSize]byte
}

// NewTransport 基于一条已建立的 TCP 连接创建传输层。
func NewTransport(conn net.Conn, maxFrame int) *Transport {
	if maxFrame <= 0 || maxFrame > MaxDirectTCPLength {
		maxFrame = DefaultMaxFrameSize
	}
	return &Transport{conn: conn, maxFrame: maxFrame}
}

// SetTimeouts 配置读写超时。传 0 表示该方向不设超时。
func (t *Transport) SetTimeouts(read, write time.Duration) {
	t.readTimeout = read
	t.writeTimeout = write
}

// MaxFrameSize 返回本端接受的单帧上限。
func (t *Transport) MaxFrameSize() int { return t.maxFrame }

// RemoteAddr 返回对端地址。
func (t *Transport) RemoteAddr() net.Addr { return t.conn.RemoteAddr() }

// LocalAddr 返回本端地址。
func (t *Transport) LocalAddr() net.Addr { return t.conn.LocalAddr() }

// Close 关闭底层连接。
func (t *Transport) Close() error { return t.conn.Close() }

// ReadFrame 读取一个完整的 Direct TCP 帧，返回**不含 4 字节头**的载荷。
//
// 严格「读满 4 字节头 → 读满 body」，不做任何粘包猜测。
// 返回的切片是新分配的，调用方可以持有。
//
// 对端正常关闭时返回 io.EOF；读到一半断开返回 io.ErrUnexpectedEOF。
func (t *Transport) ReadFrame() ([]byte, error) {
	if t.readTimeout > 0 {
		if err := t.conn.SetReadDeadline(time.Now().Add(t.readTimeout)); err != nil {
			return nil, err
		}
	}

	if _, err := io.ReadFull(t.conn, t.hdr[:]); err != nil {
		// io.ReadFull 在一个字节都没读到时返回 io.EOF，读到一半返回
		// io.ErrUnexpectedEOF —— 前者是对端优雅关闭，调用方据此区分。
		return nil, err
	}

	if t.hdr[0] != 0x00 {
		return nil, fmt.Errorf("%w: first byte 0x%02X", ErrBadStreamHeader, t.hdr[0])
	}

	// 3 字节大端长度。用 Uint32 读 4 字节再屏蔽最高字节，
	// 等价于 hdr[1]<<16 | hdr[2]<<8 | hdr[3]。
	n := int(binary.BigEndian.Uint32(t.hdr[:]) & MaxDirectTCPLength)

	if n == 0 {
		return nil, ErrEmptyFrame
	}
	if n > t.maxFrame {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, t.maxFrame)
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(t.conn, buf); err != nil {
		if errors.Is(err, io.EOF) {
			// body 读了一半就断开，对调用方而言是异常截断。
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// WriteFrame 写出一个 Direct TCP 帧。payload 不含 4 字节头。
//
// 头与载荷用 net.Buffers 一次性提交，避免把 4 字节头单独 write 出去
// （否则每帧多一个 TCP 小包，Nagle 关闭时吞吐很差）。
func (t *Transport) WriteFrame(payload []byte) error {
	n := len(payload)
	if n == 0 {
		return ErrEmptyFrame
	}
	if n > MaxDirectTCPLength {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, MaxDirectTCPLength)
	}

	if t.writeTimeout > 0 {
		if err := t.conn.SetWriteDeadline(time.Now().Add(t.writeTimeout)); err != nil {
			return err
		}
	}

	// Zero(1)=0 + Length(3, 大端)。
	t.whdr[0] = 0x00
	t.whdr[1] = byte(n >> 16)
	t.whdr[2] = byte(n >> 8)
	t.whdr[3] = byte(n)

	bufs := net.Buffers{t.whdr[:], payload}
	// net.Buffers.WriteTo 内部会处理短写与 writev 回退。
	_, err := bufs.WriteTo(t.conn)
	return err
}
