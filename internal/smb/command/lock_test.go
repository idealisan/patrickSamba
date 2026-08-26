package command

import (
	"bytes"
	"math"
	"testing"

	"github.com/finalappstore/stupidsamba/internal/smb/status"
	"github.com/finalappstore/stupidsamba/internal/smb/wire"
)

// el 拼一条加锁用的 LockElement。excl 为 true 时是独占锁，否则共享锁。
func el(off, length uint64, excl bool) wire.LockElement {
	f := wire.LockFlagSharedLock
	if excl {
		f = wire.LockFlagExclusiveLock
	}
	return wire.LockElement{Offset: off, Length: length, Flags: f}
}

// unlockEl 拼一条 UNLOCK 元素。
func unlockEl(off, length uint64) wire.LockElement {
	return wire.LockElement{Offset: off, Length: length, Flags: wire.LockFlagUnlock}
}

// lockOne 是"加一把锁"的简写，返回是否授予。
func lockOne(tbl *lockTable, path string, o *Open, off, length uint64, excl bool) bool {
	return tbl.lock(path, o, []wire.LockElement{el(off, length, excl)}) == status.Success
}

// countLocks 数某个句柄当前持有的锁数量。
func countLocks(tbl *lockTable, o *Open) int {
	tbl.mu.Lock()
	defer tbl.mu.Unlock()

	n := 0
	for _, locks := range tbl.m {
		for _, l := range locks {
			if l.owner == o {
				n++
			}
		}
	}
	return n
}

// TestLockExclusiveBlocksOtherHandle：A 持独占锁后，B 不能锁重叠区间，
// 但可以锁不相交的区间；不同文件互不影响。
func TestLockExclusiveBlocksOtherHandle(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("A 首次加独占锁应成功")
	}
	if lockOne(&tbl, "f", b, 50, 100, true) {
		t.Fatal("B 锁重叠区间 [50,150) 应被拒")
	}
	if !lockOne(&tbl, "f", b, 100, 100, true) {
		t.Fatal("B 锁 [100,200) 与 [0,100) 不相交，应成功")
	}
	if !lockOne(&tbl, "g", b, 0, 100, true) {
		t.Fatal("另一个文件上的同区间不应受影响")
	}
}

// TestLockConflictStatusIsLockNotGranted：冲突时的错误码必须是
// STATUS_LOCK_NOT_GRANTED，客户端据此决定重试还是放弃。
func TestLockConflictStatusIsLockNotGranted(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if st := tbl.lock("f", a, []wire.LockElement{el(0, 100, true)}); st != status.Success {
		t.Fatalf("A 加锁应成功，实际 %v", st)
	}
	if st := tbl.lock("f", b, []wire.LockElement{el(0, 100, true)}); st != status.LockNotGranted {
		t.Fatalf("冲突应回 STATUS_LOCK_NOT_GRANTED，实际 %v", st)
	}
}

// TestLockSharedCoexistExclusiveDoesNot：共享锁之间可以重叠共存，
// 独占锁与任何重叠锁互斥。
func TestLockSharedCoexistExclusiveDoesNot(t *testing.T) {
	var tbl lockTable
	a, b, c := &Open{}, &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, false) {
		t.Fatal("A 共享锁应成功")
	}
	if !lockOne(&tbl, "f", b, 0, 100, false) {
		t.Fatal("B 重叠共享锁应能共存")
	}
	if lockOne(&tbl, "f", c, 50, 10, true) {
		t.Fatal("在共享锁上加独占锁应被拒")
	}
}

// TestLockSameHandleExclusiveNoStack（bh4-A#6）：同一句柄对同一区间
// 重复上独占锁必须被拒 —— torture smb2/lock.c:2311-2321
// 「two exclusive locks do not stack」要求 LOCK_NOT_GRANTED。
func TestLockSameHandleExclusiveNoStack(t *testing.T) {
	var tbl lockTable
	a := &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("首次加独占锁应成功")
	}
	if st := tbl.lock("f", a, []wire.LockElement{el(0, 100, true)}); st != status.LockNotGranted {
		t.Fatalf("同句柄二次独占锁应回 STATUS_LOCK_NOT_GRANTED，实际 %v", st)
	}
}

