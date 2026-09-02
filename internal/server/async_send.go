package server

import (
	"errors"

	"github.com/finalappstore/stupidsamba/internal/smb/command"
	"github.com/finalappstore/stupidsamba/internal/smb/crypto"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// 本文件实现 command.AsyncSink：把一条**挂起后异步完成**的响应写回客户端。
//
// 与读循环里的常规响应（handleSMB2Frame → finishChain）的区别只有两点：
//  1. 它是一条**独立的帧**，不参与任何复合链，因此不需要 8 字节对齐填充，
//     也不需要回填 NextCommand；
//  2. 签名与加密必须在这里自己做完 —— 常规路径由 finishChain 在复合链
//     拼装完成后统一处理，而这里没有"链"可供收口。

// errNoAsyncSession 表示异步响应找不到它所属的会话，无法完成加密。
//
// 会话在等待期间被 LOGOFF 掉是正常现象：那之后补发的响应本来也送不到，
// 调用方一律按"送不到"处理（见 command.AsyncRequest.Complete）。
var errNoAsyncSession = errors.New("smb: 异步响应找不到所属会话")

// asyncSender 把 command.AsyncSink 实现在一条 Connection 上。
//
// 用独立的小结构体而不是让 *Connection 直接实现接口，是为了不把
// SendAsyncResponse 暴露到 Connection 的公共面上 —— 它只该被协议层
// 通过接口调用（与 breakSender 同构）。
type asyncSender struct{ c *Connection }

// compile-time 断言：*Connection 装配时注入的接口实现必须满足协议层的契约。
var _ command.AsyncSink = asyncSender{}

// SendAsyncResponse 补发一条异步响应（command.AsyncSink）。
//
// hdr 是协议层派生好的响应头：MessageId / SessionId / TreeId 与被挂起的
// 原始请求一致（客户端据此对账），ASYNC 请求还带回了当初分配的 AsyncId。
//
// 处理顺序刻意与读循环保持**一致**：先签名、再加密。
// MS-SMB2 §3.3.5.2.4 规定加密消息不签名，反过来（先加密再签名）会让
// 客户端在外层信封内找不到有效的内层签名。
func (s asyncSender) SendAsyncResponse(hdr wire.Header, body []byte, signKey []byte, encrypted bool) error {
	frame := hdr.Append(nil)
	frame = append(frame, body...)

	if len(signKey) > 0 {
		hdr.Flags |= wire.FlagSigned
		// 头已经写进 frame 了，SIGNED 位必须就地回填 —— 它在签名覆盖范围内。
		_ = hdr.PutAt(frame)
		if err := crypto.SignWith(s.c.state.SigningAlg(), signKey, frame); err != nil {
			s.c.log.Error("异步响应签名失败", "err", err)
			// 签名失败也要把帧发出去：不发等于静默不答，客户端会挂到超时。
			// 客户端验签失败会断连，那是比挂死更好的结局。
		}
	}

	if encrypted {
		enc, err := s.encrypt(hdr.SessionID, frame)
		if err != nil {
			return err
		}
		frame = enc
	}

	return s.c.sendUnsolicited(frame)
}

// encrypt 用会话的 S2C 密钥把明文响应封装进 SMB3 TRANSFORM_HEADER。
//
// nonce 必须走会话级单调递增计数器（Session.NextEncryptNonce）：同一
// (密钥, 方向) 下 nonce 复用会直接泄露明文异或值（CCM/GCM）。
func (s asyncSender) encrypt(sessionID uint64, plain []byte) ([]byte, error) {
	sess := s.c.state.Session(sessionID)
	if sess == nil {
		return nil, errNoAsyncSession
	}
	cipher := crypto.Cipher(s.c.state.Cipher)
	if cipher == 0 {
		return nil, errNoAsyncSession
	}
	enc, err := crypto.Encrypt(cipher, sess.EncryptKey(), sess.NextEncryptNonce()[:], sessionID, plain)
	if err != nil {
		s.c.log.Error("异步响应加密失败", "session", sessionID, "err", err)
		return nil, err
	}
	return enc, nil
}
