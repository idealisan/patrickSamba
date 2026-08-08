package vfs

// quota_test.go —— 卷容量配额上报。
//
// 直接测 applyQuota 而不是 StatFS：宿主机的真实剩余空间在 CI 上不可控，
// 用固定的 FSInfo 输入才能断言精确数值。StatFS 的接线由
// TestStatFSQuotaWiring 兜一个粗粒度的检查。

import "testing"

func TestApplyQuota(t *testing.T) {
	const bs = 4096
	// 宿主卷：1000 块总量，其中 400 块已用，600 块空闲。
	base := func() *FSInfo {
		return &FSInfo{BlockSize: bs, TotalBlocks: 1000, FreeBlocks: 600, AvailBlocks: 600}
	}

	cases := []struct {
		name               string
		quota              uint64
		total, free, avail uint64
	}{
		{
			name:  "配额为 0 不生效",
			quota: 0,
			total: 1000, free: 600, avail: 600,
		},
		{
			name:  "配额大于宿主容量时只压缩剩余量",
			quota: 10000 * bs, // 10000 块 > 宿主 1000 块
			total: 1000,       // Total 取小 → 保持宿主值
			free:  600,        // 配额剩余 10000-400=9600 > 600 → 保持宿主值
			avail: 600,
		},
		{
			name:  "配额小于宿主容量",
			quota: 500 * bs, // 500 块
			total: 500,      // min(1000, 500)
			free:  100,      // 500 - 已用 400
			avail: 100,
		},
		{
			name:  "已用量超过配额时剩余为 0 而不是负数回绕",
			quota: 100 * bs, // 100 块 < 已用 400 块
			total: 100,
			free:  0,
			avail: 0,
		},
		{
			name:  "配额恰好等于已用量",
			quota: 400 * bs,
			total: 400,
			free:  0,
			avail: 0,
		},
		{
			name:  "配额不足一个块时向下取整为 0",
			quota: bs - 1,
			total: 0,
			free:  0,
			avail: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := &LocalFS{cfg: LocalConfig{QuotaBytes: c.quota}}
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

// TestApplyQuotaNoUnderflow：宿主返回的 Free > Total（某些网络文件系统
// 确实会这样）时不能触发无符号回绕算出巨大的已用量。
func TestApplyQuotaNoUnderflow(t *testing.T) {
	l := &LocalFS{cfg: LocalConfig{QuotaBytes: 500 * 4096}}
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
	l := &LocalFS{cfg: LocalConfig{QuotaBytes: 1 << 30}}
	info := &FSInfo{BlockSize: 0, TotalBlocks: 10, FreeBlocks: 5, AvailBlocks: 5}
	l.applyQuota(info) // 不 panic 即通过
	if info.TotalBlocks != 10 {
		t.Errorf("BlockSize 为 0 时应原样返回，得到 %d", info.TotalBlocks)
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
