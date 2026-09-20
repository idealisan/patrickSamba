package command

import (
	"sync"

	"github.com/idealisan/patrickSamba/internal/smb/status"
	"github.com/idealisan/patrickSamba/internal/smb/wire"
)

// share_access.go —— SMB 共享模式（ShareAccess）冲突判定。
//
// 规范：MS-SMB2 §3.3.5.9 CREATE 处理要求服务端在真正打开对象之前跑一遍
// MS-FSA §2.1.5.1.2 "Algorithm to Check Sharing Access"，冲突时回
// STATUS_SHARING_VIOLATION。ShareAccess 三个位见 MS-SMB2 §2.2.13：
// FILE_SHARE_READ(0x1) / FILE_SHARE_WRITE(0x2) / FILE_SHARE_DELETE(0x4)。
//
// **判定是双向的**，这是最容易只做一半的地方：
//
//	方向一：新请求的 DesiredAccess 必须被**所有已存在句柄**的 ShareAccess 允许；
//	方向二：新请求的 ShareAccess 必须允许**所有已存在句柄**的 DesiredAccess。
//
// 只做方向一的话，「先以 SHARE_ALL 打开、再以 ShareAccess=0 打开」这种
// 反向独占请求会被放行，第二个客户端会以为自己拿到了独占，实际没有。
//
// 判定表放在 command 层而不是 vfs 层：vfs 是句柄语义的文件系统抽象，
// 跨句柄的冲突判定属于连接/会话/树/句柄状态机的职责（AGENTS.md §5 P3）。
// 表挂在 Share 上，与字节范围锁表（lock.go 的 Share.locks）完全同构 ——
// 两者都要求「跨会话可见」：两个客户端连到同一个共享的同一个文件必须
// 能看见彼此。
//
// 已知边界（与 Share.locks 相同，刻意不修）：两个**不同的共享**指向同一个
// 宿主目录时，彼此看不到对方的句柄。要跨共享检测就需要 (设备号, inode)
// 这一级的全局身份，而 vfs.Attr 只暴露卷内 FileID，vfs.FSInfo.VolumeSerial
// 又是按共享根路径派生的（internal/vfs/local.go 的 volumeSerial），
// 不能代表真实设备。

// conflictingAccess 是「会参与共享冲突判定」的访问位。
//
// 只请求属性/EA/ACL 的 open（Explorer 与 Finder 会为列表里的每个文件做
// 这种探测，量极大）一个位都不占，因此**永远不冲突**，也不必进表。
// MS-FSA §2.1.5.1.2 的算法只检查这几个位；Samba `source3/smbd/open.c` 的
// `conflicting_access`（同名变量）取值与此逐位一致。
const conflictingAccess = wire.FileReadData | wire.FileWriteData |
	wire.FileAppendData | wire.FileExecute | wire.Delete

// shareModeKey 是共享模式表的键：**文件身份**，不是路径。
//
// 用路径做键是错的：大小写不敏感回退、硬链接、以及 SET_INFO 的
// FileRenameInformation（改名后句柄仍然有效，见 set_info.go）都会让
// 路径与文件身份对不上，冲突判定要么漏判要么误判。
//
// stream 参与键：alternate data stream 是独立的共享单元。这一点规范没写，
// 依据是 Samba —— `source3/modules/vfs_streams_xattr.c` 的 `hash_inode()`
// 会把流名混进合成 inode（stream_xattr 的 stat 里 st_ex_ino 被改写），
// 而 Samba 的 share mode lock 正是按 file_id 索引的，于是每个流各成一档。
// 不这么做的话，macOS 打开 `file` 主数据流的同时再开 `file:AFP_AfpInfo`
// 会自己和自己撞车。
type shareModeKey struct {
	fileID uint64
	stream string
}

// shareModeEntry 是一个已打开句柄在共享模式表里的登记。
//
// access/share 是登记时的快照而不是每次去读 *Open：这两个字段在 Open
// 建立后就不再变化，快照能让判定完全不依赖 Open 的内部锁，
// 从根上避免「表锁 → Open 锁」的加锁顺序。
type shareModeEntry struct {
	owner  *Open
	access wire.Access
	share  wire.ShareAccess
}

// shareModeTable 是一个共享上的共享模式表。零值可用，惰性建表。
type shareModeTable struct {
	mu sync.Mutex
	m  map[shareModeKey][]shareModeEntry
}

// otherOpeners 报告该对象上是否**已经有**别的句柄打开着。
//
// 供 oplock/lease 授予判定使用：写缓存类许可（EXCLUSIVE / BATCH / W 位）
// 只有在"当前没有别人打开这个文件"时才允许授予 —— 否则第二个打开者读到
// 的可能是第一个客户端还攥在本地缓存里的内容。
//
// 判定对象是登记条目而不是 *Open：表里的快照在句柄建立后就不再变化，
// 读它不需要碰 Open 的锁，从根上避免加锁顺序问题。
func (t *shareModeTable) otherOpeners(key shareModeKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m[key]) > 0
}

// hasWriteOpener 报告该对象上是否有别的句柄带**写权限**。
//
// 供 oplock/lease 授予判定用：别人握着写权限时，我们再缓存读就会被
// 悄无声息地写脏 —— 而 SMB 的 break 只在**打开**时触发，不在写入时触发，
// 所以判据必须放在授予这一刻，不能指望"回头再打破"。
//
// 判据只看**权限**（access），不看有没有真的在读：持有写权限的句柄随时
// 可能写，按"会写"处理是唯一安全的口径。
func (t *shareModeTable) hasWriteOpener(key shareModeKey) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.m[key] {
		if e.access&writeAccessMask != 0 {
			return true
		}
	}
	return false
}

