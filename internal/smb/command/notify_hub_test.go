package command

import (
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// 本文件钉的是目录变更事件中心（notify_hub.go）—— CHANGE_NOTIFY 的服务端侧。
//
// 变异自检方向：
//   - 把 matches 里 watchTree 的判断删掉 → TestWatchTreeScopesToSubtree 变红；
//   - 把 FileName 的反斜杠替换删掉 → 同上（Name 会带上正斜杠）；
//   - 把 filter 交集判断删掉 → TestFilterNarrowsDelivery 变红；
//   - 把 cleanup 的 signal 去掉 → TestNotifyCleanupOnHandleClose 超时变红；
//   - 把 notifyRenamed 的第二条投递删掉 → TestRenameYieldsTwoEntries 变红。

// notifyWait 起一个等待者并返回结果通道，避免测试自己写超时样板。
func notifyWait(w *notifyWatch, stop <-chan struct{}) <-chan [2]interface{} {
	out := make(chan [2]interface{}, 1)
	go func() {
		entries, ended := w.wait(stop)
		out <- [2]interface{}{entries, ended}
	}()
	return out
}

func TestNotifyDirectChildDelivered(t *testing.T) {
	var h notifyHub
	o := &Open{}
	w := newNotifyWatch("docs", false, wire.NotifyChangeFileName, o)
	h.add(w)

	res := notifyWait(w, nil)
	h.notifyAdded("docs/a.txt", false)

	select {
	case r := <-res:
		entries := r[0].([]wire.NotifyEntry)
		if len(entries) != 1 {
			t.Fatalf("应投递 1 条，实际 %d", len(entries))
		}
		if entries[0].Action != wire.FileActionAdded {
			t.Fatalf("Action 应为 ADDED，实际 %v", entries[0].Action)
		}
		// 直接子项的 Name 就是对象名，不带路径。
		if entries[0].Name != "a.txt" {
			t.Fatalf("Name 应为 a.txt，实际 %q", entries[0].Name)
		}
	case <-time.After(time.Second):
		t.Fatal("等待投递超时")
	}
}

// TestWatchTreeScopesToSubtree：SMB2_WATCH_TREE 决定是否递归。
// 未置位时只有直接子项命中；置位时子树命中且 Name 是相对被监视目录的路径。
func TestWatchTreeScopesToSubtree(t *testing.T) {
	var h notifyHub
	o := &Open{}
	shallow := newNotifyWatch("a", false, wire.NotifyChangeFileName, o)
	deep := newNotifyWatch("a", true, wire.NotifyChangeFileName, o)
	h.add(shallow)
	h.add(deep)

	shallowRes := notifyWait(shallow, nil)
	deepRes := notifyWait(deep, nil)
	h.notifyAdded("a/b/c.txt", false)

	select {
	case r := <-deepRes:
		entries := r[0].([]wire.NotifyEntry)
		if len(entries) != 1 {
			t.Fatalf("watchTree 订阅应命中，实际 %d 条", len(entries))
		}
		// 线格式用反斜杠（MS-FSCC §2.7.1），且是**相对被监视目录**的路径。
		if entries[0].Name != `b\c.txt` {
			t.Fatalf("Name 应为 b\\c.txt，实际 %q", entries[0].Name)
		}
	case <-time.After(time.Second):
		t.Fatal("watchTree 订阅未被唤醒")
	}

	select {
	case <-shallowRes:
		t.Fatal("未置 WATCH_TREE 的订阅不应收到子树事件")
	case <-time.After(60 * time.Millisecond):
	}
}

// TestFilterNarrowsDelivery：CompletionFilter 是"只关心这些位"的与判定。
func TestFilterNarrowsDelivery(t *testing.T) {
	var h notifyHub
	o := &Open{}
	wantWrites := newNotifyWatch("", false, wire.NotifyChangeLastWrite, o)
	wantNames := newNotifyWatch("", false, wire.NotifyChangeFileName, o)
	h.add(wantWrites)
	h.add(wantNames)

	writesRes := notifyWait(wantWrites, nil)
	namesRes := notifyWait(wantNames, nil)
	h.notifyModified("f.txt", wire.NotifyChangeSize|wire.NotifyChangeLastWrite)

	select {
	case <-writesRes:
	case <-time.After(time.Second):
		t.Fatal("关心 LAST_WRITE 的订阅未被唤醒")
	}
	select {
	case <-namesRes:
		t.Fatal("只关心 FILE_NAME 的订阅不该被 MODIFIED 唤醒")
	case <-time.After(60 * time.Millisecond):
	}
}

// TestNotifyDirUsesDirNameBit：目录用 DIR_NAME，文件用 FILE_NAME。
func TestNotifyDirUsesDirNameBit(t *testing.T) {
	var h notifyHub
	o := &Open{}
	dirs := newNotifyWatch("", true, wire.NotifyChangeDirName, o)
	files := newNotifyWatch("", true, wire.NotifyChangeFileName, o)
	h.add(dirs)
	h.add(files)

	dirRes := notifyWait(dirs, nil)
	fileRes := notifyWait(files, nil)
	h.notifyAdded("sub", true)

	select {
	case <-dirRes:
	case <-time.After(time.Second):
		t.Fatal("关心 DIR_NAME 的订阅未被唤醒")
	}
	select {
	case <-fileRes:
		t.Fatal("只关心 FILE_NAME 的订阅不该被目录创建唤醒")
	case <-time.After(60 * time.Millisecond):
	}
}

// TestRenameYieldsTwoEntries：改名按规范给两条条目，顺序固定为
// RENAMED_OLD_NAME 后 RENAMED_NEW_NAME。
func TestRenameYieldsTwoEntries(t *testing.T) {
	var h notifyHub
	o := &Open{}
	w := newNotifyWatch("", true, wire.NotifyChangeFileName, o)
	h.add(w)

	res := notifyWait(w, nil)
	h.notifyRenamed("old.txt", "new.txt", false)

	select {
	case r := <-res:
		entries := r[0].([]wire.NotifyEntry)
		if len(entries) != 2 {
			t.Fatalf("改名应投递 2 条，实际 %d", len(entries))
		}
		if entries[0].Action != wire.FileActionRenamedOldName || entries[0].Name != "old.txt" {
			t.Fatalf("第一条应为 OLD_NAME/old.txt，实际 %v/%q", entries[0].Action, entries[0].Name)
		}
		if entries[1].Action != wire.FileActionRenamedNewName || entries[1].Name != "new.txt" {
			t.Fatalf("第二条应为 NEW_NAME/new.txt，实际 %v/%q", entries[1].Action, entries[1].Name)
		}
	case <-time.After(time.Second):
		t.Fatal("等待投递超时")
	}
}

// TestNotifyCleanupOnHandleClose：目录句柄关闭时其上未决的订阅必须以
// cleanup 收场，让等待者回 STATUS_NOTIFY_CLEANUP —— 不是继续挂着等超时。
func TestNotifyCleanupOnHandleClose(t *testing.T) {
	var h notifyHub
	o := &Open{}
	w := newNotifyWatch("d", false, wire.NotifyChangeFileName, o)
	h.add(w)

	res := notifyWait(w, nil)
	h.cleanup(o)

	select {
	case r := <-res:
		if r[0].([]wire.NotifyEntry) != nil {
			t.Fatal("cleanup 不应带回事件")
		}
		if !r[1].(bool) {
			t.Fatal("cleanup 必须让 ended 为 true")
		}
		if !w.cleanupTriggered() {
			t.Fatal("cleanupTriggered 必须为 true（收尾据此回 NOTIFY_CLEANUP）")
		}
	case <-time.After(time.Second):
		t.Fatal("句柄关闭后等待者未被唤醒")
	}
	if got := h.count(); got != 0 {
		t.Fatalf("cleanup 后订阅应已摘除，实际剩 %d", got)
	}
}

// TestNotifyRemoveWakesWaiterSilently：摘除订阅时等待者必须退出，
// 但**不带** cleanup 标志 —— 那种情形不该回 NOTIFY_CLEANUP。
func TestNotifyRemoveWakesWaiterSilently(t *testing.T) {
	var h notifyHub
	o := &Open{}
	w := newNotifyWatch("d", false, wire.NotifyChangeFileName, o)
	h.add(w)

	res := notifyWait(w, nil)
	h.remove(w)

	select {
	case r := <-res:
		if r[1].(bool) {
			t.Fatal("摘除订阅不应被判成 cleanup")
		}
	case <-time.After(time.Second):
		t.Fatal("摘除订阅后等待者未被唤醒")
	}
}

// TestNotifyOverflowEndsWithEnumDir：队列满时不再攒，等待者以
// "无事件 + ended" 收场，收尾据此回 STATUS_NOTIFY_ENUM_DIR。
func TestNotifyOverflowEndsWithEnumDir(t *testing.T) {
	var h notifyHub
	o := &Open{}
	w := newNotifyWatch("", false, wire.NotifyChangeFileName, o)
	h.add(w)

	// 不派等待者，直接把队列灌到上限再灌一个。
	for i := 0; i < notifyQueueMax; i++ {
		h.notifyAdded("f", false)
	}
	if len(w.events) != notifyQueueMax {
		t.Fatalf("队列应攒满 %d 条，实际 %d", notifyQueueMax, len(w.events))
	}
	h.notifyAdded("overflow", false)
	if !w.overflow {
		t.Fatal("超过上限应置 overflow")
	}

	// 现在派等待者：它应当立刻以"无事件 + ended"返回。
	res := notifyWait(w, nil)
	select {
	case r := <-res:
		if !r[1].(bool) {
			t.Fatal("溢出后等待者应以 ended=true 返回（收尾回 NOTIFY_ENUM_DIR）")
		}
	case <-time.After(time.Second):
		t.Fatal("溢出后等待者未被唤醒")
	}
}

// TestNotifyNoWatchersIsCheap：没有订阅时投递必须立即返回。
// 这是 CHANGE_NOTIFY 未启用时的常态路径，每次文件操作都会走一遍。
func TestNotifyNoWatchersIsCheap(t *testing.T) {
	var h notifyHub
	h.notifyAdded("a", false)
	h.notifyRemoved("b", true)
	h.notifyRenamed("c", "d", false)
	h.notifyModified("e", wire.NotifyChangeSize)
	// cleanup 也会走空表快速路（它是每条 CLOSE 的必经之路）。
	h.cleanup(&Open{})
	if got := h.count(); got != 0 {
		t.Fatalf("不应凭空产生订阅，实际 %d", got)
	}
}

// TestNilNotifyHubIsSafe：树不存在（IPC$ 等）时记账必须安全空转。
func TestNilNotifyHubIsSafe(t *testing.T) {
	var h *notifyHub
	h.notifyAdded("a", false)
	h.notifyRemoved("b", true)
	h.notifyRenamed("c", "d", false)
	h.notifyModified("e", wire.NotifyChangeSize)

	// 空 Context 也走 nil 分支。
	ctx := &Context{}
	if ctx.notifyHub() != nil {
		t.Fatal("无树时 notifyHub() 应返回 nil")
	}
	if (&Open{}).notifyHub() != nil {
		t.Fatal("无树时 Open.notifyHub() 应返回 nil")
	}
}

// TestSplitNotifyPath 钉住路径拆分的边界：共享根的对象目录部分是 ""。
func TestSplitNotifyPath(t *testing.T) {
	cases := []struct {
		in       string
		dirWant  string
		nameWant string
	}{
		{"a.txt", "", "a.txt"},
		{"docs/a.txt", "docs", "a.txt"},
		{"a/b/c", "a/b", "c"},
		{"/leading/slash.txt", "leading", "slash.txt"},
	}
	for _, c := range cases {
		dir, name := splitNotifyPath(c.in)
		if dir != c.dirWant || name != c.nameWant {
			t.Errorf("splitNotifyPath(%q) = (%q,%q)，期望 (%q,%q)",
				c.in, dir, name, c.dirWant, c.nameWant)
		}
	}
}

// TestIsUnderNotify：空 parent（共享根）包含一切。
func TestIsUnderNotify(t *testing.T) {
	if !isUnderNotify("a/b", "") {
		t.Fatal("共享根应包含一切")
	}
	if !isUnderNotify("a/b", "a") {
		t.Fatal("a/b 应在 a 之下")
	}
	if isUnderNotify("ab/c", "a") {
		t.Fatal("ab/c 不应被判成在 a 之下（前缀比较必须按路径分段）")
	}
}

// TestRenameDeliveredAtomically 钉住"一次改名必须整体可见"。
//
// 早先 notifyRenamed 是调两次 deliver（逐个追加 + 逐个 signal），
// 于是等待者可能在**只有 OLD_NAME 落地**时就被唤醒 —— 它拿到的改名是半条，
// 客户端据此把缓存里的条目改成一个已经不存在的路径。
// 修复方式：deliver 改成接收一批事件，整批在同一把 w.mu 下追加完再发一次信号。
//
// 为什么用循环：这个 bug 只在"等待者恰好在两次投递之间被唤醒"时暴露，
// 单次跑的时序是随机的。循环 200 次把各种交错都覆盖到 —— 修复前约每几十次
// 失败一次，修复后稳定通过。
func TestRenameDeliveredAtomically(t *testing.T) {
	const rounds = 200
	for i := 0; i < rounds; i++ {
		var h notifyHub
		o := &Open{}
		w := newNotifyWatch("", true, wire.NotifyChangeFileName, o)
		h.add(w)

		res := notifyWait(w, nil)
		h.notifyRenamed("old.txt", "new.txt", false)

		r := <-res
		entries := r[0].([]wire.NotifyEntry)
		if len(entries) != 2 {
			t.Fatalf("第 %d 轮：一次改名必须给出 2 条条目（整体可见），实际 %d 条：%+v",
				i, len(entries), entries)
		}
		if entries[0].Action != wire.FileActionRenamedOldName || entries[0].Name != "old.txt" {
			t.Fatalf("第 %d 轮：第一条应为 OLD_NAME/old.txt，实际 %v/%q",
				i, entries[0].Action, entries[0].Name)
		}
		if entries[1].Action != wire.FileActionRenamedNewName || entries[1].Name != "new.txt" {
			t.Fatalf("第 %d 轮：第二条应为 NEW_NAME/new.txt，实际 %v/%q",
				i, entries[1].Action, entries[1].Name)
		}
	}
}
