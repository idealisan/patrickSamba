package vfs

// usage_test.go —— 共享用量统计与缓存。

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestScanUsageCountsAllocation：统计的是**分配空间**，不是逻辑长度。
// 稀疏文件是 Time Machine 的常态（band 文件），按逻辑长度算会高估几个数量级，
// 于是一个几乎没占空间的备份共享会被误判成写满。
func TestScanUsageCountsAllocation(t *testing.T) {
	root := t.TempDir()

	// 先量一次空目录的基线（目录自身也占块）。
	baseline, ok := ScanUsage(root, 0)
	if !ok {
		t.Fatal("空目录统计失败")
	}

	const logical = 64 << 20 // 64 MiB 逻辑长度
	sparse := filepath.Join(root, "band.sparse")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(logical); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// 确认宿主文件系统真的把它存成了稀疏文件；否则本用例没有意义。
	fi, err := os.Stat(sparse)
	if err != nil {
		t.Fatal(err)
	}
	var a Attr
	a.Alloc = allocSizeFallback(fi.Size())
	fillSysAttr(fi, &a)
	if a.Alloc >= logical {
		t.Skipf("宿主文件系统未提供稀疏语义（alloc=%d），跳过", a.Alloc)
	}

	got, ok := ScanUsage(root, 0)
	if !ok {
		t.Fatal("统计失败")
	}
	if got < baseline {
		t.Fatalf("统计结果 %d 小于空目录基线 %d", got, baseline)
	}
	if delta := got - baseline; delta >= logical {
		t.Errorf("稀疏文件被按逻辑长度计入: 增量 %d，实际分配 %d", delta, a.Alloc)
	}
}

// TestScanUsageHardLinkOnce：硬链接只算一次，与 du 的口径一致。
func TestScanUsageHardLinkOnce(t *testing.T) {
	root := t.TempDir()
	const size = 1 << 20

	target := filepath.Join(root, "a.bin")
	if err := os.WriteFile(target, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	single, ok := ScanUsage(root, 0)
	if !ok {
		t.Fatal("统计失败")
	}

	if err := os.Link(target, filepath.Join(root, "b.bin")); err != nil {
		t.Skipf("宿主文件系统不支持硬链接: %v", err)
	}
	linked, ok := ScanUsage(root, 0)
	if !ok {
		t.Fatal("统计失败")
	}

	if linked != single {
		t.Errorf("加了一个硬链接后统计从 %d 变成 %d，重复计数了", single, linked)
	}
}

// TestScanUsageBudget：超出时间预算时如实报告「结果不完整」，
// 而不是把半截数字当成真值返回。
func TestScanUsageBudget(t *testing.T) {
	root := t.TempDir()
	// 条目数必须超过 usageBudgetCheckEvery，否则一次时钟都不会检查。
	for i := 0; i < usageBudgetCheckEvery*3; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%04d", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := ScanUsage(root, time.Nanosecond); ok {
		t.Error("1ns 预算下不可能扫完，却报告了完整结果")
	}
	if _, ok := ScanUsage(root, time.Minute); !ok {
		t.Error("1 分钟预算下应当能扫完这点文件")
	}
}

// TestScanUsageMissingRoot：根目录不存在时报告失败而不是「已用 0」。
// 这个区别很重要：调用方会把「未知」按 0 处理并上报配额全额可用，
// 但那必须是**明确知道自己不知道**的结果，不能是一次静默失败伪装成的 0。
func TestScanUsageMissingRoot(t *testing.T) {
	if _, ok := ScanUsage(filepath.Join(t.TempDir(), "nope"), 0); ok {
		t.Error("不存在的根目录应报告统计失败")
	}
}

func TestUsageBackoff(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    time.Duration
	}{
		{time.Millisecond, usageMinInterval},  // 小目录：受下限约束
		{2 * time.Second, usageMinInterval},   // 20s < 30s 下限
		{10 * time.Second, 100 * time.Second}, // 10 倍耗时，CPU 占用 ≈ 1/10 核
		{5 * time.Minute, usageMaxInterval},   // 受上限约束，不能冻结几小时
		{0, usageMinInterval},                 // 快到测不出耗时
	}
	for _, c := range cases {
		if got := usageBackoff(c.elapsed); got != c.want {
			t.Errorf("usageBackoff(%v) = %v, want %v", c.elapsed, got, c.want)
		}
	}
}

