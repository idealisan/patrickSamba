package vfs

// quota_test.go —— 卷容量配额上报。
//
// 直接测 applyQuota 而不是 StatFS：宿主机的真实剩余空间在 CI 上不可控，
// 用固定的 FSInfo 输入才能断言精确数值。StatFS 的接线由
// TestStatFSQuotaWiring / TestStatFSQuotaEmptyShareOnBusyHost 兜住。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// staticUsage 造一个「已经统计完、且短期内不会重扫」的用量缓存，
// 让 applyQuota 的断言完全确定。
func staticUsage(bytes uint64) *shareUsage {
	return &shareUsage{
		bytes:   bytes,
		valid:   true,
		now:     time.Now,
		nextDue: time.Now().Add(time.Hour),
		scan:    func(string) (uint64, bool) { panic("不应该发生重扫") },
	}
}

func TestApplyQuota(t *testing.T) {
	const bs = 4096
	// 宿主卷：1000 块总量，其中 400 块已用，600 块空闲。
	// 注意这 400 块**与本共享无关**（是宿主卷上别的东西占的）。
	base := func() *FSInfo {
		return &FSInfo{BlockSize: bs, TotalBlocks: 1000, FreeBlocks: 600, AvailBlocks: 600}
	}

	cases := []struct {
		name               string
		quota              uint64
		used               uint64 // 本共享已用字节
		total, free, avail uint64
	}{
		{
			name:  "配额为 0 不生效",
			quota: 0, used: 123 * bs,
			total: 1000, free: 600, avail: 600,
		},
		{
			name:  "空共享不受宿主卷已用量影响",
			quota: 500 * bs, used: 0,
			total: 500, // min(1000, 500)
			free:  500, // 配额全额可用，宿主那 400 块与本共享无关
			avail: 500,
		},
		{
			name:  "配额大于宿主容量时只压缩剩余量",
			quota: 10000 * bs, used: 100 * bs,
			total: 1000, // Total 取小 → 保持宿主值，不虚报
			free:  600,  // 配额剩余 9900 块 > 宿主 600 → 受宿主真实剩余约束
			avail: 600,
		},
		{
			name:  "配额小于宿主容量",
			quota: 500 * bs, used: 400 * bs,
			total: 500,
			free:  100, // 500 - 本共享已用 400
			avail: 100,
		},
		{
			name:  "共享已用超过配额时剩余为 0 而不是负数回绕",
			quota: 100 * bs, used: 400 * bs,
			total: 100,
			free:  0,
			avail: 0,
		},
		{
			name:  "配额恰好等于已用量",
			quota: 400 * bs, used: 400 * bs,
			total: 400,
			free:  0,
			avail: 0,
		},
		{
			name:  "已用量不足一个块也要占一个块",
			quota: 500 * bs, used: 1,
			total: 500,
			free:  499, // 向上取整，宁可少报
			avail: 499,
		},
		{
			name:  "配额不足一个块时向下取整为 0",
			quota: bs - 1, used: 0,
			total: 0,
			free:  0,
			avail: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := &LocalFS{cfg: LocalConfig{QuotaBytes: c.quota}, usage: staticUsage(c.used)}
			info := base()
			l.applyQuota(info)
			if info.TotalBlocks != c.total || info.FreeBlocks != c.free || info.AvailBlocks != c.avail {
				t.Errorf("= (total=%d free=%d avail=%d), want (total=%d free=%d avail=%d)",
					info.TotalBlocks, info.FreeBlocks, info.AvailBlocks, c.total, c.free, c.avail)
			}
			// 不变量：可用量永远不能超过剩余量，剩余量永远不能超过总量。
			// 客户端（尤其 Time Machine）会用这几个数做减法，
			// 一旦倒挂就会算出天文数字的可用空间。
			if info.AvailBlocks > info.FreeBlocks {
				t.Errorf("Avail(%d) > Free(%d)", info.AvailBlocks, info.FreeBlocks)
			}
			if info.FreeBlocks > info.TotalBlocks {
				t.Errorf("Free(%d) > Total(%d)", info.FreeBlocks, info.TotalBlocks)
			}
		})
	}
}

