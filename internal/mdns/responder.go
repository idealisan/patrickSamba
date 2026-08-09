package mdns

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// RFC 6762 §8 的探测与宣告参数。
const (
	// probeCount 是探测次数（§8.1：发三次）。
	probeCount = 3
	// probeInterval 是探测间隔（§8.1：250 毫秒）。
	probeInterval = 250 * time.Millisecond
	// probeDefer 是首次探测前的随机等待上限（§8.1：0-250 毫秒，
	// 避免整个网络同时上电时所有设备一起探测）。
	probeDefer = 250 * time.Millisecond

	// announceCount 是宣告次数（§8.3：至少两次，最多八次，间隔翻倍）。
	announceCount = 3
	// announceInterval 是首次宣告后的间隔（§8.3：一秒起步，之后翻倍）。
	announceInterval = time.Second

	// goodbyeCount 是退出时发送 TTL=0 记录的次数（§10.1）。
	goodbyeCount = 2
	// goodbyeInterval 是 goodbye 报文之间的间隔。
	goodbyeInterval = 250 * time.Millisecond
)

// RFC 6762 §9 的冲突退避参数。
const (
	// conflictBackoff 是发生冲突后重新探测前的等待（§8.1：一秒）。
	conflictBackoff = time.Second
	// conflictRateLimit：10 秒内冲突超过 15 次就把间隔拉到 5 秒（§9）。
	conflictRateWindow = 10 * time.Second
	conflictRateCount  = 15
	conflictSlowdown   = 5 * time.Second
)

// RFC 6762 §6 的应答延迟：共享记录要随机延迟 20-120 毫秒，
// 让多台设备的回应错开，顺便给聚合留出机会。
const (
	sharedReplyDelayMin = 20 * time.Millisecond
	sharedReplyDelayMax = 120 * time.Millisecond
)

// legacyTTL 是回应"传统单播查询"时的 TTL 上限（RFC 6762 §6.7）。
//
// 传统客户端（源端口不是 5353）不理解 cache-flush 语义，
// 给它们的记录必须用很短的 TTL，最多 10 秒。
const legacyTTL uint32 = 10

var (
	errStopped  = errors.New("mdns: responder 已停止")
	errConflict = errors.New("mdns: 名字冲突")
)

// state 是 responder 的状态（RFC 6762 §8 的三个阶段）。
type state int32

const (
	stateIdle state = iota
	stateProbing
	stateAnnouncing
	stateResponding
)

// Responder 是一个 mDNS/DNS-SD responder。
//
// 生命周期：New -> Start(ctx) -> ... -> Stop()。
// Start 不阻塞；Stop 会先发 goodbye 再关闭 socket。
type Responder struct {
	rs         *recordSet
	ifaceNames []string
	log        *slog.Logger

	conn *conn

	mu    sync.RWMutex
	st    state
	conns int // 已发生的冲突次数，决定改名后缀

	conflictTimes []time.Time // 冲突时间戳，用于 §9 的限速判断

	conflictCh chan struct{}
	// readdressCh 在网卡地址发生变化时被触发，promote 一次重新宣告。
	readdressCh chan struct{}

	// known 暂存跨报文的已知答案（RFC 6762 §7.2）。
	known *knownAnswerStash

	cancel   context.CancelFunc
	loopDone chan struct{}
	sendWG   sync.WaitGroup // 延迟应答与网卡监视的 goroutine
	stopOnce sync.Once
	started  bool
}

// SetLogger 替换日志器。必须在 Start 之前调用。
func (r *Responder) SetLogger(l *slog.Logger) {
	if l != nil {
		r.log = l
	}
}

// Start 打开组播 socket 并在后台开始探测 / 宣告 / 应答。
//
// ctx 取消等价于调用 Stop（但不会发 goodbye —— 想优雅退出请显式调用 Stop）。
func (r *Responder) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return errors.New("mdns: responder 已经启动过了")
	}
	r.started = true
	r.mu.Unlock()

	c, err := openConn(r.ifaceNames, r.log)
	if err != nil {
		return err
	}
	r.conn = c

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.cancel = cancel
	r.loopDone = make(chan struct{})

	// 外部 ctx 取消时连带停止。
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				cancel()
			case <-runCtx.Done():
			}
		}()
	}

	go c.run(runCtx, r.handlePacket)
	go r.loop(runCtx)

	r.sendWG.Add(1)
	go func() {
		defer r.sendWG.Done()
		r.watchAddresses(runCtx)
	}()

	r.log.Info("mDNS: 已启动",
		"instance", r.rs.instance, "host", r.rs.hostname(), "services", r.serviceTypes())
	return nil
}

