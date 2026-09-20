//go:build integration

package integration

import (
	"fmt"
	"testing"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// share_access_test.go —— 共享模式（ShareAccess）冲突判定的**跨连接**端到端验证。
//
// 与 internal/smb/command/share_access_test.go 的分工：
//   - 单元测试在同一进程里直接调 createFile，覆盖判定矩阵与表的生命周期；
//   - 本文件走**两条真实 TCP 连接、两个独立会话**，验证冲突判定在真实
//     连接边界上确实生效 —— 这是单元测试覆盖不到的部分（表挂在 Share 上，
//     必须跨会话可见才有意义）。
//
// 判定是双向的（MS-FSA §2.1.5.1.2），因此每个方向都必须有独立用例，
// 且每个「应被拒」用例都配一个**对照组**「应放行」，否则判定退化成
// 无条件拒绝时测试依然全绿（AGENTS.md §9 / 可证伪原则）。

// createShare 是 rawClient.create 的显式访问位版本。
//
// 定义在本文件而不是改 rawclient_test.go：那个文件属于 qa，
// 同包内给 *rawClient 加方法不产生文件冲突（AGENTS.md §7.1 文件所有权）。
// 返回 NTSTATUS 本身而不是 error，调用方要精确断言 0xC0000043。
func (c *rawClient) createShare(
	name string,
	access wire.Access,
	share wire.ShareAccess,
	disp wire.CreateDisposition,
) (wire.FileID, status.Status, error) {
	req := &wire.CreateRequest{
		ImpersonationLevel: wire.ImpersonationImpersonation,
		DesiredAccess:      access,
		FileAttributes:     wire.FileAttributeNormal,
		ShareAccess:        share,
		CreateDisposition:  disp,
		CreateOptions:      wire.FileNonDirectoryFile,
		Name:               name,
	}
	body, err := req.Append(nil)
	if err != nil {
		return wire.FileID{}, 0, err
	}
	resp, err := c.request(wire.CommandCreate, body)
	if err != nil {
		return wire.FileID{}, 0, err
	}
	hdr, err := wire.ParseHeader(resp)
	if err != nil {
		return wire.FileID{}, 0, err
	}
	st := status.Status(hdr.Status)
	if st != status.Success {
		return wire.FileID{}, st, nil
	}
	cr, err := wire.ParseCreateResponse(resp)
	if err != nil {
		return wire.FileID{}, st, err
	}
	return cr.FileID, st, nil
}

// shareAccessPeers 建两条独立连接（各自独立会话）并连上同一个共享。
func shareAccessPeers(t *testing.T, addr string) (*rawClient, *rawClient) {
	t.Helper()
	mk := func(tag string) *rawClient {
		c := newRawClient(t, addr)
		if err := c.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
			t.Fatalf("%s dial: %v", tag, err)
		}
		t.Cleanup(c.close)
		if err := c.treeConnect(testShare); err != nil {
			t.Fatalf("%s treeConnect: %v", tag, err)
		}
		return c
	}
	return mk("A"), mk("B")
}

// mustOpen 断言 open 成功，返回句柄。
func mustOpen(t *testing.T, c *rawClient, label, name string,
	access wire.Access, share wire.ShareAccess, disp wire.CreateDisposition) wire.FileID {
	t.Helper()
	fid, st, err := c.createShare(name, access, share, disp)
	if err != nil {
		t.Fatalf("%s: 传输层失败: %v", label, err)
	}
	if st != status.Success {
		t.Fatalf("%s: 期望打开成功，实际 %v", label, st)
	}
	return fid
}

// mustViolate 断言 open 被 STATUS_SHARING_VIOLATION 拒绝。
func mustViolate(t *testing.T, c *rawClient, label, name string,
	access wire.Access, share wire.ShareAccess, disp wire.CreateDisposition) {
	t.Helper()
	fid, st, err := c.createShare(name, access, share, disp)
	if err != nil {
		t.Fatalf("%s: 传输层失败: %v", label, err)
	}
	if st == status.Success {
		// 意外成功必须立刻关掉，否则泄漏句柄会污染同一测试里的后续断言。
		_ = c.closeFile(fid)
		t.Fatalf("%s: 期望 STATUS_SHARING_VIOLATION，实际打开成功", label)
	}
	if st != status.SharingViolation {
		t.Fatalf("%s: 期望 STATUS_SHARING_VIOLATION，实际 %v", label, st)
	}
}

// seedFile 用一条连接建好文件并写入内容，随后关闭句柄。
func seedFile(t *testing.T, c *rawClient, name string, data []byte) {
	t.Helper()
	fid := mustOpen(t, c, "seed", name,
		wire.FileWriteData, wire.ShareRead|wire.ShareWrite|wire.ShareDelete, wire.FileOpenIf)
	if err := c.write(fid, 0, data); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := c.closeFile(fid); err != nil {
		t.Fatalf("seed close: %v", err)
	}
}

