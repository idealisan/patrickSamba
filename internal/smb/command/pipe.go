package command

import (
	"errors"

	"github.com/finalappstore/stupidsamba/internal/auth"
)

// IPC$ 上的命名管道后端契约。
//
// 分层理由（AGENTS.md §5）：`internal/smb/command` **不 import**
// `internal/dcerpc` —— DCERPC 是 IPC$ 之上的载荷，不是 SMB 协议的一部分。
// 这里只定义消费侧接口，具体实现由 `internal/dcerpc` 提供、由 `cmd/` 装配。
// 这样 SMB 层对「管道里跑什么」一无所知，dcerpc 也不必知道 SMB。

// 命名管道相关错误。
var (
	// ErrNoSuchPipe 表示客户端请求了一个本服务不提供的管道。
	// SMB 层映射为 STATUS_OBJECT_NAME_NOT_FOUND。
	ErrNoSuchPipe = errors.New("command: 没有这个命名管道")

	// ErrPipeMoreData 表示响应超出了客户端给的输出缓冲上限，
	// 返回的是被截断的前缀。SMB 层映射为 STATUS_BUFFER_OVERFLOW，
	// 客户端会用更大的缓冲重新读取剩余部分。
	ErrPipeMoreData = errors.New("command: 管道响应被截断")
)

// PipeOpener 打开 IPC$ 上的命名管道实例。
//
// 实现必须并发安全：多个会话会同时打开同名管道，每次调用都要返回
// **独立的** Pipe 实例（DCERPC 的 bind 状态是每实例的）。
type PipeOpener interface {
	// OpenPipe 打开一个管道。name 是**不含前导反斜杠**的管道名，
	// 已转为小写（如 "srvsvc"）。
	//
	// id 是发起者身份，供后端做访问决策与结果过滤
	// （例如 NetShareEnumAll 要按 valid_users 过滤共享）。
	// guest / 匿名会话的 id 也非 nil，用 id.Guest / id.Anonymous 区分。
	//
	// 不认识的管道名必须返回 ErrNoSuchPipe。
	OpenPipe(name string, id *auth.Identity) (Pipe, error)
}

// Pipe 是一个已打开的命名管道实例。
//
// DCERPC over SMB 是严格的请求/响应消息模式，因此这里只需要一个
// Transact，不需要独立的流式 Read/Write：
//
//   - 客户端用 IOCTL FSCTL_PIPE_TRANSCEIVE 时直接对应一次 Transact；
//   - 客户端用 WRITE + READ 两步时，由 SMB 层在 WRITE 时调用 Transact
//     并把结果缓存起来，READ 时分批吐出。
//
// 同一实例不会被并发调用（SMB 层按句柄串行化）。
type Pipe interface {
	// Transact 处理一个**完整的** DCERPC 请求 PDU，返回响应 PDU。
	//
	// maxOut 是客户端允许的最大响应字节数。响应超出时返回
	// 截断后的前缀与 ErrPipeMoreData，剩余部分由后续 READ 取走。
	Transact(in []byte, maxOut int) ([]byte, error)

	// Close 释放管道实例。
	Close() error
}
