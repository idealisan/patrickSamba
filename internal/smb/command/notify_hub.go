package command

import (
	"strings"
	"sync"

	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// ---------------------------------------------------------------------------
// 目录变更事件中心（CHANGE_NOTIFY 的服务端侧，MS-SMB2 §3.3.5.19）
//
// **为什么不用 inotify / kqueue / ReadDirectoryChangesW**
//
// 三条理由，任何一条单独都足以否决：
//
//  1. **C9**：AGENTS.md §1.2 允许向 OS 索取的只有「一块可读写的普通文件系统」
//     与「网络套接字」两样。目录变更通知是 OS 额外提供的服务，不在白名单里，
//     判据是语义不是形式（"向 OS 要一份现成的变更通知"与"向 OS 要一次
//     挂载/一份名字解析结果"同属禁止侧）。
//  2. **C7**：跨平台要求 linux/amd64、linux/arm64、darwin/arm64、
//     windows/amd64 四目标编译通过。四套 API 语义各不相同（inotify 不递归、
//     kqueue 按 fd、ReadDirectoryChangesW 走句柄、FSEvents 有延迟），
//     等于把最难验证的一层做四遍。
//  3. 本服务是**独占写入方**的共享服务器：绝大多数变更都经过自己的 VFS 层，
//     在那一层记账就能覆盖真实场景。
//
// 代价（必须如实记录在 README/CHANGELOG）：**宿主上其它进程直接改动共享
// 目录**（`touch`、`rm`、另一个 SMB 服务实例）不会触发通知。客户端会因此
// 少一次自动刷新，不会出错 —— 它本来就有轮询兜底。
//
// 同样的取舍 Samba 也做过：它的 change notify 在不少后端上同样是"只报自己
// 的写"（`change notify = no` 时干脆全关）。
// ---------------------------------------------------------------------------

// notifyQueueMax 是单个订阅的待投递事件上限。
//
// 订阅方是**一次性**的（收到一批就应答，客户端再发下一个），所以正常情况
// 下队列里最多攒几个事件。上限是给"客户端挂了但连接还没断"这种情形兜底的：
// 超过上限就不再攒，直接让这条请求以 STATUS_NOTIFY_ENUM_DIR 结束，
// 客户端重新枚举目录即可自愈。
const notifyQueueMax = 1024

// notifyEvent 是一次目录变更的记录。
type notifyEvent struct {
	// path 是变更对象的**完整**共享内相对路径（'/' 分隔，无前导斜杠）。
	//
	// 存全路径而不是 (目录, 名字)：CHANGE_NOTIFY 的 FileName 是**相对被
	// 监视目录**的，同一个事件对监视 "a" 与监视 "a/b" 的两个订阅者要给出
	// 不同的 Name。存全路径才能在匹配时按订阅者各自算出来。
	path string
	// action 是 FILE_NOTIFY_INFORMATION 的 Action（MS-FSCC §2.7.1）。
	action wire.NotifyAction
	// filter 是本事件对应的 CompletionFilter 位（MS-SMB2 §2.2.35），
	// 订阅方按它与自己的 CompletionFilter 求交集来决定是否关心。
	filter wire.CompletionFilter
	// isDir 表示变更对象是目录（决定用 FILE_NAME 还是 DIR_NAME 位）。
	isDir bool
}

// notifyWatch 是一个 CHANGE_NOTIFY 订阅。
//
// 生命周期：handleChangeNotify 创建并登记进 hub → 等到事件后由等待
// goroutine 摘除 → 或句柄关闭/连接拆除时由 cleanup 摘除。
type notifyWatch struct {
	// dir 是被监视目录的共享内相对路径（"" 表示共享根）。
	// 取值在登记时就固定：句柄改名不会改变它监视的对象
	// （MS-SMB2 §3.3.5.19 按 FileId 定位，不按当前路径）。
	dir string
	// watchTree 对应 SMB2_WATCH_TREE：监视整棵子树而不只是本目录。
	watchTree bool
	// filter 是订阅者关心的变更位。
	filter wire.CompletionFilter

	// open 是本订阅依附的目录句柄。句柄关闭时要据此把订阅摘掉并让
	// 等待者回 STATUS_NOTIFY_CLEANUP（MS-SMB2 §3.3.5.19）。
	open *Open

	mu sync.Mutex
	// events 是已匹配、待投递的变更。
	events []wire.NotifyEntry
	// wake 是有新事件时投递的信号。缓冲 1 是为了"信号先到、等待者后进
	// select"时不丢唤醒 —— 等待者随后会一次性排空全部事件。
	wake chan struct{}
	// cleanup 表示句柄已关闭，等待者应回 STATUS_NOTIFY_CLEANUP。
	cleanup bool
	// overflow 表示队列已达上限，等待者应回 STATUS_NOTIFY_ENUM_DIR。
	overflow bool
	// closed 表示订阅已摘除，等待者应当退出。
	closed bool
}

// newNotifyWatch 创建一个订阅。
func newNotifyWatch(dir string, watchTree bool, filter wire.CompletionFilter, o *Open) *notifyWatch {
	return &notifyWatch{
		dir:       dir,
		watchTree: watchTree,
		filter:    filter,
		open:      o,
		wake:      make(chan struct{}, 1),
	}
}

// signal 唤醒等待者。调用方必须已持有 w.mu。
func (w *notifyWatch) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
		// 缓冲里已经有一个未消费的信号，等待者醒来会排空全部事件，
		// 再塞也没有意义。
	}
}

