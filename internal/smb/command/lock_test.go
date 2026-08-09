package command

import (
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

// TestLockSameHandleNoSelfConflict：同一个句柄自己的锁不与自己冲突。
func TestLockSameHandleNoSelfConflict(t *testing.T) {
	var tbl lockTable
	a := &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("首次加锁应成功")
	}
	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("同句柄重复锁同区间应成功")
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

// TestLockZeroLengthNeverConflicts：长度为 0 的锁不占据任何字节，
// 与谁都不冲突（MS-FSA §2.1.4.10）。
func TestLockZeroLengthNeverConflicts(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 0, true) {
		t.Fatal("零长度加锁应成功")
	}
	if !lockOne(&tbl, "f", b, 0, 100, true) {
		t.Fatal("零长度锁不应挡住别人")
	}
	if !lockOne(&tbl, "f", a, 50, 0, true) {
		t.Fatal("在别人的独占区间内加零长度锁也应成功")
	}
}

// TestLockOverlapNoIntegerOverflow：offset+length 在 uint64 上回绕时
// 不能把"相交"误判成"不相交"（AGENTS.md §8 整数溢出防御）。
//
// 若实现用裸加法算区间终点，[max-10, max-10+20) 的终点会回绕成一个极小值，
// 冲突判定就会错误放行 —— 那等于锁在文件尾部完全失效。
func TestLockOverlapNoIntegerOverflow(t *testing.T) {
	const max = uint64(math.MaxUint64)
	a, b := &Open{}, &Open{}

	var tbl lockTable
	if !lockOne(&tbl, "f", a, max-10, 20, true) {
		t.Fatal("A 在回绕区间加锁应成功")
	}
	if lockOne(&tbl, "f", b, max-5, 20, true) {
		t.Fatal("回绕区间仍然相交，B 必须被拒")
	}
	if !lockOne(&tbl, "f", b, 0, 10, true) {
		t.Fatal("低位不相交区间应放行")
	}

	// 反向顺序同样成立：先锁高位小区间，再锁跨越它的大区间。
	var tbl2 lockTable
	if !lockOne(&tbl2, "f", a, max-5, 5, true) {
		t.Fatal("A 加锁应成功")
	}
	if lockOne(&tbl2, "f", b, max-10, 20, true) {
		t.Fatal("包含关系也是相交，B 必须被拒")
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