func (r *Responder) serviceTypes() []string {
	out := make([]string, len(r.rs.defs))
	for i, d := range r.rs.defs {
		out[i] = d.Type
	}
	return out
}

// Stop 发送 goodbye（TTL=0）后关闭 responder。可重复调用。
func (r *Responder) Stop() {
	r.stopOnce.Do(func() {
		if r.conn == nil {
			return
		}
		// 只有已经宣告出去的名字才需要撤回。
		// 注意宣告阶段（stateAnnouncing）已经发过至少一次宣告，同样要撤回，
		// 否则"刚起来就 Ctrl-C"会在邻居缓存里留下一个死服务好几分钟 ——
		// 反复重启调试时这个残留特别烦人。
		switch r.currentState() {
		case stateAnnouncing, stateResponding:
			r.sendGoodbye()
		}
		if r.cancel != nil {
			r.cancel()
		}
		if r.loopDone != nil {
			<-r.loopDone
		}
		r.sendWG.Wait()
		r.conn.Close()
		r.log.Info("mDNS: 已停止")
	})
}

func (r *Responder) currentState() state {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.st
}

func (r *Responder) setState(s state) {
	r.mu.Lock()
	r.st = s
	r.mu.Unlock()
}

// ---------------------------------------------------------------- 主循环

func (r *Responder) loop(ctx context.Context) {
	defer close(r.loopDone)

	for {
		if err := r.probe(ctx); err != nil {
			return // 只可能是 errStopped
		}

		r.setState(stateAnnouncing)
		err := r.announce(ctx)
		switch {
		case errors.Is(err, errConflict):
			r.renameAfterConflict()
			if !sleepCtx(ctx, r.backoff()) {
				return
			}
			continue
		case err != nil:
			return
		}

		r.setState(stateResponding)
		if !r.serveUntilConflict(ctx) {
			return
		}
		r.renameAfterConflict()
		if !sleepCtx(ctx, r.backoff()) {
			return
		}
	}
}

// serveUntilConflict 停在应答状态，直到发生名字冲突（返回 true）
// 或 responder 被停止（返回 false）。
//
// 期间如果网卡地址变了就重新宣告一轮：DHCP 续约、切换 Wi-Fi、
// 容器网络重建之后，我们之前广播出去的 A/AAAA 已经指向一个不存在的地址，
// 不重新宣告的话客户端要等 TTL（120 秒）过期才会重新发现。
func (r *Responder) serveUntilConflict(ctx context.Context) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-r.conflictCh:
			return true
		case <-r.readdressCh:
			r.log.Info("mDNS: 网卡地址发生变化，重新宣告",
				"host", r.rs.hostname())
			if err := r.announce(ctx); err != nil {
				if errors.Is(err, errConflict) {
					return true
				}
				return false
			}
		}
	}
}

// probe 执行 RFC 6762 §8.1 的探测流程，冲突则改名重来。
func (r *Responder) probe(ctx context.Context) error {
	r.setState(stateProbing)

	for {
		drain(r.conflictCh)

		// §8.1：首次探测前随机等待 0-250ms。
		if !sleepCtx(ctx, rand.N(probeDefer)) {
			return errStopped
		}

		conflicted := false
		for i := 0; i < probeCount && !conflicted; i++ {
			r.sendProbe()
			select {
			case <-ctx.Done():
				return errStopped
			case <-r.conflictCh:
				conflicted = true
			case <-time.After(probeInterval):
			}
		}
		if !conflicted {
			return nil
		}

		r.renameAfterConflict()
		if !sleepCtx(ctx, r.backoff()) {
			return errStopped
		}
	}
}

// announce 执行 RFC 6762 §8.3 的宣告流程。
func (r *Responder) announce(ctx context.Context) error {
	interval := announceInterval
	for i := 0; i < announceCount; i++ {
		r.sendAnnouncement()
		if i == announceCount-1 {
			break
		}
		select {
		case <-ctx.Done():
			return errStopped
		case <-r.conflictCh:
			return errConflict
		case <-time.After(interval):
		}
		interval *= 2 // §8.3：间隔逐次翻倍
	}
	return nil
}

