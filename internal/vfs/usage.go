package vfs

// usage.go —— 共享目录已用量的统计与缓存。
//
// 为什么需要它：配额（`quota_bytes`）限制的是**本共享**能占多少空间，
// 所以「还剩多少」必须是 `配额 − 本共享已用`。而「本共享已用」在 POSIX 上
// 没有现成接口可查（statfs 给的是**整个卷**的数字），只能递归遍历。
//
// 递归遍历一个十万级 band 文件的 `.sparsebundle` 要几百毫秒到数秒，
// 而 Time Machine 会高频发 QUERY_FS_INFO —— 因此这里的核心约束是：
// **统计永远在后台做，查询永远不阻塞**。

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

const (
	// usageMinInterval 是两次统计之间的最小间隔。
	// 30 秒的依据：Time Machine 在备份过程中大约每几秒查一次可用空间，
	// 30 秒的陈旧度对于「还能不能放下这次备份」的判断完全够用，
	// 而更短的间隔在大目录上纯属浪费。
	usageMinInterval = 30 * time.Second

	// usageMaxInterval 是间隔上限。即使统计极慢也不能让数字冻结几小时 ——
	// 那会让「共享已经写满」这件事迟迟反映不到客户端。
	usageMaxInterval = 10 * time.Minute

	// usageCostFactor 让**扫描自身的耗时**决定重扫频率：
	// 下次最早重扫时间 = 本次结束 + 本次耗时 × 该系数。
	// 取 10 意味着统计线程的 CPU/IO 占用被限制在约 1/10 个核以内，
	// 目录越大自动退避得越狠，不需要用户调参。
	usageCostFactor = 10

	// usageBudgetCheckEvery 是预算模式下每遍历多少个条目检查一次时钟。
	// 遍历本身就是系统调用密集的，没必要每个条目都读一次时间。
	usageBudgetCheckEvery = 512
)

// errUsageBudget 是内部哨兵，用于在超出时间预算时提前结束遍历。
var errUsageBudget = errors.New("vfs: usage scan budget exceeded")

// ScanUsage 统计 root 目录树占用的**实际分配空间**（字节）。
//
// budget > 0 时最多花这么长时间；超时会提前返回，此时第二个返回值为 false，
// 已累加的部分结果**不可信**（调用方应当直接丢弃）。budget <= 0 表示不限时。
//
// 口径说明：
//   - 用分配长度（POSIX 的 `st_blocks × 512`）而不是逻辑长度。
//     Time Machine 的 band 文件是稀疏文件，按逻辑长度算会高估几个数量级。
//   - 不跟随符号链接（只算链接自身），避免重复计数与目录环。
//   - 目录自身占用的块也计入，与 `du` 的口径一致。
//   - 硬链接只算一次（按 FileID 去重，只有 NLink>1 的对象才进去重表，
//     正常共享里这张表是空的）。
//   - 单个条目读不到（权限不足、遍历途中被删）就跳过，不让整次统计失败。
func ScanUsage(root string, budget time.Duration) (uint64, bool) {
	var deadline time.Time
	if budget > 0 {
		deadline = time.Now().Add(budget)
	}
	return scanUsage(root, deadline)
}

func scanUsage(root string, deadline time.Time) (total uint64, complete bool) {
	var seen map[uint64]struct{}
	visited := 0

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// 根目录本身读不到 → 整次统计作废。这时候「已用 0」是个谎，
			// 必须让调用方知道自己不知道，而不是拿一个假的 0 去算可用空间。
			if p == root {
				return err
			}
			// 读不到的子树整棵跳过；单个文件读不到就忽略这一个。
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		visited++
		if !deadline.IsZero() && visited%usageBudgetCheckEvery == 0 && time.Now().After(deadline) {
			return errUsageBudget
		}

		fi, err := d.Info()
		if err != nil {
			// 遍历与删除竞态：条目刚被列出就没了，忽略即可。
			return nil
		}

		var a Attr
		// 与 attrFromFileInfo 同样的顺序：先兜底，再让平台实现覆盖成真值。
		a.Alloc = allocSizeFallback(fi.Size())
		fillSysAttr(fi, &a)
		// Windows 的路径式 stat 拿不到真实分配长度、硬链接数与文件索引，
		// 而下面按「NLink > 1 + FileID」去重这三项缺一不可（缺了会把硬链接
		// 重复计数）。POSIX 上是空操作。
		if id, ok := hostIdentityAt(p); ok {
			applyHostIdentity(&a, id)
		}

		if a.NLink > 1 && !fi.IsDir() {
			if seen == nil {
				seen = make(map[uint64]struct{})
			}
			if _, dup := seen[a.FileID]; dup {
				return nil
			}
			seen[a.FileID] = struct{}{}
		}
		if a.Alloc > 0 {
			total += uint64(a.Alloc)
		}
		return nil
	})
	if err != nil {
		return total, false
	}
	return total, true
}