// matches 报告本订阅是否关心 ev，并在关心时返回应写入 FileName 的名字。
//
// 匹配规则（MS-SMB2 §3.3.5.19 + MS-FSCC §2.7.1）：
//   - 变更**直接发生在**被监视目录里 → 命中，Name 就是对象名；
//   - 变更发生在子树里 → 只有置了 SMB2_WATCH_TREE 才命中，
//     Name 是相对被监视目录的路径，分隔符改成反斜杠；
//   - 过滤位：事件的 filter 与订阅的 filter 有交集才关心。
func (w *notifyWatch) matches(ev notifyEvent) (string, bool) {
	if w.filter&ev.filter == 0 {
		return "", false
	}
	dir, name := splitNotifyPath(ev.path)
	if dir == w.dir {
		return name, true
	}
	if !w.watchTree {
		return "", false
	}
	// 子树匹配：被监视目录是变更所在目录的祖先。
	// w.dir == ""（共享根）时 isUnder 恒真，天然覆盖"监视整棵共享树"。
	if !isUnderNotify(dir, w.dir) {
		return "", false
	}
	rel := ev.path
	if w.dir != "" {
		rel = ev.path[len(w.dir)+1:]
	}
	// 线格式的 FileName 用反斜杠分隔（MS-FSCC §2.7.1）。
	return strings.ReplaceAll(rel, "/", `\`), true
}

// cleanupTriggered 报告本订阅是否**因句柄关闭**而结束。
//
// 用于在收尾时区分三种"没有事件"的结局：句柄关闭（→ NOTIFY_CLEANUP）、
// 队列溢出（→ NOTIFY_ENUM_DIR）、被取消（→ 不补发，CANCEL 已应答）。
func (w *notifyWatch) cleanupTriggered() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cleanup
}

// wait 阻塞到有事件可投递、句柄被关闭或 stop 关闭为止。
//
// 返回 (nil, false) 表示被取消（CANCEL 或连接拆除），此时调用方不应再
// 补发响应 —— cancelPending 已经回过 STATUS_CANCELLED 了。
func (w *notifyWatch) wait(stop <-chan struct{}) ([]wire.NotifyEntry, bool) {
	for {
		w.mu.Lock()
		//
		// 判定顺序是有意为之：cleanup 与 overflow 都**优先于**已攒下的事件。
		//
		// overflow 优先的理由：它意味着"漏记过变更"，此时把攒下的那部分
		// 事件交给客户端会让它以为自己看到的是完整视图，而实际上中间少了
		// 东西。回 STATUS_NOTIFY_ENUM_DIR 让客户端重新枚举目录才是诚实的
		// 做法 —— 宁可多枚举一次，也不要给出一份看起来完整、其实缺了条目
		// 的变更列表。
		switch {
		case w.cleanup:
			w.mu.Unlock()
			return nil, true
		case w.overflow:
			w.overflow = false
			w.events = nil
			w.mu.Unlock()
			return nil, true
		case len(w.events) > 0:
			out := w.events
			w.events = nil
			w.mu.Unlock()
			return out, false
		case w.closed:
			w.mu.Unlock()
			return nil, false
		}
		w.mu.Unlock()

		select {
		case <-w.wake:
		case <-stop:
			return nil, false
		}
	}
}

// notifyHub 是挂在 Share 上的变更事件中心：订阅表 + 事件广播。
//
// 挂在 Share 而不是 Session/Tree 上，理由与 lockTable 相同：一个用户在
// Finder 里看到的变化必须能通知到另一个用户挂着的 CHANGE_NOTIFY。
// 零值可用。
type notifyHub struct {
	mu      sync.Mutex
	watches map[*notifyWatch]struct{}
}

// add 登记一个订阅。
func (h *notifyHub) add(w *notifyWatch) {
	h.mu.Lock()
	if h.watches == nil {
		h.watches = make(map[*notifyWatch]struct{})
	}
	h.watches[w] = struct{}{}
	h.mu.Unlock()
}

// remove 摘除一个订阅并唤醒它的等待者（让它退出等待）。幂等。
//
// 摘除后等待者会看到 closed，随即返回 (nil, false)。
func (h *notifyHub) remove(w *notifyWatch) {
	h.mu.Lock()
	if _, ok := h.watches[w]; ok {
		delete(h.watches, w)
	}
	h.mu.Unlock()

	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.signal()
}

// cleanup 摘除依附于某个句柄的全部订阅，并让它们的等待者回
// STATUS_NOTIFY_CLEANUP。
//
// MS-SMB2 §3.3.5.19：目录句柄关闭时，其上的未决 CHANGE_NOTIFY 以
// STATUS_NOTIFY_CLEANUP 结束（不是让它继续挂着，也不是静默丢弃 ——
// 后者会让客户端一直等到超时）。
//
// **必须在持锁区间之外调用**：调用方（Open.close）此刻正持有 o.mu，
// 而这里要取 w.mu，反过来没有 w.mu → o.mu 的路径，顺序是安全的；
// 但 h.mu 与 w.mu 的嵌套顺序必须与 deliver 一致（先 h.mu 后 w.mu，
// 且 h.mu 在取 w.mu 之前释放），这里刻意先摘表再置标志。
func (h *notifyHub) cleanup(o *Open) {
	if o == nil {
		return
	}
	h.mu.Lock()
	if len(h.watches) == 0 {
		// 没有订阅时这是**每条句柄关闭**都要走一遍的路径（本函数是
		// Open.close 的汇合处），空表必须廉价返回，否则每个 CLOSE 都要
		// 白拿一次 h.mu —— 那把锁是所有订阅者共享的。
		h.mu.Unlock()
		return
	}
	var hit []*notifyWatch
	for w := range h.watches {
		if w.open == o {
			delete(h.watches, w)
			hit = append(hit, w)
		}
	}
	h.mu.Unlock()

	for _, w := range hit {
		w.mu.Lock()
		w.cleanup = true
		w.mu.Unlock()
		w.signal()
	}
}

// deliver 把**一批**事件投递给全部关心的订阅。
//
// 参数是切片而不是单个事件，因为有些变更对应**多条**条目 —— 改名就有
// RENAMED_OLD_NAME 与 RENAMED_NEW_NAME 两条（MS-FSCC §2.7.1）。
//
// ⚠️ 一批事件必须在**同一次持锁**内全部追加完再发一次唤醒信号。
// 早先这里是逐个投递、逐个 signal，于是等待者可能在改名只落地一半时
// 就被唤醒：它拿到 OLD_NAME 却没有 NEW_NAME，客户端据此更新会指向一个
// 已经不存在的路径。一次逻辑变更必须**整体可见**，这是修那个 bug 的关键。
//
// 事件到订阅的匹配（含"相对被监视目录的名字"计算）见 notifyWatch.matches。
// 这里刻意**先摘表快照再逐个投递**，避免在持 h.mu 时去取 w.mu ——
// 反向路径（w.mu → h.mu）在别处不存在，但少一层嵌套总是对的。
func (h *notifyHub) deliver(evs ...notifyEvent) {
	if len(evs) == 0 {
		return
	}
	h.mu.Lock()
	if len(h.watches) == 0 {
		h.mu.Unlock()
		return
	}
	targets := make([]*notifyWatch, 0, len(h.watches))
	for w := range h.watches {
		targets = append(targets, w)
	}
	h.mu.Unlock()

	for _, w := range targets {
		// 整批在同一把 w.mu 下处理：等待者要么看到全部，要么一条也看不到。
		w.mu.Lock()
		if w.closed || w.cleanup {
			w.mu.Unlock()
			continue
		}
		added := 0
		for _, ev := range evs {
			name, ok := w.matches(ev)
			if !ok {
				continue
			}
			if len(w.events) >= notifyQueueMax {
				// 队列已满：不再攒，让等待者以 NOTIFY_ENUM_DIR 收场。
				// 客户端重新枚举目录即可自愈，比无限攒下去好。
				w.overflow = true
				break
			}
			w.events = append(w.events, wire.NotifyEntry{Action: ev.action, Name: name})
			added++
		}
		w.mu.Unlock()
		// 全部追加完才唤醒，且只在真有东西追加时唤醒。
		if added > 0 {
			w.signal()
		}
	}
}

// count 返回当前订阅数，用于测试与日志。
func (h *notifyHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.watches)
}

// notifyHub 返回本请求所属共享的变更事件中心。
//
// 刻意**不**直接吃 internal/config：那样协议实现就被绑在配置文件的结构上
// （改一个字段名要动协议包）。由 cmd 层做一次映射，本包只认自己需要的字段，
// 测试也不需要构造一份完整配置。
//
// 树不存在（IPC$、共享已删）时返回 nil —— 四个记账方法都是 nil-safe 的，
// 调用点因此可以无脑写 ctx.notifyHub().notifyAdded(...)，不必各自判空。
func (c *Context) notifyHub() *notifyHub {
	if c.Tree == nil || c.Tree.Share == nil {
		return nil
	}
	return &c.Tree.Share.notify
}

// notifyHub 返回本句柄所属共享的变更事件中心。
//
// 给那些只拿得到 *Open、拿不到 *Context 的记账点用（set_info.go 的各
// setXxx 辅助函数）。返回值可能为 nil，四个记账方法都是 nil-safe 的。
//
// 读取 o.Tree 不加锁：所有记账点都在连接的读循环里执行，与 durable 重连
// 改绑 o.Tree 的那条路径（rebindOpen，同样在读循环）互斥，不构成竞争。
func (o *Open) notifyHub() *notifyHub {
	if o.Tree == nil || o.Tree.Share == nil {
		return nil
	}
	return &o.Tree.Share.notify
}

// ---- 事件记账入口 ----
//
// 这四个方法由命令层在各变更点调用。放在命令层而不是 VFS 层，是因为
// VFS 不知道 SMB 语义（FILE_ACTION_* 的取值、目录/文件之分、CompletionFilter
// 位），而且 VFS 的调用点比命令层多一个数量级。

// notifyAdded 记录一次「对象被创建」。
func (h *notifyHub) notifyAdded(path string, isDir bool) {
	if h == nil {
		return
	}
	h.deliver(notifyEvent{
		path:   path,
		action: wire.FileActionAdded,
		filter: nameFilter(isDir),
		isDir:  isDir,
	})
}

// notifyRemoved 记录一次「对象被删除」。
func (h *notifyHub) notifyRemoved(path string, isDir bool) {
	if h == nil {
		return
	}
	h.deliver(notifyEvent{
		path:   path,
		action: wire.FileActionRemoved,
		filter: nameFilter(isDir),
		isDir:  isDir,
	})
}

// notifyRenamed 记录一次改名：规范要求给出**两条**条目，
// RENAMED_OLD_NAME（旧路径）与 RENAMED_NEW_NAME（新路径），顺序固定。
func (h *notifyHub) notifyRenamed(oldPath, newPath string, isDir bool) {
	if h == nil {
		return
	}
	// 一次投递两条（而不是调两次 deliver）—— 见 deliver 的注释。
	f := nameFilter(isDir)
	h.deliver(
		notifyEvent{path: oldPath, action: wire.FileActionRenamedOldName, filter: f, isDir: isDir},
		notifyEvent{path: newPath, action: wire.FileActionRenamedNewName, filter: f, isDir: isDir},
	)
}

// notifyModified 记录一次「已有对象被改动」。
//
// filter 由调用方按改动内容给（SIZE / LAST_WRITE / ATTRIBUTES /
// LAST_ACCESS / CREATION 的一个或多个），因为"什么算改动"取决于具体命令。
func (h *notifyHub) notifyModified(path string, filter wire.CompletionFilter) {
	if h == nil {
		return
	}
	if filter == 0 {
		return
	}
	h.deliver(notifyEvent{path: path, action: wire.FileActionModified, filter: filter})
}

// nameFilter 返回「名字类」变更对应的过滤位：目录用 DIR_NAME，其余用 FILE_NAME。
func nameFilter(isDir bool) wire.CompletionFilter {
	if isDir {
		return wire.NotifyChangeDirName
	}
	return wire.NotifyChangeFileName
}

// splitNotifyPath 把一个共享内相对路径拆成 (所在目录, 对象名)。
//
// 共享根的目录部分是 ""，与 notifyWatch.dir 取 "" 表示根的口径一致。
func splitNotifyPath(p string) (dir, name string) {
	p = strings.Trim(p, "/")
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// isUnderNotify 报告 dir 是否在 parent 之下（含相等）。
// parent 为 ""（共享根）时恒为真。
func isUnderNotify(dir, parent string) bool {
	if parent == "" {
		return true
	}
	if dir == parent {
		return true
	}
	return strings.HasPrefix(dir, parent+"/")
}
