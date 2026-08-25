package vfs

// create_btime_test.go —— 新建对象的创建时间必须落进旁路库（B4，bh3 F2）。
//
// 缺陷背景：builtin/times.go 的注释声称「创建时间由 vfs 在真正创建对象时调
// SetCreationTime 落下来」，但全仓唯一的产品调用点在 SET_INFO 路径 ——
// 新建文件的 btime 永不入库，portable 档下客户端看到的是合成值。
//
// 对照的 Samba 行为：新对象的 btime 来自内核（birthtime 或 calc_create_time_stat，
// source3/lib/system.c），且任何一次 file_set_dosmode 都会把当前 btime 一并
// 钉进 xattr —— 「新对象有创建时间」是默认保证。
//
// 跳过条件与现有 caps 探测一致：auto 档下 CapCreationTime 已被矩阵交给
// native 时（内核自己维护 birthtime 且读得到），旁路记录是多余的，
// 不写。这条纪律用注入 Matrix 的受控 Provider 钉住。

import (
	"testing"
	"time"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// btimeRecordIs 测 ref 的旁路 btime 是否存在且落在 [now-Δ, now+Δ] 内。
func btimeRecordIs(t *testing.T, fs *LocalFS, rel string) {
	t.Helper()
	got, err := fs.caps.Times().CreationTime(hostRef(fs, rel))
	if err != nil {
		t.Fatalf("%s: 创建后旁路无 btime 记录（%v）—— 注释声称的行为不存在", rel, err)
	}
	if d := got.Sub(time.Now()); d > time.Minute || d < -10*time.Minute {
		t.Fatalf("%s: btime = %v，离现在太远", rel, got)
	}
}

// TestCreateFileWritesBtime 新建文件后 btime 立即可查。
func TestCreateFileWritesBtime(t *testing.T) {
	fs := metaFS(t)
	h, action, err := fs.Open(&OpenRequest{
		Path:        "new.txt",
		Flags:       OpenWrite,
		Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionCreated {
		t.Fatalf("action = %v", action)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	btimeRecordIs(t, fs, "new.txt")
}

// TestCreateDirWritesBtime 目录同理（CREATE 建目录与 Mkdir 两条路都要落）。
func TestCreateDirWritesBtime(t *testing.T) {
	fs := metaFS(t)

	h, _, err := fs.Open(&OpenRequest{
		Path:        "dirA",
		Flags:       OpenDirectory | OpenWrite,
		Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	btimeRecordIs(t, fs, "dirA")

	if err := fs.Mkdir("dirB", 0); err != nil {
		t.Fatal(err)
	}
	btimeRecordIs(t, fs, "dirB")
}

// TestOverwriteKeepsExistingBtime 截断式覆盖保留原对象的 btime：
// 对象活着（inode 未换），Samba/Windows 都不改创建时间。
func TestOverwriteKeepsExistingBtime(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "old")
	plantMeta(t, fs, "f.txt", metaT1)

	h, _, err := fs.Open(&OpenRequest{
		Path:        "f.txt",
		Flags:       OpenWrite,
		Disposition: TruncateExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := fs.caps.Times().CreationTime(hostRef(fs, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(metaT1) {
		t.Fatalf("覆盖不应改写 btime: got %v, want %v", got, metaT1)
	}
}

// TestSupersedeRefreshesBtime SUPERSEDE 是删除重建：
// 前任的 btime 作废，新对象从当下起算。
func TestSupersedeRefreshesBtime(t *testing.T) {
	fs := metaFS(t)
	writeFile(t, fs, "f.txt", "old")
	plantMeta(t, fs, "f.txt", metaT1)

	h, action, err := fs.Open(&OpenRequest{
		Path:        "f.txt",
		Flags:       OpenWrite,
		Disposition: Supersede,
	})
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionSuperseded {
		t.Fatalf("action = %v", action)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := fs.caps.Times().CreationTime(hostRef(fs, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Equal(metaT1) {
		t.Fatalf("SUPERSEDE 后仍带前任的创建时间 %v", got)
	}
}

// ------------------------------------------------------------ native 跳过纪律

// nativeTimesProbe 把矩阵钉成「CapCreationTime 走 native」，其余转发。
// 用途：证明 native 档下新建**不会**写旁路 btime（内核 birthtime 已是
// 真值，旁路记录纯属浪费 —— 与 caps 探测逻辑一致）。
type nativeTimesProbe struct {
	oscap.Provider
	inner oscap.CreationTime
	sets  int
}

var _ oscap.CreationTime = (*nativeTimesProbe)(nil)

func (p *nativeTimesProbe) Times() oscap.CreationTime { return p }

func (p *nativeTimesProbe) CreationTime(ref oscap.Ref) (time.Time, error) {
	return p.inner.CreationTime(ref)
}

func (p *nativeTimesProbe) SetCreationTime(ref oscap.Ref, t time.Time) error {
	p.sets++
	return p.inner.SetCreationTime(ref, t)
}

func (p *nativeTimesProbe) Matrix() oscap.Matrix {
	return oscap.NewMatrix(oscap.ModeAuto, map[oscap.Capability]oscap.Kind{
		oscap.CapCreationTime: oscap.KindNative,
	})
}

// TestNativeCreationTimeSkipsBypass 矩阵说 Times 是 native 时，
// 新建不产生旁路写入。
func TestNativeCreationTimeSkipsBypass(t *testing.T) {
	root := t.TempDir()
	base := newPortableProvider(t, root)
	probe := &nativeTimesProbe{Provider: base, inner: base.Times()}
	fs, err := NewLocalFS(LocalConfig{Root: root, CaseInsensitive: true, Caps: probe})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	h, _, err := fs.Open(&OpenRequest{
		Path:        "n.txt",
		Flags:       OpenWrite,
		Disposition: CreateNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	if probe.sets != 0 {
		t.Fatalf("native Times 下新建仍写了 %d 次旁路 btime", probe.sets)
	}
}
