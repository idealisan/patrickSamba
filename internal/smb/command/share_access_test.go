package command

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
	"github.com/idealisan/patrickSamba/internal/vfs"
)

// share_access_test.go —— 共享模式（ShareAccess）冲突判定。
//
// 判定是**双向**的（MS-FSA §2.1.5.1.2），下面的表按方向分组，
// 每一组都同时给出「应当拒绝」与「应当放行」的用例：
// 只测拒绝会漏掉过度拒绝，那种 bug 表现为客户端莫名其妙打不开文件。

const (
	shareNone = wire.ShareAccess(0)
	shareAll  = wire.ShareRead | wire.ShareWrite | wire.ShareDelete
)

// ---------------------------------------------------------------------------
// 1. 纯判定逻辑：DesiredAccess × ShareAccess 组合矩阵
// ---------------------------------------------------------------------------

type shareCase struct {
	name string
	// dir 说明这条用例考察哪个方向，仅用于失败信息。
	dir string

	existingAccess wire.Access
	existingShare  wire.ShareAccess
	newAccess      wire.Access
	newShare       wire.ShareAccess

	wantConflict bool
}

// shareMatrix 是双向判定的组合矩阵。
//
// 「方向一」= 新请求的 DesiredAccess 撞上已存在句柄的 ShareAccess；
// 「方向二」= 已存在句柄的 DesiredAccess 撞上新请求的 ShareAccess。
var shareMatrix = []shareCase{
	// ---- 方向一：新的 DesiredAccess 被已存在句柄的 ShareAccess 拒绝 ----
	{
		name: "方向一/读撞上不共享读", dir: "1",
		existingAccess: wire.FileReadData, existingShare: shareNone,
		newAccess: wire.FileReadData, newShare: shareAll,
		wantConflict: true,
	},
	{
		name: "方向一/写撞上只共享读", dir: "1",
		existingAccess: wire.FileReadData, existingShare: wire.ShareRead,
		newAccess: wire.FileWriteData, newShare: shareAll,
		wantConflict: true,
	},
	{
		name: "方向一/追加写也算写", dir: "1",
		existingAccess: wire.FileReadData, existingShare: wire.ShareRead,
		newAccess: wire.FileAppendData, newShare: shareAll,
		wantConflict: true,
	},
	{
		name: "方向一/删除撞上不共享删除", dir: "1",
		existingAccess: wire.FileReadData, existingShare: wire.ShareRead | wire.ShareWrite,
		newAccess: wire.Delete, newShare: shareAll,
		wantConflict: true,
	},
	{
		name: "方向一/执行按读判定", dir: "1",
		existingAccess: wire.FileReadData, existingShare: shareNone,
		newAccess: wire.FileExecute, newShare: shareAll,
		wantConflict: true,
	},

	// ---- 方向二：新的 ShareAccess 拒绝了已存在句柄正在用的 DesiredAccess ----
	// 这一组就是「只实现一半」时会全部变绿的用例。
	{
		name: "方向二/已有读句柄，新请求不共享读", dir: "2",
		existingAccess: wire.FileReadData, existingShare: shareAll,
		newAccess: wire.FileReadData, newShare: shareNone,
		wantConflict: true,
	},
	{
		name: "方向二/已有写句柄，新请求不共享写", dir: "2",
		existingAccess: wire.FileWriteData, existingShare: shareAll,
		newAccess: wire.FileReadData, newShare: wire.ShareRead | wire.ShareDelete,
		wantConflict: true,
	},
	{
		name: "方向二/已有追加句柄，新请求不共享写", dir: "2",
		existingAccess: wire.FileAppendData, existingShare: shareAll,
		newAccess: wire.FileReadData, newShare: wire.ShareRead | wire.ShareDelete,
		wantConflict: true,
	},
	{
		name: "方向二/已有删除句柄，新请求不共享删除", dir: "2",
		existingAccess: wire.Delete, existingShare: shareAll,
		newAccess: wire.FileReadData, newShare: wire.ShareRead | wire.ShareWrite,
		wantConflict: true,
	},
	{
		name: "方向二/已有执行句柄，新请求不共享读", dir: "2",
		existingAccess: wire.FileExecute, existingShare: shareAll,
		newAccess: wire.FileWriteData, newShare: wire.ShareWrite | wire.ShareDelete,
		wantConflict: true,
	},

	// ---- 放行：不加这一组的话，「一律返回冲突」也能让上面全绿 ----
	{
		name: "放行/双方都全共享", dir: "-",
		existingAccess: wire.FileReadData | wire.FileWriteData | wire.Delete, existingShare: shareAll,
		newAccess: wire.FileReadData | wire.FileWriteData | wire.Delete, newShare: shareAll,
	},
	{
		name: "放行/共享读之下两个读句柄共存", dir: "-",
		existingAccess: wire.FileReadData, existingShare: wire.ShareRead,
		newAccess: wire.FileReadData, newShare: wire.ShareRead,
	},
	{
		name: "放行/双方都不要写，因此不共享写无所谓", dir: "-",
		existingAccess: wire.FileReadData, existingShare: wire.ShareRead,
		newAccess: wire.FileReadData, newShare: wire.ShareRead | wire.ShareDelete,
		// 注意：两边都没有 DELETE 访问位，所以 existingShare 缺 ShareDelete
		// 也不构成冲突 —— 共享位只在对方真的要那个访问权时才起作用。
	},
	{
		name: "放行/已有句柄只读属性，不参与判定", dir: "-",
		existingAccess: wire.FileReadAttributes, existingShare: shareNone,
		newAccess: wire.FileReadData | wire.FileWriteData | wire.Delete, newShare: shareNone,
	},
	{
		name: "放行/新请求只读属性，不参与判定", dir: "-",
		existingAccess: wire.FileReadData | wire.FileWriteData | wire.Delete, existingShare: shareNone,
		newAccess: wire.FileReadAttributes | wire.FileWriteAttributes, newShare: shareNone,
	},
	{
		name: "放行/写句柄与写句柄，双方都共享写", dir: "-",
		existingAccess: wire.FileWriteData, existingShare: wire.ShareWrite,
		newAccess: wire.FileWriteData, newShare: wire.ShareWrite,
	},
}