// TestApplyQuotaEmptyShareOnBusyHost 是本次修复的**回归测试**，
// 复刻真实阻断场景：
//
//	宿主卷 100 GiB，已用 3.7 GiB（与本共享无关）
//	共享是空的，配额 2 GiB
//
// 旧实现算 2 GiB − 3.7 GiB → **上报 0 可用**，macOS 直接拒绝启动
// Time Machine 备份。修复后应当上报接近整个配额。
func TestApplyQuotaEmptyShareOnBusyHost(t *testing.T) {
	const (
		bs        = 4096
		hostTotal = 100 << 30
		hostUsed  = 3700 << 20 // 3.7 GiB
		quota     = 2 << 30
	)
	info := &FSInfo{
		BlockSize:   bs,
		TotalBlocks: hostTotal / bs,
		FreeBlocks:  (hostTotal - hostUsed) / bs,
		AvailBlocks: (hostTotal - hostUsed) / bs,
	}
	l := &LocalFS{cfg: LocalConfig{QuotaBytes: quota}, usage: staticUsage(0)}
	l.applyQuota(info)

	if got := info.AvailBlocks * bs; got != quota {
		t.Errorf("空共享应上报整个配额可用: got %d bytes, want %d", got, quota)
	}
	if info.AvailBlocks == 0 {
		t.Error("回归: 空共享在高占用宿主卷上被报成 0 可用，Time Machine 会拒绝备份")
	}
}

// TestApplyQuotaOverfilledShare 是上一个用例的**反向对照**：
// 共享自己写超了配额时必须如实报 0，不能因为宿主卷还很空就虚报。
func TestApplyQuotaOverfilledShare(t *testing.T) {
	const (
		bs        = 4096
		hostTotal = 100 << 30
		quota     = 2 << 30
		used      = 3 << 30 // 共享自己已经写了 3 GiB，超过配额
	)
	info := &FSInfo{
		BlockSize:   bs,
		TotalBlocks: hostTotal / bs,
		FreeBlocks:  hostTotal / bs, // 宿主卷几乎全空
		AvailBlocks: hostTotal / bs,
	}
	l := &LocalFS{cfg: LocalConfig{QuotaBytes: quota}, usage: staticUsage(used)}
	l.applyQuota(info)

	if info.FreeBlocks != 0 || info.AvailBlocks != 0 {
		t.Errorf("超配额的共享必须报 0 可用: free=%d avail=%d", info.FreeBlocks, info.AvailBlocks)
	}
	if got := info.TotalBlocks * bs; got != quota {
		t.Errorf("总容量应为配额 %d，得到 %d", quota, got)
	}
}

// TestApplyQuotaColdStart：还没统计出结果时按 0 已用处理，
// 也就是上报「配额全额可用」——宁可短暂高报，也不能低报成 0 而阻断备份。
func TestApplyQuotaColdStart(t *testing.T) {
	const bs = 4096
	l := &LocalFS{
		cfg: LocalConfig{QuotaBytes: 500 * bs},
		usage: &shareUsage{
			now:     time.Now,
			nextDue: time.Now().Add(time.Hour), // 抑制重扫，模拟「统计尚未完成」
			scan:    func(string) (uint64, bool) { panic("不应该发生重扫") },
		},
	}
	info := &FSInfo{BlockSize: bs, TotalBlocks: 1000, FreeBlocks: 900, AvailBlocks: 900}
	l.applyQuota(info)
	if info.AvailBlocks != 500 {
		t.Errorf("预热窗口内应上报配额全额可用，得到 %d 块", info.AvailBlocks)
	}
}

// TestApplyQuotaNoUnderflow：宿主返回的 Free > Total（某些网络文件系统
// 确实会这样）时不能触发无符号回绕算出巨大的已用量。
func TestApplyQuotaNoUnderflow(t *testing.T) {
	l := &LocalFS{cfg: LocalConfig{QuotaBytes: 500 * 4096}, usage: staticUsage(0)}
	info := &FSInfo{BlockSize: 4096, TotalBlocks: 100, FreeBlocks: 900, AvailBlocks: 900}
	l.applyQuota(info)

	if info.FreeBlocks > info.TotalBlocks {
		t.Errorf("Free(%d) > Total(%d)，倒挂未被纠正", info.FreeBlocks, info.TotalBlocks)
	}
	if info.AvailBlocks > info.FreeBlocks {
		t.Errorf("Avail(%d) > Free(%d)", info.AvailBlocks, info.FreeBlocks)
	}
}

// TestApplyQuotaZeroBlockSize：BlockSize 为 0 时不能除零 panic。
func TestApplyQuotaZeroBlockSize(t *testing.T) {
	l := &LocalFS{cfg: LocalConfig{QuotaBytes: 1 << 30}, usage: staticUsage(0)}
	info := &FSInfo{BlockSize: 0, TotalBlocks: 10, FreeBlocks: 5, AvailBlocks: 5}
	l.applyQuota(info) // 不 panic 即通过
	if info.TotalBlocks != 10 {
		t.Errorf("BlockSize 为 0 时应原样返回，得到 %d", info.TotalBlocks)
	}
}