const shareAll = wire.ShareRead | wire.ShareWrite | wire.ShareDelete

// TestSharingViolationDirectionOne 验证方向一：新请求的 DesiredAccess
// 必须被已存在句柄的 ShareAccess 允许。
//
// A 以 ShareAccess=0 独占打开，B 再请求读数据必须被拒。
func TestSharingViolationDirectionOne(t *testing.T) {
	h := startServer(t, harnessOptions{})
	a, b := shareAccessPeers(t, h.Addr)

	const f = "d1.txt"
	seedFile(t, a, f, []byte("payload"))

	afid := mustOpen(t, a, "A 独占打开", f, wire.FileReadData, 0, wire.FileOpen)

	mustViolate(t, b, "方向一：B 请求 READ_DATA（A 不共享读）", f,
		wire.FileReadData, shareAll, wire.FileOpen)

	// 对照组：A 关闭后同一个请求必须放行。若判定退化成无条件拒绝，
	// 这一步会失败 —— 这就是上面那条断言的反向对照。
	if err := a.closeFile(afid); err != nil {
		t.Fatalf("A close: %v", err)
	}
	bfid := mustOpen(t, b, "对照：A 关闭后 B 应能打开", f,
		wire.FileReadData, shareAll, wire.FileOpen)
	if err := b.closeFile(bfid); err != nil {
		t.Fatalf("B close: %v", err)
	}
}

// TestSharingViolationDirectionTwo 验证方向二：新请求的 ShareAccess
// 必须允许已存在句柄的 DesiredAccess。
//
// 这个方向最容易漏掉：A 以最宽松的 ShareAccess 打开，看起来「什么都允许」，
// 但 B 请求的是「不允许别人写」的共享模式，而 A 正持有写权限，必须被拒。
func TestSharingViolationDirectionTwo(t *testing.T) {
	h := startServer(t, harnessOptions{})
	a, b := shareAccessPeers(t, h.Addr)

	const f = "d2.txt"
	seedFile(t, a, f, []byte("payload"))

	afid := mustOpen(t, a, "A 持写打开", f, wire.FileWriteData, shareAll, wire.FileOpen)

	mustViolate(t, b, "方向二：B 以 share=READ 打开（不允许他人写）", f,
		wire.FileReadData, wire.ShareRead, wire.FileOpen)

	// 对照组一：B 允许他人写，则应放行 —— 证明拒绝来自 SHARE_WRITE 这一位，
	// 而不是「只要有别的句柄就拒」。
	bfid := mustOpen(t, b, "对照：B 以 share=READ|WRITE 打开", f,
		wire.FileReadData, wire.ShareRead|wire.ShareWrite, wire.FileOpen)
	if err := b.closeFile(bfid); err != nil {
		t.Fatalf("B close: %v", err)
	}
	if err := a.closeFile(afid); err != nil {
		t.Fatalf("A close: %v", err)
	}

	// 对照组二：A 只持写而不持 DELETE 时，B 不允许他人删也不该冲突。
	afid = mustOpen(t, a, "A 持写打开(2)", f, wire.FileWriteData, shareAll, wire.FileOpen)
	bfid = mustOpen(t, b, "对照：A 不持 DELETE 时 B 禁删应放行", f,
		wire.FileReadData, wire.ShareRead|wire.ShareWrite, wire.FileOpen)
	if err := b.closeFile(bfid); err != nil {
		t.Fatalf("B close: %v", err)
	}
	if err := a.closeFile(afid); err != nil {
		t.Fatalf("A close: %v", err)
	}
}

// TestSharingViolationDeleteBit 单独验证 DELETE ↔ FILE_SHARE_DELETE 这一维。
//
// 与写维度分开测：三个维度用的是同一个 maskConflict，但配对关系写错
// （例如把 DELETE 配到 SHARE_WRITE）时只有本用例能抓到。
func TestSharingViolationDeleteBit(t *testing.T) {
	h := startServer(t, harnessOptions{})
	a, b := shareAccessPeers(t, h.Addr)

	const f = "del.txt"
	seedFile(t, a, f, []byte("payload"))

	afid := mustOpen(t, a, "A 持 DELETE 打开", f, wire.Delete, shareAll, wire.FileOpen)

	mustViolate(t, b, "方向二：B 以 share=READ|WRITE 打开（不允许他人删）", f,
		wire.FileReadData, wire.ShareRead|wire.ShareWrite, wire.FileOpen)

	bfid := mustOpen(t, b, "对照：B 以 share=ALL 打开应放行", f,
		wire.FileReadData, shareAll, wire.FileOpen)
	if err := b.closeFile(bfid); err != nil {
		t.Fatalf("B close: %v", err)
	}
	if err := a.closeFile(afid); err != nil {
		t.Fatalf("A close: %v", err)
	}
}