// TestShareConflictMatrix 覆盖双向判定的组合矩阵。
func TestShareConflictMatrix(t *testing.T) {
	for _, tc := range shareMatrix {
		t.Run(tc.name, func(t *testing.T) {
			e := shareModeEntry{access: tc.existingAccess, share: tc.existingShare}
			got := shareConflict(tc.newAccess, tc.newShare, e)
			if got != tc.wantConflict {
				t.Fatalf("方向%s：shareConflict(access=%#x, share=%#x | 已有 access=%#x share=%#x) = %v, 期望 %v",
					tc.dir, uint32(tc.newAccess), uint32(tc.newShare),
					uint32(tc.existingAccess), uint32(tc.existingShare), got, tc.wantConflict)
			}
		})
	}
}

// TestShareConflictSymmetric 钉住判定的对称性。
//
// 冲突关系天生对称：A 与 B 冲突 ⟺ B 与 A 冲突。只实现单向判定时这条性质
// 会当场破裂（方向二的用例反过来看就是方向一），所以它是一条独立于
// 期望值表的交叉校验 —— 表里的期望值抄错了也拦得住。
func TestShareConflictSymmetric(t *testing.T) {
	for _, tc := range shareMatrix {
		t.Run(tc.name, func(t *testing.T) {
			fwd := shareConflict(tc.newAccess, tc.newShare,
				shareModeEntry{access: tc.existingAccess, share: tc.existingShare})
			rev := shareConflict(tc.existingAccess, tc.existingShare,
				shareModeEntry{access: tc.newAccess, share: tc.newShare})
			if fwd != rev {
				t.Fatalf("判定不对称：正向 %v，反向 %v —— 说明只做了单向", fwd, rev)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. 表行为：登记、摘除、按文件身份分档
// ---------------------------------------------------------------------------

// openFor 造一个只带判定所需字段的句柄。
func openFor(access wire.Access, share wire.ShareAccess) *Open {
	return &Open{GrantedAccess: access, ShareAccess: share}
}

func TestShareModeTableAddRemove(t *testing.T) {
	var tbl shareModeTable
	key := shareModeKey{fileID: 7}

	a := openFor(wire.FileReadData, shareNone)
	if st := tbl.add(key, a); st != status.Success {
		t.Fatalf("首个句柄登记应成功，实际 %v", st)
	}
	if n := tbl.count(); n != 1 {
		t.Fatalf("登记数 = %d, 期望 1", n)
	}

	b := openFor(wire.FileReadData, shareAll)
	if st := tbl.add(key, b); st != status.SharingViolation {
		t.Fatalf("独占句柄在位时应回 STATUS_SHARING_VIOLATION，实际 %v", st)
	}
	if n := tbl.count(); n != 1 {
		t.Fatalf("被拒的句柄不得留下登记：登记数 = %d, 期望 1", n)
	}

	// 换一个文件身份：互不影响。
	if st := tbl.add(shareModeKey{fileID: 8}, openFor(wire.FileReadData, shareNone)); st != status.Success {
		t.Fatalf("另一个文件上的同样请求不应受影响，实际 %v", st)
	}
	// 同一个文件的另一个流：也是独立的一档（Samba 给每个流合成独立 inode）。
	if st := tbl.add(shareModeKey{fileID: 7, stream: "AFP_Resource"},
		openFor(wire.FileReadData, shareNone)); st != status.Success {
		t.Fatalf("同一文件的 alternate data stream 应独立判定，实际 %v", st)
	}

	tbl.remove(a)
	if st := tbl.add(key, b); st != status.Success {
		t.Fatalf("独占句柄摘除后应放行，实际 %v", st)
	}
}

// TestShareModeAttrOnlyNotRegistered：纯属性探测既不冲突也不进表。
//
// Explorer / Finder 会为目录里的每个文件做一次这种 open，进表的话表会被
// 撑爆，而且它们不占任何共享位，登记了也没有意义。
func TestShareModeAttrOnlyNotRegistered(t *testing.T) {
	var tbl shareModeTable
	key := shareModeKey{fileID: 1}

	o := openFor(wire.FileReadAttributes|wire.Synchronize, shareNone)
	if st := tbl.add(key, o); st != status.Success {
		t.Fatalf("属性探测不应被拒，实际 %v", st)
	}
	if n := tbl.count(); n != 0 {
		t.Fatalf("属性探测不应进表：登记数 = %d, 期望 0", n)
	}
	if o.shareModeOn {
		t.Error("属性探测句柄不该被标记为已登记")
	}
	// 它也不该挡住任何人。
	if st := tbl.add(key, openFor(wire.FileReadData|wire.FileWriteData, shareNone)); st != status.Success {
		t.Fatalf("属性探测不得挡住真实打开，实际 %v", st)
	}
}

// TestShareModeCheckDoesNotRegister：预检只判定，不留痕。
func TestShareModeCheckDoesNotRegister(t *testing.T) {
	var tbl shareModeTable
	key := shareModeKey{fileID: 3}

	if st := tbl.check(key, wire.FileReadData, shareNone); st != status.Success {
		t.Fatalf("空表预检应放行，实际 %v", st)
	}
	if n := tbl.count(); n != 0 {
		t.Fatalf("预检不得登记：登记数 = %d", n)
	}

	_ = tbl.add(key, openFor(wire.FileReadData, shareNone))
	if st := tbl.check(key, wire.FileReadData, shareAll); st != status.SharingViolation {
		t.Fatalf("预检应当拦下冲突，实际 %v", st)
	}
}

// ---------------------------------------------------------------------------
// 3. 走真实 createFile 的端到端行为
// ---------------------------------------------------------------------------

// shareTestEnv 是一个共享 + 两个独立会话（模拟两个客户端）。
type shareTestEnv struct {
	share *Share
	fs    *vfs.LocalFS
	root  string
	a, b  *Context
}

func newShareTestEnv(t *testing.T) *shareTestEnv {
	t.Helper()

	root := t.TempDir()
	fs := newQueryDirTestFS(t, root)
	share := &Share{Name: "data", Type: wire.ShareTypeDisk, FS: fs}
	return &shareTestEnv{
		share: share,
		fs:    fs,
		root:  root,
		a:     newShareTestClient(t, share),
		b:     newShareTestClient(t, share),
	}
}

// newShareTestClient 在同一个共享上建一个新连接+会话+树，代表另一个客户端。
func newShareTestClient(t *testing.T, share *Share) *Context {
	t.Helper()

	conn := NewConn(&Settings{Shares: []*Share{share}}, "test", "test")
	sess, st := conn.NewSession()
	if st != status.Success {
		t.Fatalf("NewSession: %v", st)
	}
	// 必须走 NewTree 而不是手搭 &Tree{}：TREE_DISCONNECT 的清理路径
	// 是按会话树表找句柄的，手搭的树不在表里，那条清理就测不到。
	tree, st := sess.NewTree(share)
	if st != status.Success {
		t.Fatalf("NewTree: %v", st)
	}
	return &Context{
		Conn:    conn,
		Chain:   &Chain{},
		Session: sess,
		Tree:    tree,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// openShared 走真实 createFile 打开一个文件，返回句柄与错误。
func openShared(ctx *Context, name string, access wire.Access, share wire.ShareAccess) (*Open, error) {
	ctx.Out = make([]byte, wire.HeaderSize)
	ctx.Chain = &Chain{}
	err := createFile(ctx, &wire.CreateRequest{
		Name:              name,
		DesiredAccess:     access,
		ShareAccess:       share,
		CreateDisposition: wire.FileOpenIf,
	})
	if err != nil {
		return nil, err
	}
	return ctx.Chain.LastOpen, nil
}

// closeOpen 走真实 CLOSE handler 的清理路径。
func closeOpen(ctx *Context, o *Open) {
	ctx.Session.RemoveOpen(o.Volatile)
	o.close()
}

// TestCreateSharingViolationEndToEnd：两个客户端争同一个文件。
func TestCreateSharingViolationEndToEnd(t *testing.T) {
	env := newShareTestEnv(t)

	// A 以独占方式打开（ShareAccess=0）。
	a, err := openShared(env.a, "f.txt", wire.FileReadData|wire.FileWriteData, shareNone)
	if err != nil {
		t.Fatalf("A 打开失败: %v", err)
	}

	// B 想读 —— 方向一：A 不共享读。
	if _, err := openShared(env.b, "f.txt", wire.FileReadData, shareAll); err != status.SharingViolation {
		t.Fatalf("B 应当收到 STATUS_SHARING_VIOLATION，实际 %v", err)
	}
	// B 只探测属性 —— 必须放行，否则 Finder/Explorer 会寸步难行。
	probe, err := openShared(env.b, "f.txt", wire.FileReadAttributes, shareNone)
	if err != nil {
		t.Fatalf("属性探测被误拒: %v", err)
	}
	closeOpen(env.b, probe)

	// A 关闭后 B 应当能打开。
	closeOpen(env.a, a)
	b, err := openShared(env.b, "f.txt", wire.FileReadData, shareAll)
	if err != nil {
		t.Fatalf("A 关闭后 B 仍打不开: %v", err)
	}
	closeOpen(env.b, b)

	if n := env.share.shareModes.count(); n != 0 {
		t.Fatalf("全部关闭后表应为空，实际残留 %d 条", n)
	}
}

// TestCreateSharingViolationReverseDirection：方向二的端到端。
//
// A 用 SHARE_ALL 打开（谁都不挡），B 却要求独占 —— 必须被拒。
// 只实现方向一的服务端会在这里放行，客户端以为自己拿到了独占。
func TestCreateSharingViolationReverseDirection(t *testing.T) {
	env := newShareTestEnv(t)

	a, err := openShared(env.a, "f.txt", wire.FileReadData, shareAll)
	if err != nil {
		t.Fatalf("A 打开失败: %v", err)
	}
	defer closeOpen(env.a, a)

	if _, err := openShared(env.b, "f.txt", wire.FileReadData, shareNone); err != status.SharingViolation {
		t.Fatalf("B 请求独占应被拒（方向二），实际 %v", err)
	}
}

// TestSharingViolationKeyedByIdentityNotPath：大小写不同的两个名字指向
// 同一个文件时必须互相看见。
//
// 这条钉住「表按文件身份索引，不是按路径字符串」——用路径做键的实现
// 会在这里放行，而客户端明明打开的是同一个文件。
func TestSharingViolationKeyedByIdentityNotPath(t *testing.T) {
	env := newShareTestEnv(t)
	if err := os.WriteFile(filepath.Join(env.root, "Report.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := openShared(env.a, "Report.txt", wire.FileReadData|wire.FileWriteData, shareNone)
	if err != nil {
		t.Fatalf("A 打开失败: %v", err)
	}
	defer closeOpen(env.a, a)

	// 大小写不敏感回退会解析到同一个文件。
	if _, err := openShared(env.b, "report.txt", wire.FileReadData, shareAll); err != status.SharingViolation {
		t.Fatalf("大小写不同的同一文件应当冲突，实际 %v", err)
	}
}

// TestSharingViolationAcrossHardLinks：硬链接是同一个文件。
func TestSharingViolationAcrossHardLinks(t *testing.T) {
	env := newShareTestEnv(t)
	orig := filepath.Join(env.root, "a.bin")
	if err := os.WriteFile(orig, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(orig, filepath.Join(env.root, "b.bin")); err != nil {
		t.Skipf("宿主文件系统不支持硬链接: %v", err)
	}

	a, err := openShared(env.a, "a.bin", wire.FileReadData|wire.FileWriteData, shareNone)
	if err != nil {
		t.Fatalf("A 打开失败: %v", err)
	}
	defer closeOpen(env.a, a)

	if _, err := openShared(env.b, "b.bin", wire.FileReadData, shareAll); err != status.SharingViolation {
		t.Fatalf("硬链接指向同一文件，应当冲突，实际 %v", err)
	}
}

// TestSharingViolationStreamsIndependent：主数据流与 alternate data stream
// 是独立的共享单元，独占主流不该挡住 AFP_AfpInfo。
//
// 依据 Samba vfs_streams_xattr 的 hash_inode()：流被合成独立 inode，
// 因而在 share mode lock 里各成一档。不这么做的话 macOS 打开文件的同时
// 再写 FinderInfo 会自己撞自己。
func TestSharingViolationStreamsIndependent(t *testing.T) {
	env := newShareTestEnv(t)

	a, err := openShared(env.a, "f.txt", wire.FileReadData|wire.FileWriteData, shareNone)
	if err != nil {
		t.Fatalf("A 打开主数据流失败: %v", err)
	}
	defer closeOpen(env.a, a)

	s, err := openShared(env.b, "f.txt:AFP_AfpInfo", wire.FileReadData|wire.FileWriteData, shareNone)
	if err != nil {
		t.Fatalf("独占主数据流不应挡住 alternate data stream: %v", err)
	}
	closeOpen(env.b, s)
}

// TestSharingViolationNotDestructive：被共享模式拒绝的 FILE_OVERWRITE
// 不得先把别人的数据截断掉。
//
// 这是把预检放在 fs.Open **之前**的唯一理由：vfs 的 Open 内部就会截断。
func TestSharingViolationNotDestructive(t *testing.T) {
	env := newShareTestEnv(t)
	const payload = "important"
	if err := os.WriteFile(filepath.Join(env.root, "f.txt"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := openShared(env.a, "f.txt", wire.FileReadData, shareNone)
	if err != nil {
		t.Fatalf("A 打开失败: %v", err)
	}
	defer closeOpen(env.a, a)

	env.b.Out = make([]byte, wire.HeaderSize)
	env.b.Chain = &Chain{}
	err = createFile(env.b, &wire.CreateRequest{
		Name:              "f.txt",
		DesiredAccess:     wire.FileReadData | wire.FileWriteData,
		ShareAccess:       shareAll,
		CreateDisposition: wire.FileOverwrite,
	})
	if err != status.SharingViolation {
		t.Fatalf("覆盖式打开应被拒，实际 %v", err)
	}

	got, rerr := os.ReadFile(filepath.Join(env.root, "f.txt"))
	if rerr != nil {
		t.Fatalf("读回文件: %v", rerr)
	}
	if string(got) != payload {
		t.Fatalf("被拒的覆盖式打开损坏了文件内容: %q, 期望 %q", got, payload)
	}
}

// ---------------------------------------------------------------------------
// 4. 清理：异常路径也必须摘除登记
// ---------------------------------------------------------------------------

// TestShareModeReleasedOnTreeDisconnect：TREE_DISCONNECT 必须摘除登记。
func TestShareModeReleasedOnTreeDisconnect(t *testing.T) {
	env := newShareTestEnv(t)

	if _, err := openShared(env.a, "f.txt", wire.FileReadData|wire.FileWriteData, shareNone); err != nil {
		t.Fatalf("A 打开失败: %v", err)
	}
	if st := env.a.Session.RemoveTree(env.a.Tree.ID); st != status.Success {
		t.Fatalf("RemoveTree: %v", st)
	}
	if n := env.share.shareModes.count(); n != 0 {
		t.Fatalf("TREE_DISCONNECT 后表应为空，实际残留 %d 条", n)
	}
	if _, err := openShared(env.b, "f.txt", wire.FileReadData, shareAll); err != nil {
		t.Fatalf("树断开后 B 应能打开: %v", err)
	}
}

// TestShareModeReleasedOnSessionClose：LOGOFF / 连接断开必须摘除登记。
func TestShareModeReleasedOnSessionClose(t *testing.T) {
	env := newShareTestEnv(t)

	if _, err := openShared(env.a, "f.txt", wire.FileReadData|wire.FileWriteData, shareNone); err != nil {
		t.Fatalf("A 打开失败: %v", err)
	}
	env.a.Session.Close()
	if n := env.share.shareModes.count(); n != 0 {
		t.Fatalf("会话关闭后表应为空，实际残留 %d 条", n)
	}
	if _, err := openShared(env.b, "f.txt", wire.FileReadData, shareAll); err != nil {
		t.Fatalf("会话关闭后 B 应能打开: %v", err)
	}
}

// TestShareModeReleasedOnCreateRollback：CREATE 在登记之后失败时，
// 回滚路径必须把登记摘掉，否则那个文件被永久锁死。
//
// 触发方式：只读共享上带 FILE_DELETE_ON_CLOSE ——
// 登记发生在 RequireWritable 检查之前，正好走回滚。
func TestShareModeReleasedOnCreateRollback(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newQueryDirTestFS(t, root)
	share := &Share{Name: "ro", Type: wire.ShareTypeDisk, FS: fs, ReadOnly: true}
	ctx := newShareTestClient(t, share)

	ctx.Out = make([]byte, wire.HeaderSize)
	ctx.Chain = &Chain{}
	err := createFile(ctx, &wire.CreateRequest{
		Name:              "f.txt",
		DesiredAccess:     wire.FileReadData,
		ShareAccess:       shareNone,
		CreateDisposition: wire.FileOpen,
		CreateOptions:     wire.FileDeleteOnClose,
	})
	if err == nil {
		t.Fatal("只读共享上的 delete-on-close 应当失败")
	}
	if n := share.shareModes.count(); n != 0 {
		t.Fatalf("CREATE 回滚后表应为空，实际残留 %d 条", n)
	}
}

// ---------------------------------------------------------------------------
// 5. 并发（配合 -race）
// ---------------------------------------------------------------------------

// TestShareModeConcurrentExclusive 并发争抢独占句柄。
//
// 断言的不是「不 panic」，而是**任意时刻最多只有一个独占句柄存活**：
// 判定与登记若不在同一次持锁内完成，这里会立刻抓到 2。
func TestShareModeConcurrentExclusive(t *testing.T) {
	env := newShareTestEnv(t)
	if err := os.WriteFile(filepath.Join(env.root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	const goroutines, rounds = 8, 60

	var (
		inflight atomic.Int32
		granted  atomic.Int64
		bad      atomic.Int32
		wg       sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := newShareTestClient(t, env.share)
			for i := 0; i < rounds; i++ {
				o, err := openShared(ctx, "f.txt", wire.FileReadData|wire.FileWriteData, shareNone)
				if err != nil {
					continue
				}
				granted.Add(1)
				if n := inflight.Add(1); n != 1 {
					bad.Store(n)
				}
				inflight.Add(-1)
				closeOpen(ctx, o)
			}
		}()
	}
	wg.Wait()

	if n := bad.Load(); n != 0 {
		t.Fatalf("同一时刻出现了 %d 个独占句柄 —— 判定与登记不是原子的", n)
	}
	if granted.Load() == 0 {
		t.Fatal("一次都没成功：这个测试没有在验证任何东西")
	}
	if n := env.share.shareModes.count(); n != 0 {
		t.Fatalf("全部关闭后表应为空，实际残留 %d 条", n)
	}
}