// renameAfterConflict 按 RFC 6762 §9 改名。
func (r *Responder) renameAfterConflict() {
	r.mu.Lock()
	r.conns++
	n := r.conns
	r.conflictTimes = append(r.conflictTimes, time.Now())
	r.mu.Unlock()

	old := r.rs.instance
	r.rs.rename(n)
	r.log.Warn("mDNS: 检测到名字冲突，已改名",
		"old", old, "new", r.rs.instance, "host", r.rs.hostname(), "conflicts", n)
}

// backoff 返回冲突后重新探测前的等待时间。
//
// RFC 6762 §9：10 秒内冲突超过 15 次说明网络里有别的东西在跟我们抢，
// 把重试间隔拉长到 5 秒，避免把链路吵爆。
func (r *Responder) backoff() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := time.Now().Add(-conflictRateWindow)
	kept := r.conflictTimes[:0]
	for _, t := range r.conflictTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.conflictTimes = kept

	if len(kept) > conflictRateCount {
		return conflictSlowdown
	}
	return conflictBackoff
}

// ---------------------------------------------------------------- 发送

// sendProbe 发送探测查询（RFC 6762 §8.1）。
//
// 格式：Question 用 qtype=ANY、QU=1；Authority 段放我们打算宣告的记录，
// 供对端做 §8.2 的同时探测仲裁。
func (r *Responder) sendProbe() {
	names := r.rs.uniqueNames()
	for i := range r.conn.ifaces {
		ifi := &r.conn.ifaces[i]
		m := &Message{}
		for _, n := range names {
			m.Questions = append(m.Questions, Question{
				Name: n, Type: TypeANY, Class: ClassIN, Unicast: true,
			})
		}
		// 提议中的记录不置 cache-flush：此刻我们还没有拥有这些名字。
		for _, rec := range r.proposedRecords(ifi) {
			rec.CacheFlush = false
			m.Authorities = append(m.Authorities, rec)
		}
		r.conn.sendMulticast(m, ifi.Index)
	}
}

// sendAnnouncement 发送无请求应答（RFC 6762 §8.3）。
func (r *Responder) sendAnnouncement() {
	for i := range r.conn.ifaces {
		ifi := &r.conn.ifaces[i]
		m := &Message{
			Flags:   FlagResponse | FlagAuthoritative,
			Answers: r.recordsFor(ifi),
		}
		r.conn.sendMulticast(m, ifi.Index)
	}
}

// sendGoodbye 发送 TTL=0 的记录，让邻居立刻清掉缓存（RFC 6762 §10.1）。
func (r *Responder) sendGoodbye() {
	for n := 0; n < goodbyeCount; n++ {
		for i := range r.conn.ifaces {
			ifi := &r.conn.ifaces[i]
			// NSEC 是否定记录，撤回它没有意义，只撤回正面记录。
			recs := withTTL(withoutType(r.recordsFor(ifi), TypeNSEC), ttlGoodbye)
			m := &Message{
				Flags:   FlagResponse | FlagAuthoritative,
				Answers: recs,
			}
			r.conn.sendMulticast(m, ifi.Index)
		}
		if n != goodbyeCount-1 {
			time.Sleep(goodbyeInterval)
		}
	}
	r.log.Debug("mDNS: 已发送 goodbye（TTL=0）")
}

// ---------------------------------------------------------------- 网卡监视

// addressPollInterval 是网卡地址变化的轮询周期。
//
// 用轮询而不是订阅内核事件（Linux 的 RTNETLINK、macOS 的 SCNetworkReachability、
// Windows 的 NotifyAddrChange）：后者每个平台一套实现，且 Windows 上要走
// syscall，跨平台成本远高于收益。地址变化不是高频事件，30 秒的发现延迟
// 远小于记录 TTL（120 秒），够用。
const addressPollInterval = 30 * time.Second

// watchAddresses 轮询网卡地址，变化时通知主循环重新宣告。
func (r *Responder) watchAddresses(ctx context.Context) {
	last := addressFingerprint(r.conn.ifaces)

	t := time.NewTicker(addressPollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		cur := addressFingerprint(r.conn.ifaces)
		if cur == last {
			continue
		}
		last = cur
		select {
		case r.readdressCh <- struct{}{}:
		default: // 已经有一个未处理的通知，不必重复
		}
	}
}