// TestSharingViolationAttributeOnlyAllowed 验证纯属性 open 不参与冲突判定。
//
// Explorer / Finder 会为目录列表里的每个文件做这种探测，量极大；
// 若它们也进冲突表，任何被独占打开的文件都会让文件管理器报错。
func TestSharingViolationAttributeOnlyAllowed(t *testing.T) {
	h := startServer(t, harnessOptions{})
	a, b := shareAccessPeers(t, h.Addr)

	const f = "attr.txt"
	seedFile(t, a, f, []byte("payload"))

	afid := mustOpen(t, a, "A 独占打开", f, wire.FileReadData, 0, wire.FileOpen)

	// 纯属性请求，一个冲突位都不占，即便 A 独占也必须放行。
	bfid := mustOpen(t, b, "B 纯属性 open", f, wire.FileReadAttributes, 0, wire.FileOpen)
	if err := b.closeFile(bfid); err != nil {
		t.Fatalf("B close: %v", err)
	}
	if err := a.closeFile(afid); err != nil {
		t.Fatalf("A close: %v", err)
	}
}

// TestSharingViolationPreCheckNotDestructive 验证冲突预检发生在真正打开之前。
//
// FILE_OVERWRITE_IF 会在 vfs.Open 内部就把文件截断。若冲突判定放在
// Open 之后，客户端会拿到 STATUS_SHARING_VIOLATION，**但文件已经被清空** ——
// 一个「报错了却已经毁了数据」的静默数据丢失。
func TestSharingViolationPreCheckNotDestructive(t *testing.T) {
	h := startServer(t, harnessOptions{})
	a, b := shareAccessPeers(t, h.Addr)

	const f = "nodestroy.txt"
	want := []byte("must-survive-the-rejected-overwrite")
	seedFile(t, a, f, want)

	afid := mustOpen(t, a, "A 独占打开", f, wire.FileReadData, 0, wire.FileOpen)

	mustViolate(t, b, "B 以 FILE_OVERWRITE_IF 打开应被拒", f,
		wire.FileWriteData, shareAll, wire.FileOverwriteIf)

	if err := a.closeFile(afid); err != nil {
		t.Fatalf("A close: %v", err)
	}

	vfid := mustOpen(t, a, "复核读取", f, wire.FileReadData, shareAll, wire.FileOpen)
	got, err := a.read(vfid, 0, uint32(len(want)))
	if err != nil {
		t.Fatalf("复核读取失败: %v", err)
	}
	if err := a.closeFile(vfid); err != nil {
		t.Fatalf("复核 close: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("被拒后文件内容被破坏\n want %q\n  got %q", want, got)
	}
}

// TestSharingViolationReleasedOnDisconnect 验证连接异常断开（不发 CLOSE）
// 也会释放登记。
//
// 这是最要命的清理路径：客户端崩溃 / 拔网线时若不释放，文件会被**永久**
// 锁死，只能重启服务端。
func TestSharingViolationReleasedOnDisconnect(t *testing.T) {
	h := startServer(t, harnessOptions{})
	a, b := shareAccessPeers(t, h.Addr)

	const f = "disc.txt"
	seedFile(t, a, f, []byte("payload"))

	// 另起一条连接独占打开，然后直接断链，不发 CLOSE、不发 LOGOFF。
	victim := newRawClient(t, h.Addr)
	if err := victim.dial([]wire.Dialect{wire.SMB302}, false); err != nil {
		t.Fatalf("victim dial: %v", err)
	}
	if err := victim.treeConnect(testShare); err != nil {
		t.Fatalf("victim treeConnect: %v", err)
	}
	mustOpen(t, victim, "victim 独占打开", f, wire.FileReadData, 0, wire.FileOpen)

	// 断链前先确认它确实锁住了 —— 否则下面的「断链后能开」是空断言。
	mustViolate(t, b, "断链前 B 应被拒", f, wire.FileReadData, shareAll, wire.FileOpen)

	victim.close()

	// 服务端清理是异步的（读循环发现 EOF 后才收敛），给它一点时间。
	var last status.Status
	for i := 0; i < 100; i++ {
		fid, st, err := b.createShare(f, wire.FileReadData, shareAll, wire.FileOpen)
		if err != nil {
			t.Fatalf("B create: %v", err)
		}
		last = st
		if st == status.Success {
			if err := b.closeFile(fid); err != nil {
				t.Fatalf("B close: %v", err)
			}
			return
		}
		if st != status.SharingViolation {
			t.Fatalf("B 打开返回意外状态: %v", st)
		}
		if err := b.echo(); err != nil { // 轻量等待，同时确认连接仍活着
			t.Fatalf("B echo: %v", err)
		}
	}
	t.Fatalf("victim 断链后登记始终未释放，最后一次状态 %v", last)
}

// TestSharingViolationStreamsIndependent 验证命名流是独立的共享单元。
//
// 依据是 Samba 的 vfs_streams_xattr.c hash_inode()：流名参与合成 inode，
// 于是每条流各占一档 share mode。不这么做的话 macOS 在打开主数据流的
// 同时再开 file:AFP_AfpInfo 会自己和自己撞车。
func TestSharingViolationStreamsIndependent(t *testing.T) {
	h := startServer(t, harnessOptions{})
	a, b := shareAccessPeers(t, h.Addr)

	const base = "streamed.txt"
	seedFile(t, a, base, []byte("payload"))

	// A 独占主数据流。
	afid := mustOpen(t, a, "A 独占主数据流", base, wire.FileReadData, 0, wire.FileOpen)

	// B 打开同一文件的命名流 —— 不同的共享单元，必须放行。
	stream := base + ":ads:$DATA"
	sfid := mustOpen(t, b, "B 打开命名流应放行", stream,
		wire.FileWriteData, 0, wire.FileOpenIf)

	// 而 B 再去碰主数据流仍然要被拒 —— 证明上一步的放行来自「流不同」，
	// 不是判定整体失效。
	mustViolate(t, b, "B 打开主数据流仍应被拒", base,
		wire.FileReadData, shareAll, wire.FileOpen)

	// 同一条流上的第二个独占请求同样要被拒。
	mustViolate(t, a, "A 请求同一条流应被拒", stream,
		wire.FileReadData, shareAll, wire.FileOpen)

	if err := b.closeFile(sfid); err != nil {
		t.Fatalf("B close stream: %v", err)
	}
	if err := a.closeFile(afid); err != nil {
		t.Fatalf("A close: %v", err)
	}
}

// TestSharingViolationMatrixOverWire 把判定矩阵整体在真实连接上跑一遍。
//
// 单元测试已经覆盖了 shareConflict 的纯函数矩阵；这里重跑一遍是为了确认
// create.go 里「预检 → 打开 → 登记」这条链路把 DesiredAccess / ShareAccess
// 原样传到了判定函数（曾经的 bug 形态：字段解析出来了但从没接线）。
func TestSharingViolationMatrixOverWire(t *testing.T) {
	cases := []struct {
		name         string
		firstAccess  wire.Access
		firstShare   wire.ShareAccess
		secondAccess wire.Access
		secondShare  wire.ShareAccess
		wantConflict bool
	}{
		{"读独占 vs 读", wire.FileReadData, 0, wire.FileReadData, shareAll, true},
		{"读独占 vs 写", wire.FileReadData, 0, wire.FileWriteData, shareAll, true},
		{"读共享 vs 读", wire.FileReadData, shareAll, wire.FileReadData, shareAll, false},
		{"写共享 vs 禁写", wire.FileWriteData, shareAll, wire.FileReadData, wire.ShareRead, true},
		{"写共享 vs 允写", wire.FileWriteData, shareAll, wire.FileReadData, wire.ShareRead | wire.ShareWrite, false},
		{"读共享 vs 禁读", wire.FileReadData, shareAll, wire.FileWriteData, wire.ShareWrite, true},
		{"删共享 vs 禁删", wire.Delete, shareAll, wire.FileReadData, wire.ShareRead | wire.ShareWrite, true},
		{"删共享 vs 允删", wire.Delete, shareAll, wire.FileReadData, shareAll, false},
		{"属性 vs 读独占", wire.FileReadAttributes, 0, wire.FileReadData, 0, false},
		{"读独占 vs 属性", wire.FileReadData, 0, wire.FileReadAttributes, 0, false},
	}

	h := startServer(t, harnessOptions{})
	a, b := shareAccessPeers(t, h.Addr)

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fmt.Sprintf("matrix%02d.txt", i)
			seedFile(t, a, f, []byte("payload"))

			afid := mustOpen(t, a, "第一个句柄", f, tc.firstAccess, tc.firstShare, wire.FileOpen)
			defer func() {
				if err := a.closeFile(afid); err != nil {
					t.Fatalf("close 第一个句柄: %v", err)
				}
			}()

			if tc.wantConflict {
				mustViolate(t, b, "第二个句柄", f, tc.secondAccess, tc.secondShare, wire.FileOpen)
				return
			}
			bfid := mustOpen(t, b, "第二个句柄", f, tc.secondAccess, tc.secondShare, wire.FileOpen)
			if err := b.closeFile(bfid); err != nil {
				t.Fatalf("close 第二个句柄: %v", err)
			}
		})
	}
}