// newFakeUsage 造一个用假扫描器与假时钟驱动的统计器。
func newFakeUsage(clock *atomic.Int64, calls *atomic.Int32, bytes uint64) *shareUsage {
	return &shareUsage{
		root: "/fake",
		now:  func() time.Time { return time.Unix(0, clock.Load()) },
		scan: func(string) (uint64, bool) {
			calls.Add(1)
			return bytes, true
		},
	}
}

// TestShareUsageCaches：拿到结果之后在退避期内不再重扫。
func TestShareUsageCaches(t *testing.T) {
	var clock atomic.Int64
	var calls atomic.Int32
	clock.Store(int64(time.Hour))
	u := newFakeUsage(&clock, &calls, 4096)

	u.refresh()
	if got, ok := u.used(); !ok || got != 4096 {
		t.Fatalf("used() = (%d, %v)，want (4096, true)", got, ok)
	}
	for i := 0; i < 100; i++ {
		u.used()
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("退避期内扫描了 %d 次，应当只有 1 次", n)
	}

	// 时钟越过退避点后应当（异步）重扫一次。
	clock.Add(int64(usageMinInterval + time.Second))
	u.used()
	waitFor(t, func() bool { return calls.Load() == 2 })
}

// TestShareUsageSingleFlight：并发查询不能起出一堆扫描 goroutine。
func TestShareUsageSingleFlight(t *testing.T) {
	var clock atomic.Int64
	var calls atomic.Int32
	clock.Store(int64(time.Hour))

	release := make(chan struct{})
	u := &shareUsage{
		root: "/fake",
		now:  func() time.Time { return time.Unix(0, clock.Load()) },
		scan: func(string) (uint64, bool) {
			calls.Add(1)
			<-release // 卡住，制造「统计进行中」的窗口
			return 8192, true
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.used()
		}()
	}
	wg.Wait()
	close(release)

	waitFor(t, func() bool {
		v, ok := u.used()
		return ok && v == 8192
	})
	if n := calls.Load(); n != 1 {
		t.Errorf("50 个并发查询触发了 %d 次扫描，应当只有 1 次", n)
	}
}

// TestShareUsageNeverBlocks：统计再慢，used() 也必须立刻返回。
// 这是整个设计的核心约束 —— QUERY_FS_INFO 在 Time Machine 备份期间是高频请求。
func TestShareUsageNeverBlocks(t *testing.T) {
	var clock atomic.Int64
	clock.Store(int64(time.Hour))
	release := make(chan struct{})
	defer close(release)

	u := &shareUsage{
		root: "/fake",
		now:  func() time.Time { return time.Unix(0, clock.Load()) },
		scan: func(string) (uint64, bool) {
			<-release
			return 1, true
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			u.used()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("统计进行中时 used() 被阻塞了")
	}
}

// TestShareUsageKeepsLastGoodValue：一次统计失败不应把已知的真值抹成「未知」。
func TestShareUsageKeepsLastGoodValue(t *testing.T) {
	var clock atomic.Int64
	clock.Store(int64(time.Hour))
	fail := atomic.Bool{}
	u := &shareUsage{
		root: "/fake",
		now:  func() time.Time { return time.Unix(0, clock.Load()) },
		scan: func(string) (uint64, bool) {
			if fail.Load() {
				return 0, false
			}
			return 12345, true
		},
	}

	u.refresh()
	fail.Store(true)
	clock.Add(int64(usageMinInterval + time.Second))
	u.refresh()

	if got, ok := u.used(); !ok || got != 12345 {
		t.Errorf("统计失败后应保留上一次的真值，得到 (%d, %v)", got, ok)
	}
}

// TestShareUsageStop：停止之后不再启动新的统计。
func TestShareUsageStop(t *testing.T) {
	var clock atomic.Int64
	var calls atomic.Int32
	clock.Store(int64(time.Hour))
	u := newFakeUsage(&clock, &calls, 4096)

	u.stop()
	u.start()
	u.refresh()
	for i := 0; i < 10; i++ {
		u.used()
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("停止后仍扫描了 %d 次", n)
	}
}

// TestShareUsageNilSafe：nil 接收者上的所有方法都不能崩
// （未设配额的共享根本不构造统计器）。
func TestShareUsageNilSafe(t *testing.T) {
	var u *shareUsage
	u.start()
	u.refresh()
	u.stop()
	if got, ok := u.used(); ok || got != 0 {
		t.Errorf("nil 统计器应返回 (0, false)，得到 (%d, %v)", got, ok)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("等待条件成立超时")
}