// TestLockSameHandleReadWriteStacks（bh4-A#6）：同句柄的共享/独占**混合**
// 叠加允许 —— torture smb2/lock.c:2205-2236（shared-over-exclusive 可叠）；
// brlock.c:225-245 brl_conflict 矩阵：READ×READ 不冲突、同 context 涉及
// 至少一方 READ 的组合可叠、其余（W×W）冲突。
func TestLockSameHandleReadWriteStacks(t *testing.T) {
	var tbl lockTable
	a := &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("先上独占锁应成功")
	}
	if st := tbl.lock("f", a, []wire.LockElement{el(0, 100, false)}); st != status.Success {
		t.Fatalf("同句柄在独占锁上叠共享锁应授予，实际 %v", st)
	}

	var tbl2 lockTable
	if !lockOne(&tbl2, "f", a, 0, 100, false) {
		t.Fatal("先上共享锁应成功")
	}
	if st := tbl2.lock("f", a, []wire.LockElement{el(0, 100, true)}); st != status.Success {
		t.Fatalf("同句柄在共享锁上叠独占锁应授予，实际 %v", st)
	}
}

// TestLockAllOrNothing：一条请求里任何一条元素冲突，整条请求都不得留下
// 部分已授予的锁（MS-SMB2 §3.3.5.14）。
func TestLockAllOrNothing(t *testing.T) {
	var tbl lockTable
	a, b, c := &Open{}, &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 200, 50, true) {
		t.Fatal("A 加锁应成功")
	}
	// B 请求两条：第一条本可授予，第二条与 A 冲突 → 整条必须失败。
	st := tbl.lock("f", b, []wire.LockElement{el(0, 50, true), el(200, 50, true)})
	if st != status.LockNotGranted {
		t.Fatalf("含冲突的批量加锁应整体失败，实际 %v", st)
	}
	if n := countLocks(&tbl, b); n != 0 {
		t.Fatalf("失败的批量请求不得留下任何锁，实际留下 %d 条", n)
	}
	// 用第三个句柄验证 [0,50) 确实仍然空闲。
	if !lockOne(&tbl, "f", c, 0, 50, true) {
		t.Fatal("[0,50) 应仍空闲")
	}
}

// TestUnlockRequiresExactRange：解锁区间必须与加锁时完全一致，
// 部分解锁、解别人的锁都必须回 STATUS_RANGE_NOT_LOCKED
// （MS-SMB2 §3.3.5.14）。
func TestUnlockRequiresExactRange(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("加锁应成功")
	}
	if st := tbl.unlock("f", a, []wire.LockElement{unlockEl(0, 50)}); st != status.RangeNotLocked {
		t.Fatalf("部分区间解锁应回 STATUS_RANGE_NOT_LOCKED，实际 %v", st)
	}
	if st := tbl.unlock("f", b, []wire.LockElement{unlockEl(0, 100)}); st != status.RangeNotLocked {
		t.Fatalf("解别人的锁应回 STATUS_RANGE_NOT_LOCKED，实际 %v", st)
	}
	if st := tbl.unlock("f", a, []wire.LockElement{unlockEl(0, 100)}); st != status.Success {
		t.Fatalf("精确解锁应成功，实际 %v", st)
	}
	if !lockOne(&tbl, "f", b, 0, 100, true) {
		t.Fatal("解锁后 B 应能加锁")
	}
}

// TestUnlockAllOrNothing：批量解锁里有一条找不到时，
// 已匹配的那些也不能被摘掉。
func TestUnlockAllOrNothing(t *testing.T) {
	var tbl lockTable
	a := &Open{}

	if !lockOne(&tbl, "f", a, 0, 10, true) {
		t.Fatal("加锁应成功")
	}
	st := tbl.unlock("f", a, []wire.LockElement{unlockEl(0, 10), unlockEl(50, 10)})
	if st != status.RangeNotLocked {
		t.Fatalf("含无效条目的批量解锁应整体失败，实际 %v", st)
	}
	if n := countLocks(&tbl, a); n != 1 {
		t.Fatalf("失败的批量解锁不得摘掉已匹配的锁，实际剩 %d 条", n)
	}
}

// TestReleaseAllOnClose：releaseAll 释放该句柄的全部锁，且不动别人的。
// 句柄关闭时若不释放，崩掉的客户端会把文件永久锁死。
func TestReleaseAllOnClose(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 10, true) || !lockOne(&tbl, "f", a, 100, 10, true) {
		t.Fatal("A 加两把锁应成功")
	}
	if !lockOne(&tbl, "f", b, 200, 10, true) {
		t.Fatal("B 加锁应成功")
	}
	if n := countLocks(&tbl, a); n != 2 {
		t.Fatalf("A 应持有 2 把锁，实际 %d", n)
	}

	tbl.releaseAll("f", a)

	if n := countLocks(&tbl, a); n != 0 {
		t.Fatalf("releaseAll 后 A 应持有 0 把锁，实际 %d", n)
	}
	if n := countLocks(&tbl, b); n != 1 {
		t.Fatalf("B 的锁不应被 A 的 releaseAll 带走，实际 %d", n)
	}

	c := &Open{}
	if !lockOne(&tbl, "f", c, 0, 10, true) || !lockOne(&tbl, "f", c, 100, 10, true) {
		t.Fatal("A 的区间应已可用")
	}
	if lockOne(&tbl, "f", c, 200, 10, true) {
		t.Fatal("B 的区间仍应被占用")
	}
}

