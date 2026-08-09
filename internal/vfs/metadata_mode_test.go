package vfs

// metadata_mode_test.go —— 旁路元数据（uid/gid/mode）的 round-trip 回归。
//
// 这两个 bug 只在**接了 MetadataStore 的平台**（Windows）上触发，
// 但根本不需要真 Windows 就能复现：`LocalFS.meta` 是个接口，
// 塞一个假实现进去，Linux 上照样能打到同一条代码路径。
//
// 感谢 win-vfs 从 Windows 视角定位出这两条。

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeMetaStore 是 MetadataStore 的内存实现，只为测试用。
// 行为刻意做得和 bbolt 版一致：Get 未命中返回零值 + false。
type fakeMetaStore struct {
	mu   sync.Mutex
	m    map[string]Metadata
	puts int // 记录写入次数，用于证明「根本没调用 Put」这类缺陷
}

func newFakeMetaStore() *fakeMetaStore {
	return &fakeMetaStore{m: make(map[string]Metadata)}
}

func (f *fakeMetaStore) Get(key string) (Metadata, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	md, ok := f.m[key]
	return md, ok
}

func (f *fakeMetaStore) Put(key string, md Metadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = md
	f.puts++
	return nil
}

func (f *fakeMetaStore) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
	return nil
}

func (f *fakeMetaStore) Rename(oldKey, newKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if md, ok := f.m[oldKey]; ok {
		delete(f.m, oldKey)
		f.m[newKey] = md
	}
	return nil
}

func (f *fakeMetaStore) Close() error { return nil }

func (f *fakeMetaStore) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

// newMetaFS 建一个挂了假旁路存储的共享，模拟 Windows 平台的装配结果。
func newMetaFS(t *testing.T) (*LocalFS, *fakeMetaStore) {
	t.Helper()
	fs := newTestFS(t, false)
	store := newFakeMetaStore()
	fs.meta = store
	return fs, store
}

// ------------------------------------------------- Bug B-1：Mode 被回读成 0

// TestApplyMetadataFallsBackWhenModeUnset 是**核心回归**。
//
// 客户端只设过 UID（没设 mode）时，库里的记录是 {uid, gid, 0}。
// 旧实现命中记录后 `a.UID, a.GID, a.Mode = md.UID, md.GID, md.Mode` 然后直接
// return，而 `if a.Mode == 0` 的兜底在够不着的 else 分支里 —— 于是回读的
// Mode 是 0，客户端看到一个 000 权限的对象，Time Machine 直接判定
// 备份目标不可用。
func TestApplyMetadataFallsBackWhenModeUnset(t *testing.T) {
	fs, store := newMetaFS(t)
	writeFile(t, fs, "f.txt", "x")
	if err := store.Put("f.txt", Metadata{UID: 501, GID: 20}); err != nil {
		t.Fatal(err)
	}

	a, err := fs.Stat("f.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if a.UID != 501 || a.GID != 20 {
		t.Errorf("uid/gid = %d/%d, want 501/20（记录里的值没被采信）", a.UID, a.GID)
	}
	if a.Mode == 0 {
		t.Fatal("Mode 被回读成 0：客户端会看到 000 权限的对象，TM 直接拒绝备份")
	}
	if want := uint32(fs.cfg.FileMode.Perm()); a.Mode != want {
		t.Errorf("Mode = %#o, want %#o（配置里的默认文件权限）", a.Mode, want)
	}
}