// shareUsage 缓存一个共享的已用量，并在后台按需刷新。
//
// 失效场景（**已知取舍，不是 bug**）：
//
//  1. **冷启动预热窗口**。第一次统计完成之前 valid 为 false，
//     调用方会把已用量当作 0（即上报「配额全部可用」）。
//     构造 LocalFS 时就已经异步启动了首次统计，而客户端要走完
//     NEGOTIATE + SESSION_SETUP + TREE_CONNECT 才可能问到容量，
//     所以正常情况下这个窗口摸不到。极大的共享上可能摸到，
//     后果是短暂高报可用空间（不会低报，不会阻断 Time Machine 启动）。
//  2. **两次统计之间的写入不反映在数字里**。备份过程中上报的可用空间
//     最多滞后一个刷新周期。配额本来就只是**上报口径**、不做强制拦截
//     （真正写满由宿主文件系统 ENOSPC 兜底），所以滞后是可接受的。
//     这里刻意没有做写路径增量维护：那要在 WriteAt / Truncate / Remove /
//     打洞 / 流写 等十几条路径上挂钩子，漏一条就长期漂移，
//     而收益只是让本来就允许滞后的数字更新快一点。
//  3. **统计结果偏保守**。硬链接虽然去重了，但块共享（reflink / btrfs 快照）
//     会被重复计入，此时上报的可用空间偏小 —— 宁可少报也不要让客户端
//     以为还能写。
type shareUsage struct {
	root string

	mu       sync.Mutex
	bytes    uint64
	valid    bool
	scanning bool
	// nextDue 是下次允许启动统计的最早时间。零值表示立刻可以。
	nextDue time.Time
	stopped bool
	// done 在一次统计进行中时非 nil，统计结束时被关闭。
	// 用于让 refresh 等待进行中的那一次，而不是直接放弃。
	done chan struct{}

	// now / scan 是测试注入点，构造后不再修改。
	now  func() time.Time
	scan func(root string) (uint64, bool)
}

func newShareUsage(root string) *shareUsage {
	return &shareUsage{
		root: root,
		now:  time.Now,
		scan: func(root string) (uint64, bool) { return scanUsage(root, time.Time{}) },
	}
}

// start 异步启动首次统计。
func (u *shareUsage) start() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.kickLocked()
	u.mu.Unlock()
}

// stop 停止后台统计。进行中的那一次会跑完但结果被丢弃。
func (u *shareUsage) stop() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.stopped = true
	u.mu.Unlock()
}

// used 返回本共享已用字节数。
//
// 第二个返回值为 false 表示「还没有任何统计结果」，调用方应当按 0 处理
// （见上面失效场景 1）。**本方法永不阻塞**：需要刷新时只是异步起一次统计。
func (u *shareUsage) used() (uint64, bool) {
	if u == nil {
		return 0, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.kickLocked()
	return u.bytes, u.valid
}

// kickLocked 在需要且允许时异步启动一次统计。调用方必须持有 u.mu。
func (u *shareUsage) kickLocked() {
	if u.stopped || u.scanning {
		return
	}
	if !u.nextDue.IsZero() && u.now().Before(u.nextDue) {
		return
	}
	// 先在锁内置位，避免并发的 used() 同时起多个 goroutine。
	u.scanning = true
	u.done = make(chan struct{})
	go u.scanNow()
}

// refresh 同步做一次统计，返回时缓存里一定是一个**不早于本次调用**的结果。
//
// 若已有统计在进行中，先等它跑完再重新统计一次 —— 那一次可能在调用者
// 关心的写入之前就开始了，直接采信它会拿到过期数字。
func (u *shareUsage) refresh() {
	if u == nil {
		return
	}
	for {
		u.mu.Lock()
		if u.stopped {
			u.mu.Unlock()
			return
		}
		if u.scanning {
			ch := u.done
			u.mu.Unlock()
			if ch != nil {
				<-ch
			}
			continue
		}
		u.scanning = true
		u.done = make(chan struct{})
		u.mu.Unlock()
		u.scanNow()
		return
	}
}

// scanNow 执行统计并回写缓存。调用前 u.scanning 必须已经为 true。
func (u *shareUsage) scanNow() {
	start := u.now()
	bytes, ok := u.scan(u.root)
	elapsed := u.now().Sub(start)

	u.mu.Lock()
	defer u.mu.Unlock()
	u.scanning = false
	if u.done != nil {
		close(u.done)
		u.done = nil
	}
	// 统计失败时保留上一次的结果：一个陈旧的真值好过退回「未知」。
	if ok && !u.stopped {
		u.bytes = bytes
		u.valid = true
	}
	// 失败时同样退避，否则读不到的根目录会让这里变成忙循环。
	u.nextDue = u.now().Add(usageBackoff(elapsed))
}

// usageBackoff 由本次统计耗时算出下次统计的最早时间间隔。
func usageBackoff(elapsed time.Duration) time.Duration {
	iv := elapsed * usageCostFactor
	if iv < usageMinInterval {
		iv = usageMinInterval
	}
	if iv > usageMaxInterval {
		iv = usageMaxInterval
	}
	return iv
}