// TestLockTableEmptiesOut：锁全部释放后不留空桶，避免长跑内存泄漏。
func TestLockTableEmptiesOut(t *testing.T) {
	var tbl lockTable
	a := &Open{}

	if !lockOne(&tbl, "f", a, 0, 10, true) {
		t.Fatal("加锁应成功")
	}
	tbl.releaseAll("f", a)
	if _, ok := tbl.m["f"]; ok {
		t.Fatal("releaseAll 清空后不应残留路径条目")
	}

	if !lockOne(&tbl, "f", a, 0, 10, true) {
		t.Fatal("重新加锁应成功")
	}
	if st := tbl.unlock("f", a, []wire.LockElement{unlockEl(0, 10)}); st != status.Success {
		t.Fatalf("解锁应成功，实际 %v", st)
	}
	if _, ok := tbl.m["f"]; ok {
		t.Fatal("解掉最后一把锁后也应清掉路径条目")
	}
}

// TestLockZeroLengthMarker（bh4-A#5）：零长度锁是「分界点」标记 ——
// 它不占用字节，但挡住一切**跨过**该偏移点的区间。
// torture smb2/lock.c:1319-1351 zero_byte_tests（Windows 归纳）：
// 持 {10,0} 独占锁时，{9,2}/{9,3} 必须 LOCK_NOT_GRANTED，
// {10,2}/{11,1} 则 OK。Samba brlock.c:158-210 byte_range_overlap 用
// last = ofs+len-1 判交，空区间 last=ofs-1 恰好给出「跨点才冲突」。
func TestLockZeroLengthMarker(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 10, 0, true) {
		t.Fatal("A 在偏移 10 上零长独占锁应成功")
	}
	for _, tc := range []struct {
		off, length uint64
		want        bool
	}{
		{9, 2, false}, // [9,11) 跨过 10 → 拒
		{9, 3, false}, // [9,12) 跨过 10 → 拒
		{10, 2, true}, // [10,12) 从点开始 → 允许
		{11, 1, true}, // [11,12) 点之后 → 允许
		{0, 10, true}, // [0,10) 止于点之前 → 允许
		{8, 1, true},  // [8,9) 不跨点 → 允许
	} {
		got := lockOne(&tbl, "f", b, tc.off, tc.length, true)
		if got != tc.want {
			t.Errorf("B 锁 {%d,%d}: 应授予=%v，实际授予=%v", tc.off, tc.length, tc.want, got)
		}
		if got {
			if st := tbl.unlock("f", b, []wire.LockElement{unlockEl(tc.off, tc.length)}); st != status.Success {
				t.Fatalf("清理 B 的锁 {%d,%d}: %v", tc.off, tc.length, st)
			}
		}
	}
}

// TestLockZeroLengthAtOriginInert：{0,0} 是唯一必须特判的零长锁 ——
// ofs-1 在 0 处下溢成 MaxUint64，若不特判它会「与一切冲突」。
// Samba brlock.c 对此同样特判；locking.c:305 明注「0 byte ranges ARE
// allowed and should be stored」。
func TestLockZeroLengthAtOriginInert(t *testing.T) {
	var tbl lockTable
	a := &Open{}

	if !lockOne(&tbl, "f", a, 0, 0, true) {
		t.Fatal("A 的 {0,0} 独占锁应成功")
	}
	// {0,0} 不挡任何区间，也不被任何区间挡。
	b := &Open{}
	if !lockOne(&tbl, "f", b, 0, 100, true) {
		t.Fatal("B 锁 [0,100) 不应受 {0,0} 影响")
	}
	c := &Open{}
	if !lockOne(&tbl, "g", c, 5, 0, true) {
		t.Fatal("另一路径上的零长锁不受影响")
	}
}

// TestLockForeignExclusiveBlocksZeroLenInside：别人的独占区间内的零长锁
// 必须被拒 —— 零长请求在偏移 P 处的空区间与覆盖「P-1|P 边界」的锁相交。
// 这是 overlaps 改为 last=ofs+len-1 后的自然结论（与 torture 语义一致）。
func TestLockForeignExclusiveBlocksZeroLenInside(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 50, 10, true) {
		t.Fatal("A 锁 [50,60) 应成功")
	}
	if lockOne(&tbl, "f", b, 55, 0, true) {
		t.Fatal("B 在 A 的独占区间内部上零长锁应被拒")
	}
	// 区间边界上的零长锁：起点在独占区间终点处不冲突。
	if !lockOne(&tbl, "f", b, 60, 0, true) {
		t.Fatal("B 在 A 区间右端点上零长锁应允许")
	}
}