// TestApplyMetadataDirModeFallback：目录要兜 DirMode，不是 FileMode。
func TestApplyMetadataDirModeFallback(t *testing.T) {
	fs, store := newMetaFS(t)
	if err := os.Mkdir(filepath.Join(fs.Root(), "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("d", Metadata{UID: 7}); err != nil {
		t.Fatal(err)
	}

	a, err := fs.Stat("d")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if want := uint32(fs.cfg.DirMode.Perm()); a.Mode != want {
		t.Errorf("目录 Mode = %#o, want %#o", a.Mode, want)
	}
}

// TestApplyMetadataKeepsRecordedMode 是**反向对照**：记录里有真实 mode 时
// 必须原样返回，不能被兜底覆盖掉。
//
// 没有这一条，上面两个测试可以靠「无脑忽略记录里的 Mode」作弊通过。
func TestApplyMetadataKeepsRecordedMode(t *testing.T) {
	fs, store := newMetaFS(t)
	writeFile(t, fs, "f.txt", "x")
	if err := store.Put("f.txt", Metadata{UID: 1, GID: 2, Mode: 0o750}); err != nil {
		t.Fatal(err)
	}

	a, err := fs.Stat("f.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if a.Mode != 0o750 {
		t.Errorf("Mode = %#o, want 0750（记录里的值被兜底覆盖了）", a.Mode)
	}
}

// TestApplyMetadataNoRecord：没有记录时走配置默认，行为不变。
func TestApplyMetadataNoRecord(t *testing.T) {
	fs, _ := newMetaFS(t)
	writeFile(t, fs, "f.txt", "x")

	a, err := fs.Stat("f.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if a.UID != fs.cfg.UID || a.GID != fs.cfg.GID {
		t.Errorf("uid/gid = %d/%d, want %d/%d", a.UID, a.GID, fs.cfg.UID, fs.cfg.GID)
	}
	if a.Mode == 0 {
		t.Error("Mode = 0")
	}
}

// ------------------------------------------- Bug B-2：纯 mode 变更不落库

// TestSetAttrModeOnlyPersists 是**核心回归**。
//
// 客户端单独发一个 AttrMode 时，旧实现只调 os.Chmod（Windows 上只翻
// READONLY 位），`mask&(AttrUID|AttrGID) != 0` 不成立所以 meta.Put 根本
// 没执行 → 改动丢失，回读是旧值。
func TestSetAttrModeOnlyPersists(t *testing.T) {
	fs, store := newMetaFS(t)
	writeFile(t, fs, "f.txt", "x")
	if err := store.Put("f.txt", Metadata{UID: 501, GID: 20, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	before := store.putCount()

	h, _, err := fs.Open(&OpenRequest{
		Path:        "f.txt",
		Flags:       OpenRead | OpenWrite,
		Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	if err := h.SetAttr(&Attr{Mode: 0o700}, AttrMode); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	if store.putCount() == before {
		t.Fatal("meta.Put 根本没被调用 —— 纯 mode 变更没落库")
	}

	md, ok := store.Get("f.txt")
	if !ok {
		t.Fatal("记录不见了")
	}
	if md.Mode != 0o700 {
		t.Errorf("库里 Mode = %#o, want 0700", md.Mode)
	}
	// 属主必须原样保留，不能被这次纯 mode 变更清成 0。
	if md.UID != 501 || md.GID != 20 {
		t.Errorf("uid/gid = %d/%d, want 501/20（纯 mode 变更把属主冲掉了）", md.UID, md.GID)
	}

	// round-trip：回读要拿到新值。
	a, err := fs.Stat("f.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if a.Mode != 0o700 {
		t.Errorf("回读 Mode = %#o, want 0700", a.Mode)
	}
}

// TestSetAttrUIDOnlyStillPersists 是**反向对照**：只改 UID 的老路径没被改坏。
func TestSetAttrUIDOnlyStillPersists(t *testing.T) {
	fs, store := newMetaFS(t)
	writeFile(t, fs, "f.txt", "x")
	if err := store.Put("f.txt", Metadata{UID: 1, GID: 2, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}

	h, _, err := fs.Open(&OpenRequest{
		Path:        "f.txt",
		Flags:       OpenRead | OpenWrite,
		Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	if err := h.SetAttr(&Attr{UID: 99}, AttrUID); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	md, _ := store.Get("f.txt")
	if md.UID != 99 {
		t.Errorf("UID = %d, want 99", md.UID)
	}
	if md.Mode != 0o644 {
		t.Errorf("Mode = %#o, want 0644（只改 UID 不该动 mode）", md.Mode)
	}
}

// TestSetAttrNoMetaStoreIsNoop：Linux/macOS（meta == nil）上不能 panic。
func TestSetAttrNoMetaStoreIsNoop(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f.txt", "x")

	h, _, err := fs.Open(&OpenRequest{
		Path:        "f.txt",
		Flags:       OpenRead | OpenWrite,
		Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	if err := h.SetAttr(&Attr{Mode: 0o700}, AttrMode); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	// Linux 上 os.Chmod 是真的，回读应当是新值。
	a, err := fs.Stat("f.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if a.Mode != 0o700 {
		t.Errorf("Mode = %#o, want 0700", a.Mode)
	}
}
