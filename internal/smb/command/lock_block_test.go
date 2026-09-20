package command

import (
	"sync"
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// 本文件钉的是 bh4-A#4：阻塞锁（单元素、未置 FAIL_IMMEDIATELY）应当
// **挂起等待**锁释放后授予，而不是一律立即 LOCK_NOT_GRANTED。
//
// Samba 语义参照（source3/smbd/smb2_lock.c）：首元素裸 SHARED/EXCLUSIVE
// ⇒ blocking=true；冲突时进 pending_queue，等持锁方释放后补授。
// 本实现的等待是**同步有界**的：handler 在锁表上等，超时回
// LOCK_NOT_GRANTED。与规范的差距（无 interim STATUS_PENDING / 无 CANCEL）
// 见 handleLock 的注释 —— 那需要异步未决请求表，涉及 server 层装配。

// blockingEl 拼一条阻塞加锁元素（裸 SHARED/EXCLUSIVE，未置 FAIL_IMMEDIATELY）。
func blockingEl(off, length uint64, excl bool) wire.LockElement {
	return el(off, length, excl)
}

// TestBlockingLockGrantsAfterRelease：A 持独占锁期间 B 的阻塞锁必须等待；
// A 释放后 B 在超时窗口内被授予。
func TestBlockingLockGrantsAfterRelease(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("A 加独占锁应成功")
	}

	done := make(chan status.Status, 1)
	go func() {
		done <- tbl.waitLock("f", b, blockingEl(0, 100, true), 5*time.Second, maxBlockingLockWaiters)
	}()

	// 给 B 一点时间进入等待，然后释放。
	time.Sleep(50 * time.Millisecond)
	if st := tbl.unlock("f", a, []wire.LockElement{unlockEl(0, 100)}); st != status.Success {
		t.Fatalf("A 解锁应成功，实际 %v", st)
	}

	select {
	case st := <-done:
		if st != status.Success {
			t.Fatalf("A 释放后 B 的阻塞锁应被授予，实际 %v", st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("B 的阻塞锁在 A 释放后仍未被授予（唤醒通路断了）")
	}
}

// TestBlockingLockTimesOut：无人释放时阻塞锁在期限后回 LOCK_NOT_GRANTED，
// 且不留任何部分状态。
func TestBlockingLockTimesOut(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("A 加独占锁应成功")
	}

	start := time.Now()
	st := tbl.waitLock("f", b, blockingEl(0, 100, false), 80*time.Millisecond, maxBlockingLockWaiters)
	if st != status.LockNotGranted {
		t.Fatalf("超时后应回 STATUS_LOCK_NOT_GRANTED，实际 %v", st)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("过早返回（%v），像是没等就直接拒了", elapsed)
	}
	if n := countLocks(&tbl, b); n != 0 {
		t.Fatalf("失败的阻塞锁不得留下任何锁，实际留下 %d 条", n)
	}
}

// TestBlockingLockWaiterCap：并发等待者达到上限后，后来的请求**立即**
// 回 LOCK_NOT_GRANTED —— 资源上限优先于等待语义（AGENTS.md §8），
// 否则畸形客户端能用海量阻塞锁耗尽 goroutine。
func TestBlockingLockWaiterCap(t *testing.T) {
	var tbl lockTable
	a := &Open{}
	if !lockOne(&tbl, "f", a, 0, 10, true) {
		t.Fatal("A 加独占锁应成功")
	}

	const waiterCap = 2
	var wg sync.WaitGroup
	release := make(chan struct{})
	for i := 0; i < waiterCap; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release // 等主测试确认它们都已进入等待再放行
			// 1s 期限：足够覆盖本测试的时序，又不让测试拖满真实上限。
			tbl.waitLock("f", &Open{}, blockingEl(0, 10, true), time.Second, waiterCap)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	time.Sleep(50 * time.Millisecond) // 让两个等待者就位

	// 上限已满：第三个请求必须立即失败，不阻塞。
	done := make(chan status.Status, 1)
	go func() {
		done <- tbl.waitLock("f", &Open{}, blockingEl(0, 10, true), 10*time.Second, waiterCap)
	}()
	select {
	case st := <-done:
		if st != status.LockNotGranted {
			t.Fatalf("超过等待者上限应立即回 STATUS_LOCK_NOT_GRANTED，实际 %v", st)
		}
	case <-time.After(time.Second):
		t.Fatal("第三个请求没有立即返回 —— 上限没生效")
	}

	// 收尾：解锁放走前两个等待者（它们的句柄不同，都能被授予？不 ——
	// 它们互相也冲突，只有一个能拿到；另一个等到超时。这里只验证
	// 不悬挂即可）。
	if st := tbl.unlock("f", a, []wire.LockElement{unlockEl(0, 10)}); st != status.Success {
		t.Fatalf("A 解锁应成功，实际 %v", st)
	}
	wg.Wait()
}

// TestBlockingLockWakeOnReleaseAll：句柄消失路径（releaseAll）同样要
// 唤醒等待者 —— 客户端崩溃时它挂起的阻塞锁不能陪葬到超时。
func TestBlockingLockWakeOnReleaseAll(t *testing.T) {
	var tbl lockTable
	a, b := &Open{}, &Open{}

	if !lockOne(&tbl, "f", a, 0, 100, true) {
		t.Fatal("A 加独占锁应成功")
	}

	done := make(chan status.Status, 1)
	go func() {
		done <- tbl.waitLock("f", b, blockingEl(0, 100, true), 5*time.Second, maxBlockingLockWaiters)
	}()
	time.Sleep(50 * time.Millisecond)

	tbl.releaseAll("f", a) // 模拟 A 的句柄消失（CLOSE/断连/durable 回收）

	select {
	case st := <-done:
		if st != status.Success {
			t.Fatalf("releaseAll 后 B 的阻塞锁应被授予，实际 %v", st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("releaseAll 没有唤醒等待者")
	}
}

// TestBlockingLockImmediateWhenAvailable：区间本来就空闲时，阻塞请求
// 必须立即授予 —— 不能傻等第一个唤醒信号。
func TestBlockingLockImmediateWhenAvailable(t *testing.T) {
	var tbl lockTable
	b := &Open{}

	done := make(chan status.Status, 1)
	go func() { done <- tbl.waitLock("f", b, blockingEl(0, 10, true), 5*time.Second, maxBlockingLockWaiters) }()
	select {
	case st := <-done:
		if st != status.Success {
			t.Fatalf("空闲区间的阻塞锁应立即授予，实际 %v", st)
		}
	case <-time.After(time.Second):
		t.Fatal("空闲区间上的阻塞锁没有立即返回")
	}
}
