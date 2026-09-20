package command

import (
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// TestReleaseAllSurvivesRename 是一条**回归测试**，钉死一个真实存在过的锁泄漏。
//
// 背景：`close.go` 调用 `locks.releaseAll(open.Path, open)`，把当前路径传进来。
// 如果 releaseAll 只扫这一个桶，就隐含了「Open 的路径终生不变」这个假设 ——
// 而这个假设在本代码库里**是假的**：`set_info.go` 处理
// FileRenameInformation 成功后会执行 `open.Path = dst`，因为按 SMB 语义
// 句柄在改名后依然有效。
//
// 于是这条时序会漏锁：
//
//	CREATE f  →  LOCK [0,100) 登记在桶 "old"
//	SET_INFO rename old→new  →  open.Path 变成 "new"
//	CLOSE  →  releaseAll("new", open)  →  只扫桶 "new"，桶 "old" 里那把锁留下
//
// 后果不是「少释放一次」这么轻描淡写：锁表挂在 Share 上、跨会话可见，
// 那段字节范围会被**永久**占住，直到进程重启。任何客户端之后重新创建
// 同名文件再想加锁都会莫名其妙拿到 STATUS_LOCK_NOT_GRANTED。
//
// 修法是让 releaseAll 按 owner 扫全表、不信任 path 参数。
// 这条测试比注释可靠：将来谁再把它改回「只扫一个桶」，这里立刻炸。
func TestReleaseAllSurvivesRename(t *testing.T) {
	var tbl lockTable
	owner := &Open{Path: "old"}
	other := &Open{Path: "old"}

	// 1. 在旧路径上加锁。
	if st := tbl.lock(owner.Path, owner, []wire.LockElement{el(0, 100, true)}); st != status.Success {
		t.Fatalf("加锁应成功，实际 %v", st)
	}

	// 2. 模拟 SET_INFO 改名：句柄还活着，但 Path 变了。
	//    （set_info.go setRename 的最后一行就是这个赋值。）
	owner.Path = "new"

	// 3. CLOSE：close.go 传的是**改名后**的路径。
	tbl.releaseAll(owner.Path, owner)

	// 4. 旧路径上那把锁必须已经没了 —— 用另一个句柄去抢同一区间来验证。
	if st := tbl.lock("old", other, []wire.LockElement{el(0, 100, true)}); st != status.Success {
		t.Fatalf("改名后关闭句柄，旧路径上的锁必须被释放；"+
			"实际仍被占用（%v）—— 锁泄漏回归了", st)
	}
}

// TestReleaseAllDoesNotTouchOtherOwners：全表扫描不能误伤别的句柄。
//
// 把 releaseAll 从「扫一个桶」改成「扫全表」之后，误删别人锁的风险面变大了，
// 所以这条要一起钉住：多个路径、多个持有者，只有目标句柄的锁被摘掉。
func TestReleaseAllDoesNotTouchOtherOwners(t *testing.T) {
	var tbl lockTable
	victim := &Open{Path: "a"}
	keep := &Open{Path: "a"}

	// victim 在两个不同路径上各持一把锁（跨桶）。
	if st := tbl.lock("a", victim, []wire.LockElement{el(0, 10, true)}); st != status.Success {
		t.Fatalf("victim 在 a 上加锁应成功，实际 %v", st)
	}
	if st := tbl.lock("b", victim, []wire.LockElement{el(0, 10, true)}); st != status.Success {
		t.Fatalf("victim 在 b 上加锁应成功，实际 %v", st)
	}
	// keep 在两个路径上也各持一把不重叠的锁。
	if st := tbl.lock("a", keep, []wire.LockElement{el(100, 10, true)}); st != status.Success {
		t.Fatalf("keep 在 a 上加锁应成功，实际 %v", st)
	}
	if st := tbl.lock("b", keep, []wire.LockElement{el(100, 10, true)}); st != status.Success {
		t.Fatalf("keep 在 b 上加锁应成功，实际 %v", st)
	}

	tbl.releaseAll("a", victim)

	probe := &Open{}
	// victim 的两把锁（含另一个桶里的那把）都应释放。
	if st := tbl.lock("a", probe, []wire.LockElement{el(0, 10, true)}); st != status.Success {
		t.Errorf("victim 在 a 上的锁应已释放，实际 %v", st)
	}
	if st := tbl.lock("b", probe, []wire.LockElement{el(0, 10, true)}); st != status.Success {
		t.Errorf("victim 在 b 上的锁应已释放（跨桶），实际 %v", st)
	}
	// keep 的两把锁一根汗毛都不能动。
	if st := tbl.lock("a", probe, []wire.LockElement{el(100, 10, true)}); st != status.LockNotGranted {
		t.Errorf("keep 在 a 上的锁被误删了，实际 %v", st)
	}
	if st := tbl.lock("b", probe, []wire.LockElement{el(100, 10, true)}); st != status.LockNotGranted {
		t.Errorf("keep 在 b 上的锁被误删了，实际 %v", st)
	}
}
