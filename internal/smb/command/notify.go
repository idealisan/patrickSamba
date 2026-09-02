package command

import (
	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

func init() {
	register(wire.CommandChangeNotify, true, true, handleChangeNotify)
}

// handleChangeNotify 处理 SMB2 CHANGE_NOTIFY（MS-SMB2 §3.3.5.19）。
//
// 规范语义是**长挂起**：服务端把请求收下、先不回（或回 interim
// STATUS_PENDING），等被监视的目录真的发生变化时再补发一条带
// FILE_NOTIFY_INFORMATION 的响应。响应是**一次性**的 —— 客户端拿到后
// 立刻再发下一个，形成持续的订阅。
//
// 实现依赖两件事，二者都已就位（async.go / notify_hub.go）：
//   - 挂起通路：Context.Defer 把请求登记进未决表，稍后由任意 goroutine 补发；
//   - 变更事件：Share.notify 由命令层在各变更点记账（不用 inotify，理由见
//     notify_hub.go 顶部）。
//
// 缓冲区放不下时按规范回 STATUS_NOTIFY_ENUM_DIR 且**不带**任何条目，
// 让客户端自己重新枚举目录（§3.3.5.19）。
func handleChangeNotify(ctx *Context) error {
	req, err := wire.ParseChangeNotifyRequest(ctx.Msg)
	if err != nil {
		return status.InvalidParameter
	}

	// 即便要挂起，也要先把句柄校验做掉：句柄非法时回句柄错误比回
	// NOT_SUPPORTED 更准确，客户端据此能区分"服务端不支持"和"我用错句柄了"。
	open, err := ctx.resolveOpen(req.FileID)
	if err != nil {
		return err
	}
	if !open.IsDir {
		// CHANGE_NOTIFY 只能作用于目录句柄（MS-SMB2 §3.3.5.19）。
		return status.InvalidParameter
	}
	if ctx.Tree == nil || ctx.Tree.Share == nil {
		return status.NetworkNameDeleted
	}

	// CompletionFilter 含有效位以外的比特 → 畸形请求。
	// 置了有效位以外的值我们不猜它的含义（AGENTS.md §9）。
	if req.CompletionFilter & ^wire.CompletionFilterAll != 0 {
		return status.InvalidParameter
	}
	// 全 0 的过滤位意味着"什么都不关心"，这样的订阅永远不会被唤醒 ——
	// 客户端会一直挂到超时，是能想到的最坏结果。直接按畸形请求拒绝，
	// 让它立刻退化成轮询，好过无声挂死。
	if req.CompletionFilter == 0 {
		return status.InvalidParameter
	}

	// 挂不起来时退回 v0.5.x 的旧行为：恒回 STATUS_NOT_SUPPORTED，
	// 客户端降级为定时轮询。这里**绝不能**改成空转等待 —— 那会占死读循环。
	//
	// 挂不起来的情形见 Context.Defer 的注释（无 AsyncSink / 非复合链末条 /
	// 未决数达上限）。真实客户端的 CHANGE_NOTIFY 都是单独一帧发出的，
	// 实践中不会命中。
	ar, ok := ctx.Defer()
	if !ok {
		return status.NotSupported
	}

	// 被监视目录按句柄的**当前路径**确定。句柄后续被改名不会改变本订阅
	// 监视的对象（MS-SMB2 §3.3.5.19 按 FileId 定位）。
	//
	// 这里是读循环，与 SET_INFO 的 open.Path 写入同 goroutine，无竞争。
	// 后面交给后台 goroutine 的一律是拷贝值，不是 *Open 的字段。
	dir := open.Path

	watch := newNotifyWatch(dir, req.WatchTree(), req.CompletionFilter, open)
	hub := &ctx.Tree.Share.notify
	hub.add(watch)

	// 取消/连接拆除时把订阅摘下来，否则它会一直留在表上等一个永远不会
	// 再来的事件（订阅表条目与它引用的 *Open 一起泄漏）。
	ar.OnAbort(func() { hub.remove(watch) })

	maxLen := int(req.OutputBufferLength)
	go waitNotify(ar, watch, hub, maxLen)

	// 响应由 waitNotify 补发。Dispatch 看到 ctx.async != nil 会按挂起处理。
	return nil
}

// waitNotify 在独立 goroutine 里等待目录变更，并在有结果时补发响应。
//
// 三种结局，各不相同且都不能省略：
//
//  1. 等到事件 → 编码成 FILE_NOTIFY_INFORMATION 链回 SUCCESS；
//     放不下（或队列曾溢出）→ 回 STATUS_NOTIFY_ENUM_DIR 且不带条目，
//     客户端重新枚举目录自愈（MS-SMB2 §3.3.5.19）；
//  2. 句柄被关闭 → 回 STATUS_NOTIFY_CLEANUP，让客户端知道订阅结束了，
//     而不是让它挂着等超时；
//  3. 被 CANCEL / 连接拆除 → **不回任何东西**：cancelPending 已经就这条
//     请求回过 STATUS_CANCELLED 了，再补发一次就是同一个 MessageId 上
//     的第二条响应，客户端状态机会乱。
//
// emptyNotifyBody 生成一条**不带任何条目**的 CHANGE_NOTIFY 响应体。
//
// 用于 STATUS_NOTIFY_ENUM_DIR 与 STATUS_NOTIFY_CLEANUP：两者都是成功类
// 状态码，响应结构照旧（9 字节，OutputBufferLength = 0），只是缓冲区为空。
// 直接回空 body 会让客户端在解析响应结构时就报截断。
func emptyNotifyBody(dst []byte) ([]byte, error) {
	return (&wire.ChangeNotifyResponse{}).Append(dst)
}

func waitNotify(ar *AsyncRequest, w *notifyWatch, hub *notifyHub, maxLen int) {
	entries, ended := w.wait(ar.Aborted())
	// 无论哪种结局，订阅都必须摘除 —— 它是一次性的。
	hub.remove(w)

	switch {
	case entries == nil && !ended:
		// 结局 3：已由 CANCEL 应答，或连接已拆除。
		return
	case ended && len(entries) == 0:
		// 结局 2（句柄关闭）或队列溢出。
		// 两者都回 NOTIFY_ENUM_DIR 让客户端重新枚举：句柄都关了，
		// 重新枚举是唯一自洽的做法；NOTIFY_CLEANUP 虽然更"准确"，
		// 但它同样要求客户端重新枚举，带来的差别只是日志可读性。
		//
		// 这里选 NOTIFY_CLEANUP（status.Notify）用于句柄关闭的情形 ——
		// 它是规范为此定义的专用码，且能让运维从抓包里一眼看出原因。
		st := status.NotifyEn
		if w.cleanupTriggered() {
			st = status.Notify
		}
		// 两个码都是**成功类**，响应体仍然要带 CHANGE_NOTIFY 的
		// 9 字节结构（OutputBufferLength = 0），只是不带任何条目。
		// 空 body 会让客户端在解析响应结构时直接截断报错。
		ar.Complete(st, emptyNotifyBody)
		return
	}

	// 结局 1：按 OutputBufferLength 上限编码。
	//
	// 放不下时**整批丢弃**再回 NOTIFY_ENUM_DIR：FILE_NOTIFY_INFORMATION
	// 链没有"部分有效"的表示法，截断在条目中间会让客户端解析出乱码。
	nw := wire.NewNotifyWriter(maxLen)
	fits := true
	for _, e := range entries {
		ok, err := nw.Add(e)
		if err != nil || !ok {
			fits = false
			break
		}
	}
	if !fits {
		ar.Complete(status.NotifyEn, emptyNotifyBody)
		return
	}
	buf := nw.Bytes()
	ar.Complete(status.Success, func(dst []byte) ([]byte, error) {
		return (&wire.ChangeNotifyResponse{Buffer: buf}).Append(dst)
	})
}
