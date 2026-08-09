package mdns

import (
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// RFC 6762 §7.2 多包已知答案抑制。
//
// 查询者的已知答案列表塞不进一个报文时，会把 TC 位置 1，随后再发若干
// 只有 Answer 段、没有 Question 段的续包。响应者收到带 TC 的查询后必须
// 等 400-500 毫秒，把续包里的已知答案也考虑进来再应答；否则会把对方
// 已经缓存好的记录又重发一遍 —— 服务多、客户端多的网段上这是主要的
// 流量来源（Finder 浏览 smb:// 时一次能带上百条已知答案）。
const (
	tcReplyDelayMin = 400 * time.Millisecond
	tcReplyDelayMax = 500 * time.Millisecond

	// knownAnswerTTL 是暂存的已知答案的有效期。
	//
	// 取 1 秒：远大于 §7.2 的 400-500ms 等待窗口，又足够短，
	// 不会把上一轮查询的已知答案带进下一轮（那会误抑制本该发的记录）。
	knownAnswerTTL = time.Second

	// 资源上限（AGENTS.md §8：任何来自网络的缓冲都要有上限）。
	// 恶意或故障的对端可以无限发续包，不设限会把内存吃光。
	maxKnownAnswerSources = 64
	maxKnownAnswersPerSrc = 512
)

// knownAnswerStash 按查询者地址暂存跨报文的已知答案（RFC 6762 §7.2）。
//
// 键用 "IP:端口"：mDNS 查询者的续包与首包来自同一个 socket，源端口一致；
// 只用 IP 做键会让同一台机器上两个不同的查询进程互相误抑制。
type knownAnswerStash struct {
	mu      sync.Mutex
	entries map[string]*knownAnswerEntry
}

type knownAnswerEntry struct {
	records []Record
	expires time.Time
}

func newKnownAnswerStash() *knownAnswerStash {
	return &knownAnswerStash{entries: make(map[string]*knownAnswerEntry)}
}

// add 记下某个查询者报上来的已知答案。records 为空时什么也不做。
func (s *knownAnswerStash) add(src *net.UDPAddr, records []Record, now time.Time) {
	if src == nil || len(records) == 0 {
		return
	}
	key := src.String()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)

	e := s.entries[key]
	if e == nil {
		if len(s.entries) >= maxKnownAnswerSources {
			// 上限已满且没有可回收的过期项：丢弃本次暂存。
			// 后果只是少抑制几条记录（多发一点报文），不影响正确性。
			return
		}
		e = &knownAnswerEntry{}
		s.entries[key] = e
	}
	e.expires = now.Add(knownAnswerTTL)

	room := maxKnownAnswersPerSrc - len(e.records)
	if room <= 0 {
		return
	}
	if len(records) > room {
		records = records[:room]
	}
	e.records = append(e.records, records...)
}

// get 取出某个查询者当前暂存的已知答案（不删除：同一次查询可能触发多条应答）。
func (s *knownAnswerStash) get(src *net.UDPAddr, now time.Time) []Record {
	if src == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.entries[src.String()]
	if e == nil || !now.Before(e.expires) {
		return nil
	}
	out := make([]Record, len(e.records))
	copy(out, e.records)
	return out
}

// gcLocked 清掉过期条目。调用者必须持有锁。
func (s *knownAnswerStash) gcLocked(now time.Time) {
	for k, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, k)
		}
	}
}

// replyMode 描述一次查询该怎么应答。
//
// 纯函数推导，不碰任何状态与 IO（AGENTS.md §5 P1），便于用固定输入做单测。
type replyMode struct {
	// unicast 表示要单播回给查询者，而不是发到组播组。
	unicast bool
	// legacy 表示这是 RFC 6762 §6.7 的"传统单播查询"：
	// 源端口不是 5353，对端是个普通 DNS 解析器，不懂 mDNS 的那套语义。
	legacy bool
	// delayMin/delayMax 是应答前的随机延迟区间，两者相等表示立即发送。
	delayMin time.Duration
	delayMax time.Duration
}

// delay 在 [delayMin, delayMax) 里取一个随机值。
func (m replyMode) delay() time.Duration {
	if m.delayMax <= m.delayMin {
		return m.delayMin
	}
	return m.delayMin + rand.N(m.delayMax-m.delayMin)
}

// replyModeFor 推导应答方式。
//
// shared 表示待发的答案里含共享记录（没有 cache-flush 位的 PTR）。
func replyModeFor(p packet, shared bool) replyMode {
	// §6.7：源端口不是 5353 的是传统单播查询，必须单播回应。
	legacy := p.src == nil || p.src.Port != mdnsPort
	// §5.4：QU 位要求单播回应。
	// §5.5：直接单播发给我们（目的地址不是组播组）的查询也要单播回应 ——
	// 对方显然不在组播组里，回到组播组它收不到。
	unicast := legacy || anyUnicastQuestion(p.msg.Questions) || !p.multicast

	m := replyMode{unicast: unicast, legacy: legacy}
	switch {
	case p.msg.Flags.Truncated():
		// §7.2：还有已知答案在续包里，等 400-500ms 再答。
		// 这条优先级最高：即使是单播查询也得等，否则抑制就失效了。
		m.delayMin, m.delayMax = tcReplyDelayMin, tcReplyDelayMax
	case unicast:
		// 单播回应只有一个接收者，不存在多台设备同时应答的风暴问题，立即发。
	case shared || len(p.msg.Questions) > 1:
		// §6：共享记录或多问题查询，随机延迟 20-120ms 错峰。
		m.delayMin, m.delayMax = sharedReplyDelayMin, sharedReplyDelayMax
	}
	return m
}
