package server

import "sync"

// SMB2 Credit 管理（MS-SMB2 §3.3.1.1 / §3.3.5.2.4，protocol-notes §12）。
//
// Credit 是 SMB2 的流控机制：客户端每发一条请求消耗若干 credit，
// 服务端在响应里授予新的 credit。**credit 耗尽客户端会停止发送并挂死**，
// 因此本实现的第一原则是：**任何响应都至少授予 1 个 credit**。
const (
	// CreditUnit 是一个 credit 覆盖的字节数（64 KiB，MS-SMB2 §3.3.1.1）。
	CreditUnit = 65536

	// DefaultMaxCredits 是单连接可持有的 credit 上限。
	// Windows Server 默认 512，够大且不至于让恶意客户端占用过多内存。
	DefaultMaxCredits = 512

	// InitialCredits 是 NEGOTIATE 阶段授予的初始 credit 数。
	//
	// MS-SMB2 §3.3.5.4 要求 NEGOTIATE 响应至少授予 1 个。给 1 是最保守的
	// 做法：客户端拿到 1 个后发 SESSION_SETUP，我们再按其请求量放量。
	InitialCredits = 1
)

// CreditCharge 计算一条消息应当消耗的 credit 数（MS-SMB2 §3.3.5.2.5）。
//
//	CreditCharge = ceil(max(SendPayloadSize, ExpectedResponsePayloadSize) / 65536)
//
// 至少为 1。SMB 2.0.2 不支持 multi-credit，此时恒为 1（调用方需按方言判断，
// 见 Credits.Charge）。
func CreditCharge(sendSize, expectedResponseSize int) uint16 {
	n := sendSize
	if expectedResponseSize > n {
		n = expectedResponseSize
	}
	if n <= 0 {
		return 1
	}
	// 向上取整除法，避免浮点。
	charge := (n + CreditUnit - 1) / CreditUnit
	if charge < 1 {
		charge = 1
	}
	// uint16 上限保护：单帧最大 1 MiB+512B → charge 最多 17，不会溢出，
	// 但恶意的 expectedResponseSize 计算可能溢出，这里兜底。
	if charge > 0xFFFF {
		charge = 0xFFFF
	}
	return uint16(charge)
}

// Credits 是一条连接的 credit 池，并发安全。
//
// 语义（MS-SMB2 §3.3.1.1 Connection.CommandSequenceWindow 的简化实现）：
// 我们不做严格的 sequence window 校验（那主要防重放，且客户端乱序发送时
// 极易误伤），只做数量上的收放，保证客户端永远有 credit 可用。
type Credits struct {
	mu sync.Mutex

	// granted 是当前已授予客户端、尚未被消耗的 credit 数。
	granted uint16
	// max 是 granted 的上限。
	max uint16

	// multiCredit 表示当前方言是否支持多信用（2.1 起）。
	// 2.0.2 下 CreditCharge 字段保留为 0，每条消息恒消耗 1 个。
	multiCredit bool
}

// NewCredits 创建一个 credit 池。max 传 0 使用 DefaultMaxCredits。
func NewCredits(max uint16) *Credits {
	if max == 0 {
		max = DefaultMaxCredits
	}
	return &Credits{granted: InitialCredits, max: max}
}

// SetMultiCredit 在方言协商完成后设置是否启用多信用。
func (c *Credits) SetMultiCredit(v bool) {
	c.mu.Lock()
	c.multiCredit = v
	c.mu.Unlock()
}

// Charge 把请求头里的 CreditCharge 归一化为实际消耗值。
//
// MS-SMB2 §3.3.5.2.5：不支持 multi-credit 的方言（2.0.2）里该字段保留为 0，
// 一律按 1 计。支持 multi-credit 时字段为 0 也按 1 计（部分客户端会填 0）。
func (c *Credits) Charge(headerCreditCharge uint16) uint16 {
	c.mu.Lock()
	multi := c.multiCredit
	c.mu.Unlock()

	if !multi || headerCreditCharge == 0 {
		return 1
	}
	return headerCreditCharge
}

// Grant 消耗 charge 个 credit 并计算本次响应应当授予的 credit 数。
//
// requested 是请求头里的 CreditRequest 字段。
//
// 策略（protocol-notes §12）：尽量满足客户端的请求量，把 granted 拉到
// 客户端期望的水位，但不超过 max。**返回值恒 >= 1** —— 这是死锁防线：
// 哪怕客户端请求 0 个 credit，我们也必须给 1 个，否则它再也发不出请求。
func (c *Credits) Grant(charge, requested uint16) uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 先扣除本次消耗。客户端可能超发（我们不严格拒绝，只把水位归零），
	// 因为严格拒绝会因乱序/重传误杀正常客户端。
	if charge > c.granted {
		c.granted = 0
	} else {
		c.granted -= charge
	}

	// 客户端没要就至少补回本次消耗的量，保持水位不下降。
	want := requested
	if want == 0 {
		want = charge
	}

	// 不变式：granted <= max，所以 room 不会下溢。
	room := c.max - c.granted
	if want > room {
		want = room
	}
	if want < 1 {
		// 水位已顶到 max。协议上客户端有的是 credit，但仍然必须授予 1，
		// 否则某些客户端实现会判定为流控停滞而挂起。
		want = 1
	}

	c.granted += want
	if c.granted > c.max {
		c.granted = c.max
	}
	return want
}

// Granted 返回当前授予水位，用于日志与测试。
func (c *Credits) Granted() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.granted
}