// TestLockIOStillExemptOwnLocks（bh4-A#6 回归钉子）：brl 冲突矩阵收紧后，
// READ/WRITE 入口的同句柄豁免**不得**跟着收紧 —— 句柄写自己持有的
// 独占锁区间必须照常放行（Samba STRICT_LOCK_CHECK 按 fsp 豁免自己，
// smb2_read.c:584 / smb2_write.c:392）。
func TestLockIOStillExemptOwnLocks(t *testing.T) {
	p := newLockIOPair(t, bytes.Repeat([]byte("m"), 64))
	if !lockOne(p.table, "f", p.a, 0, 64, true) {
		t.Fatal("A 加独占锁应成功")
	}
	if err := p.runWrite(t, p.a, 0, []byte("self-write")); err != nil {
		t.Fatalf("A 写自己的独占锁区间不应冲突: %v", err)
	}
}

// TestLockWrapRangeRejected（bh4-A#7）：offset+length 在 uint64 上回绕的
// 锁区间必须整条回 STATUS_INVALID_LOCK_RANGE，而不是入库后被当成
// 「与一切冲突」或被误判成不相交（MS-FSA §2.1.4.10 的 byte_range_valid
// 判据；Samba brlock.c:392-400 同）。AGENTS.md §8 整数溢出防御。
func TestLockWrapRangeRejected(t *testing.T) {
	const max = uint64(math.MaxUint64)
	a := &Open{}

	var tbl lockTable
	// {max-10, 20}：终点回绕。旧实现会把它当「与一切冲突」，更早的实现
	// 会把它误判成不相交 —— 两种都错，正确答案是拒绝。
	if st := tbl.lock("f", a, []wire.LockElement{el(max-10, 20, true)}); st != status.InvalidLockRange {
		t.Fatalf("回绕锁区间应回 STATUS_INVALID_LOCK_RANGE，实际 %v", st)
	}
	if n := countLocks(&tbl, a); n != 0 {
		t.Fatalf("被拒的请求不得留下任何锁，实际留下 %d 条", n)
	}

	// 全有或全无：批量里第二条回绕时第一条也不得入库。
	b := &Open{}
	st := tbl.lock("f", b, []wire.LockElement{el(0, 10, true), el(max-5, 10, true)})
	if st != status.InvalidLockRange {
		t.Fatalf("含回绕条目的批量加锁应整体失败，实际 %v", st)
	}
	if n := countLocks(&tbl, b); n != 0 {
		t.Fatalf("失败的批量请求不得留下任何锁，实际留下 %d 条", n)
	}

	// 恰好顶到地址空间末尾的不回绕区间仍然合法：[max-9, max]。
	c := &Open{}
	if st := tbl.lock("f", c, []wire.LockElement{el(max-9, 10, true)}); st != status.Success {
		t.Fatalf("[max-9,max] 不回绕，应授予，实际 %v", st)
	}

	// 零长标记在任意偏移（含 max）本身恒有效；但注意它有分界点语义 ——
	// 上面的 [max-9,max] 跨过 max-1|max 边界，会挡住 {max,0}，
	// 所以这里用另一条路径验证「有效性」本身。
	d := &Open{}
	if st := tbl.lock("g", d, []wire.LockElement{el(max, 0, true)}); st != status.Success {
		t.Fatalf("{max,0} 零长标记应授予，实际 %v", st)
	}
}

// TestLockAdjacentRangesDoNotConflict：相邻但不重叠的区间必须能同时持有。
// 这是 off-by-one 最容易翻车的地方。
func TestLockAdjacentRangesDoNotConflict(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("A 锁 [0,100) 应成功")
	}
	if !lockOne(&tbl, "f", b, 100, 100, true) {
		t.Fatal("B 锁 [100,200) 与 [0,100) 相邻不相交，应成功")
	}
	if lockOne(&tbl, "f", b, 99, 1, true) {
		t.Fatal("[99,100) 落在 A 的区间内，应被拒")
	}
}

// TestLockCommandsRegistered：LOCK / CHANGE_NOTIFY / OPLOCK_BREAK
// 都必须挂上分发表，否则会落到 defaultHandler；且它们都**必须**产生响应
// （不是 CANCEL 那种无响应命令）。
func TestLockCommandsRegistered(t *testing.T) {
	for _, c := range []wire.Command{
		wire.CommandLock, wire.CommandChangeNotify, wire.CommandOplockBreak,
	} {
		if !Registered(c) {
			t.Errorf("%s 未注册到分发表", c)
		}
		if NoResponse(c) {
			t.Errorf("%s 不应被标记为无响应命令", c)
		}
	}
}