// addressFingerprint 把这些网卡上会被宣告的地址压成一个可比较的字符串。
//
// 只看我们真正会写进 A/AAAA 的地址（interfaceIPs 的口径），
// 这样 MTU、队列长度之类与 mDNS 无关的属性变化不会触发无谓的重新宣告。
func addressFingerprint(ifaces []net.Interface) string {
	var sb strings.Builder
	for i := range ifaces {
		ifi := &ifaces[i]
		v4, v6 := interfaceIPs(ifi)
		ips := make([]string, 0, len(v4)+len(v6))
		for _, ip := range v4 {
			ips = append(ips, ip.String())
		}
		for _, ip := range v6 {
			ips = append(ips, ip.String())
		}
		// 内核返回的地址顺序没有保证，排序后再比较，避免顺序抖动误报变化。
		sort.Strings(ips)

		sb.WriteString(ifi.Name)
		sb.WriteByte('=')
		sb.WriteString(strings.Join(ips, ","))
		sb.WriteByte(';')
	}
	return sb.String()
}

// ---------------------------------------------------------------- 记录集合

// proposedRecords 是探测时放进 Authority 段的记录：只要正面记录，不含 NSEC。
func (r *Responder) proposedRecords(ifi *net.Interface) []Record {
	out := r.rs.serviceRecords()
	out = append(out, r.rs.addressRecords(ifi)...)
	return withoutType(out, TypePTR) // PTR 是共享记录，不参与唯一性探测
}

// recordsFor 返回在某张网卡上要宣告 / 应答的全部记录。
//
// ifi 为 nil 表示平台没告诉我们收包网卡，只能把所有网卡的地址都带上。
func (r *Responder) recordsFor(ifi *net.Interface) []Record {
	out := r.rs.serviceRecords()

	var hasV4, hasV6 bool
	for _, x := range r.chooseIfaces(ifi) {
		addrs := r.rs.addressRecords(x)
		for _, a := range addrs {
			if a.Type() == TypeA {
				hasV4 = true
			} else {
				hasV6 = true
			}
		}
		out = append(out, addrs...)
	}
	if nsec, ok := r.rs.hostNSEC(hasV4, hasV6); ok {
		out = append(out, nsec)
	}
	out = append(out, r.rs.instanceNSECs()...)
	return out
}

// ownRecords 返回用于冲突判定的正面记录（全部网卡）。
func (r *Responder) ownRecords() []Record {
	out := r.rs.serviceRecords()
	for i := range r.conn.ifaces {
		out = append(out, r.rs.addressRecords(&r.conn.ifaces[i])...)
	}
	return out
}

func (r *Responder) chooseIfaces(ifi *net.Interface) []*net.Interface {
	if ifi != nil {
		return []*net.Interface{ifi}
	}
	out := make([]*net.Interface, 0, len(r.conn.ifaces))
	for i := range r.conn.ifaces {
		out = append(out, &r.conn.ifaces[i])
	}
	return out
}

