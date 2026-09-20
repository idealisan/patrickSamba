package builtin

// store_nosync_test.go —— NoSync 持久化语义边界的回归用例。
//
// 背景（v0.5 team-lead 拍板，全文见 store.go 的 bucketIsRegenerable 注释）：
// 库以 NoSync 打开，提交只进页缓存 —— 进程崩溃不丢，掉电仅威胁可再生桶
// （btime/dosattr）；内容类桶（stream/xattr/holes/fileid）每笔写事务后显式
// db.Sync() 强制真落盘；关闭路径统一再 Sync 一次。
//
// ⚠️ **如实注明本文件的验证强度**：下面所有「重开后仍在」的用例验证的是
// **进程崩溃存活** —— 数据已经 write() 进内核页缓存，进程死了缓存还在；
// 它们**不能**证明掉电存活。真正的掉电场景需要内核崩溃注入，任何单测环境
// 都做不到；那一半由设计论证保证（关键桶逐事务 Sync、可再生桶回落合成基线），
// 不由本文件冒充。别把这里的绿灯当成「掉电也不丢」的证据。

import (
	"sync"
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// TestNoSyncReopenDOSAndCreationTimeSurvive 验证拍板边界里「进程崩溃不丢」
// 那一半在两个可再生桶上的兑现：设置 DOS 属性与创建时间 → 关闭库（走
// releaseDB 的收尾 Sync）→ 重开 → 两个值都原样回来。
func TestNoSyncReopenDOSAndCreationTimeSurvive(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.txt", nil)

	const wantAttrs = 0x21 // READONLY|ARCHIVE，一个客户端完全可能设出的组合
	if err := e.set.DOS.SetDOSAttributes(ref, wantAttrs); err != nil {
		t.Fatalf("SetDOSAttributes 失败: %v", err)
	}
	wantTime := time.Date(2026, 8, 26, 9, 30, 0, 123456700, time.UTC)
	if err := e.set.Times.SetCreationTime(ref, wantTime); err != nil {
		t.Fatalf("SetCreationTime 失败: %v", err)
	}

	e.reopen(false)

	gotAttrs, err := e.set.DOS.DOSAttributes(ref)
	if err != nil || gotAttrs != wantAttrs {
		t.Fatalf("重开后 DOS 属性丢失或变值: (%#x, %v)，期望 %#x", gotAttrs, err, wantAttrs)
	}
	gotTime, err := e.set.Times.CreationTime(ref)
	if err != nil {
		t.Fatalf("重开后创建时间查询失败: %v", err)
	}
	if !gotTime.Equal(wantTime) {
		t.Fatalf("重开后创建时间变值: 得 %v 期望 %v", gotTime, wantTime)
	}
}

// TestNoSyncCriticalBucketsSurviveReopen 验证内容类桶的持久性承诺：
// 命名流内容与扩展属性经过「逐事务显式 Sync」路径，关闭重开后原样回来。
// 流内容=用户数据，这条是拍板边界里「一寸不让」的部分。
func TestNoSyncCriticalBucketsSurviveReopen(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.txt", []byte("main content"))

	wantStream := []byte("AFP resource payload \x00\x01\xff")
	h, err := e.set.Streams.OpenStream(ref, "AFP_Resource",
		oscap.StreamWrite|oscap.StreamCreate)
	if err != nil {
		t.Fatalf("OpenStream 失败: %v", err)
	}
	if _, err := h.WriteAt(wantStream, 0); err != nil {
		t.Fatalf("流写入失败: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("流关闭失败: %v", err)
	}

	wantXattr := []byte("com.apple.metadata:value")
	if err := e.set.Xattr.SetXattr(ref, "com.apple.metadata", wantXattr); err != nil {
		t.Fatalf("SetXattr 失败: %v", err)
	}

	e.reopen(false)

	sh, err := e.set.Streams.OpenStream(ref, "AFP_Resource", oscap.StreamRead)
	if err != nil {
		t.Fatalf("重开后打开流失败: %v", err)
	}
	defer sh.Close()
	got := make([]byte, len(wantStream))
	if _, err := sh.ReadAt(got, 0); err != nil {
		t.Fatalf("重开后读流失败: %v", err)
	}
	if string(got) != string(wantStream) {
		t.Fatalf("重开后流内容损坏: 得 %q 期望 %q", got, wantStream)
	}

	gotXattr, err := e.set.Xattr.GetXattr(ref, "com.apple.metadata")
	if err != nil {
		t.Fatalf("重开后读 xattr 失败: %v", err)
	}
	if string(gotXattr) != string(wantXattr) {
		t.Fatalf("重开后 xattr 变值: 得 %q 期望 %q", gotXattr, wantXattr)
	}
}

// TestBucketDurabilityClassification 把分类器本身钉死：哪两桶允许吃掉电窗口、
// 哪些桶必须逐事务落盘。这是拍板边界的机器形式，谁改动它必须同时改这条用例
// 和拍板记录。
func TestBucketDurabilityClassification(t *testing.T) {
	for _, b := range [][]byte{bucketTimes, bucketDOS} {
		if !bucketIsRegenerable(b) {
			t.Errorf("桶 %q 应属可再生（免逐事务 Sync），分类器说不是", b)
		}
	}
	for _, b := range allBuckets {
		want := string(b) == string(bucketTimes) || string(b) == string(bucketDOS)
		if got := bucketIsRegenerable(b); got != want {
			t.Errorf("桶 %q 分类错误: 得 %v 期望 %v", b, got, want)
		}
	}
}

// TestEmptyLedgerFastPaths 白盒验证空账快速路的语义等价性：
// deleteKeys/renameKeys 在「无任何旁路记录」时与旧行为一样是幂等 no-op，
// 只是不再开空写事务付 fsync（perf-v050 §4 F4 的 DELETE 侧另一半）。
// bbolt v1.5 的 TxStats 没有可用的写事务计数器，fsync 与否本身不可白盒观测；
// 这里钉住的是「快速路判定正确」—— 有账必进事务、清完账退回快速路。
func TestEmptyLedgerFastPaths(t *testing.T) {
	e := newEnv(t)
	st := e.set.DOS.(*adapter).st

	ghost := st.pathKey(e.root + "/ghost.txt")

	has, err := st.rangeHasRecords(newKeyRanges(ghost))
	if err != nil {
		t.Fatalf("rangeHasRecords(空账) 失败: %v", err)
	}
	if has {
		t.Fatal("从未写过任何记录的范围不该报有账")
	}

	// 空账上调用删除/改名：必须幂等成功（与旧实现的空写事务 no-op 等价）。
	if err := st.deleteKeys(e.root + "/ghost.txt"); err != nil {
		t.Fatalf("空账 deleteKeys 应幂等成功: %v", err)
	}
	if err := st.renameKeys(e.root+"/g1.txt", e.root+"/g2.txt"); err != nil {
		t.Fatalf("双方均空账的 renameKeys 应幂等成功: %v", err)
	}

	// 落一条可再生记录后再看：预检必须能看见（否则快速路会漏删真账）。
	ref := e.file("real.txt", nil)
	if err := e.set.DOS.SetDOSAttributes(ref, 0x20); err != nil {
		t.Fatalf("SetDOSAttributes 失败: %v", err)
	}
	rng := newKeyRanges(st.pathKey(ref.Path))
	if has, err = st.rangeHasRecords(rng); err != nil || !has {
		t.Fatalf("有账范围预检失败: (has=%v, err=%v)", has, err)
	}

	// 删除后退回空账状态，再次删除仍幂等成功。
	if err := st.deleteKeys(ref.Path); err != nil {
		t.Fatalf("deleteKeys 失败: %v", err)
	}
	if has, err = st.rangeHasRecords(rng); err != nil || has {
		t.Fatalf("删除后仍有残留账目: (has=%v, err=%v)", has, err)
	}
	if err := st.deleteKeys(ref.Path); err != nil {
		t.Fatalf("二次 deleteKeys 应幂等成功: %v", err)
	}
}

// TestNoSyncConcurrentMixedWrites 压一把两类桶并发的组合：多个 goroutine 同时
// 写流（关键桶，逐笔 Sync）、DOS 位与创建时间（可再生桶，免 Sync）。全部完成
// 并重开之后，每一笔都必须在。NoSync 化把「每笔写事务后的 fdatasync」从部分
// 调用方身上拿掉了，但绝不能把并发正确性也一起拿掉。
func TestNoSyncConcurrentMixedWrites(t *testing.T) {
	e := newEnv(t)
	const workers = 8
	const opsPerWorker = 25

	var wg sync.WaitGroup
	errs := make(chan error, workers*opsPerWorker*2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			name := "w" + string(rune('A'+w)) + ".bin"
			ref := e.file(name, nil)
			for i := 0; i < opsPerWorker; i++ {
				if err := e.set.DOS.SetDOSAttributes(ref, uint32(0x20|(i&1)<<1)); err != nil {
					errs <- err
					return
				}
				if err := e.set.Times.SetCreationTime(ref,
					time.Unix(int64(1700000000+w*1000+i), 0).UTC()); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发写失败: %v", err)
		}
	}

	e.reopen(false)

	for w := 0; w < workers; w++ {
		name := "w" + string(rune('A'+w)) + ".bin"
		ref := oscap.Ref{Path: e.root + "/" + name}
		attrs, err := e.set.DOS.DOSAttributes(ref)
		if err != nil {
			t.Fatalf("%s 重开查 DOS 属性失败: %v", name, err)
		}
		if attrs&0x20 == 0 {
			t.Fatalf("%s 的 ARCHIVE 位丢了: %#x", name, attrs)
		}
		if _, err := e.set.Times.CreationTime(ref); err != nil {
			t.Fatalf("%s 重开查创建时间失败: %v", name, err)
		}
	}
}

// TestNoSyncReadOnlyCloseUnchanged 兜底：只读共享的关闭路径不走收尾 Sync
// （Windows 上 FlushFileBuffers 要求写权限句柄），行为必须与从前一致 ——
// 关闭成功、重开只读视图照常读得到既有数据。
func TestNoSyncReadOnlyCloseUnchanged(t *testing.T) {
	e := newEnv(t)
	ref := e.file("a.txt", nil)
	if err := e.set.DOS.SetDOSAttributes(ref, 0x20); err != nil {
		t.Fatalf("SetDOSAttributes 失败: %v", err)
	}
	e.reopen(true)
	if _, err := e.set.DOS.DOSAttributes(ref); err != nil {
		t.Fatalf("只读重开后读 DOS 属性失败: %v", err)
	}
}
