package server

import (
	"errors"
	"io"
	"strings"

	"github.com/finalappstore/stupidsamba/internal/auth"
	"github.com/finalappstore/stupidsamba/internal/dcerpc"
	"github.com/finalappstore/stupidsamba/internal/dcerpc/srvsvc"
	"github.com/finalappstore/stupidsamba/internal/smb/command"
)

// IPC$ 上的命名管道装配。
//
// 分层说明（AGENTS.md §5）：`internal/smb/command` 只定义 PipeOpener/Pipe
// 两个消费侧接口，**不 import internal/dcerpc** —— DCERPC 是 IPC$ 之上的载荷，
// 不是 SMB 协议的一部分。真正的装配放在这里（server 层在 command 之上，
// 依赖方向仍然是自上而下）。

// srvsvcPipeName 是共享枚举用的命名管道（MS-SRVS）。
//
// 客户端可能写成 `srvsvc` 或 `pipe\srvsvc`，command 层已经把名字统一成
// 小写且去掉前导反斜杠，这里再兜一次 `pipe\` 前缀。
const srvsvcPipeName = "srvsvc"

// STYPE_* 是 SHARE_INFO_1 的共享类型（MS-SRVS §2.2.2.4）。
const (
	stypeDiskTree uint32 = 0x00000000
	stypeIPC      uint32 = 0x00000003
	// stypeSpecial 标记隐藏共享（名字以 $ 结尾的那些）。
	stypeSpecial uint32 = 0x80000000
)

// pipeOpener 把 command.PipeOpener 适配到 internal/dcerpc。
type pipeOpener struct {
	settings *command.Settings
}

// NewPipeOpener 构造 IPC$ 的命名管道后端。
//
// 返回的对象只读且并发安全，整个 Server 共用一个即可。
func NewPipeOpener(settings *command.Settings) command.PipeOpener {
	return &pipeOpener{settings: settings}
}

// OpenPipe 实现 command.PipeOpener。
func (o *pipeOpener) OpenPipe(name string, id *auth.Identity) (command.Pipe, error) {
	name = strings.TrimPrefix(name, `pipe\`)
	name = strings.TrimPrefix(name, "pipe/")

	if name != srvsvcPipeName {
		return nil, command.ErrNoSuchPipe
	}

	h := srvsvc.NewHandler(&shareLister{settings: o.settings, id: id})
	h.ServerName = o.settings.ServerName
	h.ServerComment = o.settings.Domain

	p, err := dcerpc.OpenPipe(name, h)
	if err != nil {
		return nil, err
	}
	return &pipeAdapter{p: p}, nil
}

// pipeAdapter 把 dcerpc 的流式 Write/Read 适配成 command.Pipe 的
// 单次 Transact 语义。
//
// DCERPC over SMB 是严格的请求/响应消息模式，两种形态是等价的；
// 用 Transact 表达可以让 SMB 层不必关心「写了多少、还能读多少」。
type pipeAdapter struct {
	p dcerpc.Pipe
}

// Transact 实现 command.Pipe。
func (a *pipeAdapter) Transact(in []byte, maxOut int) ([]byte, error) {
	if _, err := a.p.Write(in); err != nil {
		return nil, err
	}

	// 一次性把 handler 产出的响应全部读出来。
	// maxOut 只用于**截断**，不能用来决定读多少 —— 剩下的字节还要留给
	// 后续的 READ，丢掉就会让客户端解不出完整的 PDU。
	out, err := io.ReadAll(a.p)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if maxOut > 0 && len(out) > maxOut {
		return out[:maxOut], command.ErrPipeMoreData
	}
	return out, nil
}

// Close 实现 command.Pipe。
func (a *pipeAdapter) Close() error { return a.p.Close() }

// shareLister 把已装配的共享表适配成 dcerpc.ShareLister，
// 并按发起者身份过滤。
//
// 过滤放在这里而不是 dcerpc 里：授权知识属于配置层
// （AGENTS.md §1.1 C8，授权完全由配置决定）。
type shareLister struct {
	settings *command.Settings
	id       *auth.Identity
}

// Shares 实现 dcerpc.ShareLister。
func (l *shareLister) Shares() []dcerpc.ShareEntry {
	out := make([]dcerpc.ShareEntry, 0, len(l.settings.Shares))
	for _, sh := range l.settings.Shares {
		// 不可浏览的共享不出现在枚举里；IPC$ 是例外 ——
		// 客户端（含 smbclient -L 与 Windows 资源管理器）期待看到它。
		if !sh.Browseable && !sh.IsIPC() {
			continue
		}
		// 连不上的共享也不该列出来，否则用户会看到一堆点不开的条目。
		if !sh.Authorize(l.id) {
			continue
		}

		t := stypeDiskTree
		if sh.IsIPC() {
			t = stypeIPC
		}
		// 名字以 $ 结尾的是隐藏共享（Windows 的约定）。
		if strings.HasSuffix(sh.Name, "$") {
			t |= stypeSpecial
		}

		out = append(out, dcerpc.ShareEntry{
			Name:   sh.Name,
			Type:   t,
			Remark: sh.Comment,
		})
	}
	return out
}