func (r *Responder) ifaceByIndex(idx int) *net.Interface {
	if idx == 0 {
		return nil
	}
	for i := range r.conn.ifaces {
		if r.conn.ifaces[i].Index == idx {
			return &r.conn.ifaces[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- 收包处理

func (r *Responder) handlePacket(p packet) {
	m := p.msg
	// RFC 6762 §18.3：OPCODE 不是 0（标准查询）的报文一律忽略。
	if m.Flags.Opcode() != 0 {
		return
	}
	// §18.11：RCODE 不为 0 的报文也要忽略。
	if m.Flags.RCode() != 0 {
		return
	}

	if m.Flags.IsResponse() {
		r.checkConflicts(m.Answers)
		r.checkConflicts(m.Additionals)
		return
	}

	// RFC 6762 §7.2：查询者的已知答案可能分散在多个报文里，后续报文
	// 只有 Answer 段、没有 Question 段。先无条件暂存，等真正要应答时再合并。
	r.known.add(p.src, m.Answers, time.Now())

	// 查询报文里也可能夹带别人的 Authority（同时探测），先做仲裁。
	if r.currentState() == stateProbing {
		r.checkProbeTiebreak(m)
		return
	}
	// 探测一旦通过我们就拥有了这些名字（RFC 6762 §8.3），宣告阶段
	// 也必须应答查询 —— 宣告要跨 1+2 秒，这几秒里装死会让客户端
	// 在"刚启动就来浏览"的场景下白等一个完整的重试周期。
	switch r.currentState() {
	case stateAnnouncing, stateResponding:
		r.answerQuery(p)
	}
}

// checkConflicts 检查对方的应答是否与我们宣告的记录冲突（RFC 6762 §9）。
func (r *Responder) checkConflicts(records []Record) {
	if r.currentState() == stateIdle {
		return
	}
	for _, rec := range records {
		if r.isConflict(rec) {
			r.signalConflict(rec)
			return
		}
	}
}

// isConflict 判断收到的一条记录是否与我们的记录冲突。
//
// 判定规则：名字与类型都是我们的，但 rdata 不在我们的集合里。
// 反过来说，rdata 完全一致的记录不算冲突 —— 那多半是我们自己发出去、
// 又通过组播回环收回来的报文。
func (r *Responder) isConflict(rec Record) bool {
	switch rec.Type() {
	case TypePTR:
		// PTR 是共享记录，多台主机同时宣告同一个服务类型完全正常。
		return false
	case TypeNSEC:
		// NSEC 的类型位图与网卡相关（只有 IPv4 的网卡不会有 AAAA），
		// 拿它判冲突会误报，直接跳过。
		return false
	}
	if !r.rs.ownsName(rec.Name) {
		return false
	}

	sameKey := false
	for _, own := range r.ownRecords() {
		if !own.SameKey(rec) {
			continue
		}
		sameKey = true
		if rdataEqual(own.Data, rec.Data) {
			return false
		}
	}
	return sameKey
}

func (r *Responder) signalConflict(rec Record) {
	r.log.Debug("mDNS: 收到冲突记录", "record", rec.String())
	select {
	case r.conflictCh <- struct{}{}:
	default: // 已经有一个未处理的冲突信号，不必重复
	}
}

// checkProbeTiebreak 处理"同时探测"（RFC 6762 §8.2）。
//
// 两台机器同时探测同一个名字时，比较双方 Authority 段里的记录，
// 字典序大的一方获胜；输的一方改名。
func (r *Responder) checkProbeTiebreak(m *Message) {
	if len(m.Authorities) == 0 {
		return
	}
	names := r.rs.uniqueNames()
	for _, name := range names {
		theirs := recordsNamed(m.Authorities, name)
		if len(theirs) == 0 {
			continue
		}
		ours := recordsNamed(r.ownRecords(), name)
		if len(ours) == 0 {
			continue
		}
		if tiebreakLost(ours, theirs) {
			r.log.Debug("mDNS: 同时探测仲裁失败", "name", name)
			r.signalConflict(theirs[0])
			return
		}
	}
}

// tiebreakLost 按 RFC 6762 §8.2.1 比较两组记录，返回 true 表示我们输了。
//
// 规则：两边各自按 (class, type, rdata) 字典序排序后逐条比较，
// 第一处不同就决出胜负；记录少的一方输。
func tiebreakLost(ours, theirs []Record) bool {
	a := sortedRDataKeys(ours)
	b := sortedRDataKeys(theirs)
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := bytes.Compare(a[i], b[i]); c != 0 {
			return c < 0
		}
	}
	return len(a) < len(b)
}

// sortedRDataKeys 把记录编码成可比较的字节串并排序。
//
// 每条记录编码为 class(2) + type(2) + rdata，全部大端，与 §8.2.1 的
// "按 class、type、rdata 依次比较"等价。
func sortedRDataKeys(records []Record) [][]byte {
	out := make([][]byte, 0, len(records))
	for _, rec := range records {
		if rec.Data == nil {
			continue
		}
		key := []byte{
			byte(uint16(rec.Class) >> 8), byte(uint16(rec.Class)),
			byte(uint16(rec.Data.Type()) >> 8), byte(uint16(rec.Data.Type())),
		}
		body, err := rec.Data.pack(nil)
		if err != nil {
			continue
		}
		out = append(out, append(key, body...))
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

// ---------------------------------------------------------------- 应答查询

func (r *Responder) answerQuery(p packet) {
	ifi := r.ifaceByIndex(p.ifIndex)
	pool := r.recordsFor(ifi)

	var (
		answers []Record
		shared  bool
	)
	for _, q := range p.msg.Questions {
		for _, rec := range pool {
			if !rec.Matches(q) || containsRecord(answers, rec) {
				continue
			}
			answers = append(answers, rec)
			// 没有 cache-flush 位的就是共享记录（PTR），回应要随机延迟。
			if !rec.CacheFlush {
				shared = true
			}
		}
	}
	if len(answers) == 0 {
		return
	}

	mode := replyModeFor(p, shared)
	delay := mode.delay()

	// 已知答案抑制刻意放到**发送前**才做：带 TC 位的查询要等 400-500ms
	// 收齐续包（RFC 6762 §7.2），这段时间里 r.known 还会继续进货，
	// 提前算就白等了。
	send := func() {
		answers := suppressKnownAnswers(answers, r.known.get(p.src, time.Now()))
		if len(answers) == 0 {
			return
		}
		additionals := r.additionalsFor(answers, pool)

		resp := &Message{Flags: FlagResponse | FlagAuthoritative}
		if mode.legacy {
			// §6.7：传统单播查询要复用原 ID、带上原问题，
			// TTL 压到 10 秒以内，并清掉它读不懂的 cache-flush 位。
			resp.ID = p.msg.ID
			resp.Questions = p.msg.Questions
			answers = withoutCacheFlush(capTTL(answers, legacyTTL))
			additionals = withoutCacheFlush(capTTL(additionals, legacyTTL))
		}
		resp.Answers = answers
		resp.Additionals = additionals

		if mode.unicast {
			r.conn.sendUnicast(resp, p.src, p.v6)
			return
		}
		r.conn.sendMulticast(resp, p.ifIndex)
	}

	if delay <= 0 {
		send()
		return
	}
	r.sendWG.Add(1)
	go func() {
		defer r.sendWG.Done()
		time.Sleep(delay)
		send()
	}()
}

// additionalsFor 按 RFC 6762 §6 附带"对方接下来一定会问"的记录：
// 回 PTR 就顺带 SRV/TXT/地址，回 SRV 就顺带地址。
func (r *Responder) additionalsFor(answers, pool []Record) []Record {
	var out []Record
	add := func(rec Record) {
		if containsRecord(answers, rec) || containsRecord(out, rec) {
			return
		}
		out = append(out, rec)
	}

	host := r.rs.hostname()
	wantHostAddrs := false

	for _, a := range answers {
		switch d := a.Data.(type) {
		case PTR:
			// 服务实例的 SRV / TXT / NSEC
			for _, rec := range pool {
				if EqualName(rec.Name, d.Target) &&
					(rec.Type() == TypeSRV || rec.Type() == TypeTXT || rec.Type() == TypeNSEC) {
					add(rec)
				}
			}
			wantHostAddrs = true
		case SRV:
			if EqualName(d.Target, host) {
				wantHostAddrs = true
			}
		}
	}

	if wantHostAddrs {
		for _, rec := range pool {
			if !EqualName(rec.Name, host) {
				continue
			}
			switch rec.Type() {
			case TypeA, TypeAAAA, TypeNSEC:
				add(rec)
			}
		}
	}
	return out
}

// suppressKnownAnswers 实现 RFC 6762 §7.1 的已知答案抑制。
//
// 只有当对方缓存里的剩余 TTL 大于我们记录 TTL 的一半时才抑制；
// 否则对方的缓存快过期了，还是该重发。
func suppressKnownAnswers(answers, known []Record) []Record {
	if len(known) == 0 {
		return answers
	}
	out := make([]Record, 0, len(answers))
	for _, a := range answers {
		suppressed := false
		for _, k := range known {
			if a.Equal(k) && k.TTL > a.TTL/2 {
				suppressed = true
				break
			}
		}
		if !suppressed {
			out = append(out, a)
		}
	}
	return out
}

// ---------------------------------------------------------------- 小工具

func anyUnicastQuestion(qs []Question) bool {
	for _, q := range qs {
		if q.Unicast {
			return true
		}
	}
	return false
}

func containsRecord(list []Record, rec Record) bool {
	for _, x := range list {
		if x.Equal(rec) {
			return true
		}
	}
	return false
}

func recordsNamed(list []Record, name string) []Record {
	var out []Record
	for _, rec := range list {
		if EqualName(rec.Name, name) {
			out = append(out, rec)
		}
	}
	return out
}

func withoutType(list []Record, t Type) []Record {
	out := make([]Record, 0, len(list))
	for _, rec := range list {
		if rec.Type() != t {
			out = append(out, rec)
		}
	}
	return out
}

func withoutCacheFlush(list []Record) []Record {
	out := make([]Record, len(list))
	copy(out, list)
	for i := range out {
		out[i].CacheFlush = false
	}
	return out
}

func capTTL(list []Record, max uint32) []Record {
	out := make([]Record, len(list))
	copy(out, list)
	for i := range out {
		if out[i].TTL > max {
			out[i].TTL = max
		}
	}
	return out
}

func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// sleepCtx 睡眠 d，被取消时返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
