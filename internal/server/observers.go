package server

import "sync/atomic"

// 测试观测点（AGENTS.md §3 / memory/project_timing_criteria_flaky.md）：
//
//	把「某件事有没有发生」从耗时反推改成直接计数断言。
//	这些计数器只是观测点 —— 不参与任何业务判断，不改变生产行为；
//	快路径上只在对应事件已经发生时自增一次，无额外开销。
//
// 约定（同 vfs 包 pathFullScans 的先例）：
//   - 计数器是进程全局的，测试用 before/after 差值断言，禁止 Store(0) 复位；
//   - 因此使用这些计数器的测试文件不得 t.Parallel；
//   - 每个计数器必须有「断言 >0」的反向对照用例，防止空转恒绿。
var (
	// connRejected 累计因超出并发上限被拒的连接数
	// （server.trackConn 的上限分支，acceptLoop 随即关闭该连接）。
	connRejected atomic.Int64

	// rejectLogSuppressed 累计因节流窗口（rejectLogInterval）未写日志的
	// 超限拒绝次数（server.logRejected 的早退分支）。
	rejectLogSuppressed atomic.Int64

	// frameTooLargeDrops 累计因声明帧长超过单帧上限而断开的连接数
	// （connection.serve 读到 ErrFrameTooLarge）。
	frameTooLargeDrops atomic.Int64

	// malformedFrameDrops 累计因致命协议错误帧导致的断连次数
	// （connection.serve 里 handleFrame 返回 error）。
	malformedFrameDrops atomic.Int64

	// idleTimeoutCloses 累计因**空闲超时**（滑动窗口）读超时而被断开的连接数。
	idleTimeoutCloses atomic.Int64

	// handshakeTimeoutCloses 累计因**握手绝对期限**（认证前 hard deadline）
	// 到期而被断开的连接数。与 idleTimeoutCloses 互斥，
	// 区分依据见 transport.deadlineSource。
	handshakeTimeoutCloses atomic.Int64

	// handshakeDeadlineClears 累计认证成功后解除握手期限的次数
	// （connection.onSessionEstablished）。
	handshakeDeadlineClears atomic.Int64
)