// maskConflict 判定一个维度上的双向冲突（MS-FSA §2.1.5.1.2）。
//
// accessBits 是该维度的访问位集合，shareBit 是与之配对的共享位：
//
//	FILE_WRITE_DATA|FILE_APPEND_DATA ↔ FILE_SHARE_WRITE
//	FILE_READ_DATA|FILE_EXECUTE      ↔ FILE_SHARE_READ
//	DELETE                           ↔ FILE_SHARE_DELETE
//
// 两个 if 就是双向性本身，删掉任何一个都会退化成单向判定。
func maskConflict(
	newAccess, existingAccess, accessBits wire.Access,
	newShare, existingShare, shareBit wire.ShareAccess,
) bool {
	// 方向一：我要这个访问权，但已存在的句柄不允许别人这么用。
	if newAccess&accessBits != 0 && existingShare&shareBit == 0 {
		return true
	}
	// 方向二：已存在的句柄正在这么用，而我不允许别人这么用。
	if existingAccess&accessBits != 0 && newShare&shareBit == 0 {
		return true
	}
	return false
}

// shareConflict 判定一次新的 open 是否与一条已存在的登记冲突。
func shareConflict(newAccess wire.Access, newShare wire.ShareAccess, e shareModeEntry) bool {
	// 任一方完全不碰数据（纯属性探测）就不可能冲突。
	if newAccess&conflictingAccess == 0 || e.access&conflictingAccess == 0 {
		return false
	}
	return maskConflict(newAccess, e.access, wire.FileWriteData|wire.FileAppendData,
		newShare, e.share, wire.ShareWrite) ||
		maskConflict(newAccess, e.access, wire.FileReadData|wire.FileExecute,
			newShare, e.share, wire.ShareRead) ||
		maskConflict(newAccess, e.access, wire.Delete,
			newShare, e.share, wire.ShareDelete)
}

// check 只判定不登记，供**打开文件之前**的预检使用。
//
// 为什么需要一次不登记的预检：FILE_SUPERSEDE / FILE_OVERWRITE /
// FILE_OVERWRITE_IF 会在 vfs.FileSystem.Open 内部就把文件截断，
// 等打开成功再判定冲突时数据已经毁了 —— 而这个 CREATE 本该整个失败。
// 预检拦下常见情形，add 里的原子判定负责收口并发竞态。
func (t *shareModeTable) check(key shareModeKey, access wire.Access, share wire.ShareAccess) status.Status {
	if access&conflictingAccess == 0 {
		return status.Success
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.checkLocked(key, access, share, nil)
}

// checkLocked 扫一遍某个文件身份上的全部登记。self 非 nil 时跳过它自己。
// 调用方必须已持有 t.mu。
func (t *shareModeTable) checkLocked(key shareModeKey, access wire.Access, share wire.ShareAccess, self *Open) status.Status {
	for _, e := range t.m[key] {
		if e.owner == self {
			continue
		}
		if shareConflict(access, share, e) {
			return status.SharingViolation
		}
	}
	return status.Success
}

// add 原子地「判定 + 登记」，是唯一权威的一道关。
//
// 判定与写入必须在同一次持锁内完成，否则两个并发 CREATE 会双双通过预检、
// 双双登记，落成一对本该互斥的句柄。
func (t *shareModeTable) add(key shareModeKey, o *Open) status.Status {
	if o.GrantedAccess&conflictingAccess == 0 {
		// 纯属性探测不占任何共享位：既不会冲突，也不会挡住别人，
		// 不进表可以让 Explorer/Finder 的海量探测零开销。
		return status.Success
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if st := t.checkLocked(key, o.GrantedAccess, o.ShareAccess, o); st != status.Success {
		return st
	}
	if t.m == nil {
		t.m = make(map[shareModeKey][]shareModeEntry)
	}
	t.m[key] = append(t.m[key], shareModeEntry{
		owner:  o,
		access: o.GrantedAccess,
		share:  o.ShareAccess,
	})
	// 记下键，摘除时才能 O(1) 定位。此刻 o 还没进会话句柄表，
	// 对其它 goroutine 不可见，写字段是安全的。
	o.shareModeKey = key
	o.shareModeOn = true
	return status.Success
}

// remove 摘除一个句柄的登记。幂等。
//
// 必须覆盖**所有**句柄消失的路径（CLOSE、连接断开、TREE_DISCONNECT、
// LOGOFF、CREATE 中途失败回滚），漏掉任何一条都会让这个文件被永久锁死 ——
// 表里留着一条谁也关不掉的登记，比不做判定还糟。因此调用点放在
// Open.close()（见 open.go），那是所有路径的唯一汇合处。
func (t *shareModeTable) remove(o *Open) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := o.shareModeKey
	cur := t.m[key]
	kept := cur[:0]
	for _, e := range cur {
		if e.owner != o {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		delete(t.m, key)
		return
	}
	t.m[key] = kept
}

// countLocked 返回表内登记总数，仅供测试与日志使用。
func (t *shareModeTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, es := range t.m {
		n += len(es)
	}
	return n
}