// TestApplyQuotaNilUsage：直接构造的 LocalFS（usage 为 nil）不能崩。
func TestApplyQuotaNilUsage(t *testing.T) {
	l := &LocalFS{cfg: LocalConfig{QuotaBytes: 500 * 4096}}
	info := &FSInfo{BlockSize: 4096, TotalBlocks: 1000, FreeBlocks: 600, AvailBlocks: 600}
	l.applyQuota(info)
	if info.TotalBlocks != 500 || info.AvailBlocks != 500 {
		t.Errorf("nil usage 应按 0 已用处理: total=%d avail=%d", info.TotalBlocks, info.AvailBlocks)
	}
}

// TestStatFSQuotaWiring 确认配额真的接进了 StatFS，
// 而不是只有 applyQuota 这个函数自己是对的。
func TestStatFSQuotaWiring(t *testing.T) {
	root := t.TempDir()

	plain, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	if plain.usage != nil {
		t.Error("未设配额的共享不应该有用量统计器（那是白付一次目录遍历）")
	}
	unlimited, err := plain.StatFS()
	if err != nil {
		t.Fatal(err)
	}

	const quota = 16 << 20 // 16 MiB，几乎肯定小于任何真实卷
	limited, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, QuotaBytes: quota})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limited.Close() }()
	capped, err := limited.StatFS()
	if err != nil {
		t.Fatal(err)
	}

	if capped.TotalBlocks >= unlimited.TotalBlocks {
		t.Errorf("配额应压小上报容量: capped=%d unlimited=%d",
			capped.TotalBlocks, unlimited.TotalBlocks)
	}
	if got := capped.TotalBlocks * uint64(capped.BlockSize); got > quota {
		t.Errorf("上报容量 %d 字节超过配额 %d", got, quota)
	}
	if capped.AvailBlocks > capped.FreeBlocks || capped.FreeBlocks > capped.TotalBlocks {
		t.Errorf("容量字段倒挂: total=%d free=%d avail=%d",
			capped.TotalBlocks, capped.FreeBlocks, capped.AvailBlocks)
	}
}

// TestStatFSQuotaEmptyShareOnBusyHost 是端到端版本的回归测试：
// 在**真实**的宿主卷（本机磁盘必定已用了不少）上开一个空共享，
// 配 16 MiB 配额，上报的可用空间必须接近配额而不是 0。
//
// 这一条是修复前会真实失败的用例 —— 旧实现在这里报 0。
func TestStatFSQuotaEmptyShareOnBusyHost(t *testing.T) {
	root := t.TempDir()
	const quota = 16 << 20

	l, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, QuotaBytes: quota})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	l.usage.refresh() // 消除异步预热窗口，让断言确定

	info, err := l.StatFS()
	if err != nil {
		t.Fatal(err)
	}
	hostUsed := (info.TotalBlocks - info.FreeBlocks) * uint64(info.BlockSize)
	t.Logf("上报: total=%d free=%d avail=%d blocksize=%d",
		info.TotalBlocks, info.FreeBlocks, info.AvailBlocks, info.BlockSize)

	avail := info.AvailBlocks * uint64(info.BlockSize)
	// 空目录自身也占一两个块，所以不要求恰好等于配额，只要求「几乎全部可用」。
	if avail < quota-64<<10 {
		t.Errorf("空共享上报可用 %d 字节，远小于配额 %d（宿主卷已用 %d 字节被错误地算进来了）",
			avail, quota, hostUsed)
	}
}

// TestStatFSQuotaOverfilledShare 是端到端的**反向对照**：
// 共享内写入超过配额的数据后必须上报 0 可用。
// 只测空共享那一条是假阳性 —— 一个恒返回「配额全额可用」的实现也能通过它。
func TestStatFSQuotaOverfilledShare(t *testing.T) {
	root := t.TempDir()
	const (
		quota   = 1 << 20 // 1 MiB
		written = 3 << 20 // 写 3 MiB，是配额的三倍
	)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, written), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, QuotaBytes: quota})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	l.usage.refresh()

	info, err := l.StatFS()
	if err != nil {
		t.Fatal(err)
	}
	if info.FreeBlocks != 0 || info.AvailBlocks != 0 {
		used, _ := l.usage.used()
		t.Errorf("已写入 %d 字节（统计为 %d）超过配额 %d，却上报 free=%d avail=%d",
			written, used, quota, info.FreeBlocks, info.AvailBlocks)
	}
}
