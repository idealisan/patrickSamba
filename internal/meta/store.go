// Package meta 实现「宿主文件系统表达不了的 POSIX 元数据」的旁路存储
// （AGENTS.md §5 P7）。
//
// # 范围（刻意卡得很死）
//
// 只存 **POSIX 属主 / 属组 / 权限位（uid、gid、mode）**。
//
// NTFS 原生就支持 alternate data stream、稀疏文件、稳定 FileID、真实创建时间
// 和 DOS 属性位 —— 这些一律直接用宿主能力，**不进本存储**。每多存一份就多一个
// 不一致来源：旁路记录和宿主属性一旦分叉，谁也说不清哪个是真的。
//
// 这里的 uid/gid 只是**给 VFS 层用的数字标签**（AGENTS.md §1.1）：不做任何系统
// 用户解析，也不要求宿主上真的存在这个用户。
//
// # 平台裁剪
//
// 本包在非 Windows 上编译出的是 noop 实现（noop.go），bbolt 实现（bolt.go）连
// 编译都不会发生，二进制体积不受影响。热路径应当用编译期常量 Enabled 做门卫：
//
//	if meta.Enabled {
//		// 整个分支在 Linux/macOS 上被死代码消除，零开销
//	}
//
// # 在 Linux 上验证 Windows 代码路径
//
// bolt.go / bolt_test.go 的约束是 `windows || metabolt`。本容器跑不了 Windows
// 二进制，所以 CI 必须额外跑一遍：
//
//	go test -tags metabolt ./internal/meta/...
//
// 否则真正上生产的那份实现（bbolt 事务、游标、子树搬迁）在任何机器上都没被执行过，
// 只被 `GOOS=windows go vet` 看过一眼。
package meta

import (
	"encoding/binary"
	"errors"
	"strings"
)

// ErrInvalidKey 表示调用方传入了不合法的键（含 ".." 或 NUL）。
//
// 这是调用方的 bug，不是客户端能触发的正常错误：VFS 层在把路径交给本包之前
// 已经做过规范化与根目录约束（AGENTS.md §8）。此处再拦一道属于纵深防御 ——
// 一旦 ".." 混进键里，Rename/Delete 的前缀扫描就会作用到错误的子树上。
var ErrInvalidKey = errors.New("meta: 非法的元数据键")

// Record 是一个对象的旁路元数据。
type Record struct {
	// UID、GID 是数字标签，不做系统用户解析（AGENTS.md §1.1）。
	UID uint32
	GID uint32

	// Mode 只保留 POSIX 权限位（含 setuid/setgid/sticky），即低 12 位 0o7777。
	// 文件类型位（S_IFREG/S_IFDIR 等）由宿主文件系统决定，不在这里存 ——
	// 存了就会和真实类型分叉。Put 会自动做掩码。
	Mode uint32

	// FileID 是写入时观察到的宿主稳定文件号（Windows 上取
	// GetFileInformationByHandle 的 nFileIndex，即 48 位 MFT 序号 + 16 位复用
	// 计数）。0 表示「未知，不参与校验」。
	//
	// 它**不是键**，只是一枚防串味的印章，见 Record.StaleFor。
	FileID uint64
}

// StaleFor 判断这条记录是否已经不属于 observed 这个对象。
//
// 键是路径，而路径是会被复用的：客户端删掉 a/b.txt 再建一个同名文件、或者管理员
// 在 Windows 本机上绕过 SMB 直接删改，旧记录都会留在库里，被新对象读到 ——
// 那是实打实的属主串味（新文件显示成别人的 uid，甚至按别人的 mode 决策）。
//
// 所以每条记录都记下写入时的 FileID。读的时候把当前观察到的 FileID 传进来，
// 对不上就当作**没有记录**，回退到配置里的默认标签。宁可退化成默认值，
// 也不能给出错误的属主。
//
// observed 或记录里的 FileID 为 0（拿不到）时跳过校验：此时降级为纯路径语义，
// 与不带校验的旧行为一致，不会比原来更差。
func (r Record) StaleFor(observed uint64) bool {
	if r.FileID == 0 || observed == 0 {
		return false
	}
	return r.FileID != observed
}

// Store 是旁路元数据的持久化后端，即 AGENTS.md §5 P7 里说的 MetadataStore。
//
// # 键：为什么用路径而不是 FileID
//
// 候选方案与取舍：
//
//	路径做键   —— 不用打开文件就能读写；Delete/Rename 时对象可能已经不在了，
//	              只有路径拿得到；子树操作是一次前缀扫描。
//	              代价：路径会被复用、外部改名会让记录失联。
//	FileID 做键 —— 天然跟随改名，硬链接的多个名字共享一条记录（这其实才符合
//	              POSIX 语义：uid/gid/mode 是 inode 属性不是名字属性）。
//	              代价：必须先打开文件才拿得到；Delete 之后对象没了，无从回收；
//	              QUERY_DIRECTORY 想批量查就要求枚举时能拿到 FileID
//	              （FileIdBothDirectoryInfo 可以，但 os.ReadDir 走的
//	              FindFirstFile 拿不到），会把复杂度推给 VFS 层。
//
// 结论：**路径做键，FileID 降级为记录内的校验印章**（Record.FileID）。
// 拿到了路径方案的全部便利，同时用印章堵掉它唯一真正危险的失败模式 ——
// 路径复用导致的属主串味。剩下的失败模式（外部改名 → 记录失联）只会退化成
// 「读到默认值」，是安全的，并由 Reap 回收。
//
// # 键的形式
//
// 传入的是**共享内相对路径**（`a/b.txt`，'/' 或 '\' 分隔均可，根目录用 ""）。
// 内部会规范化并做大小写折叠，见 Fold。
//
// # 并发
//
// 所有方法都必须并发安全。
type Store interface {
	// Get 读取单个对象的记录。查不到、记录损坏、事务出错一律返回 ok=false。
	//
	// 旁路存储不可用**绝不能**导致文件共享不可用：调用方在 ok=false 时退回
	// 配置里的默认 uid/gid/mode 即可。因此本方法不返回 error。
	//
	// Get 不做任何写入（包括不顺手清理过期记录），读路径上不产生写放大。
	Get(key string) (Record, bool)

	// GetDir 一次取出 dir 下**直接子项**的全部记录，返回的 map 以折叠后的
	// 文件名（不含路径）为键，用 Fold 转换后查表。
	//
	// QUERY_DIRECTORY 一次要列几千个文件，逐个 Get 就是几千次独立读事务；
	// 这里一次游标前缀扫描全部拿到。dir 为 "" 表示共享根。
	GetDir(dir string) map[string]Record

	// Put 写入（覆盖）一条记录。rec.Mode 会被掩码到 0o7777。
	Put(key string, rec Record) error

	// Delete 删除 key 的记录，**并连带清掉它的整棵子树**。
	//
	// 删目录时子树必须一起清：否则之后在同名路径下重建目录，里面的文件会
	// 读到上一个目录里同名文件的属主。
	Delete(key string) error

	// Rename 把 oldKey 及其子树整体迁到 newKey 下。
	//
	// 目标若已有记录（改名覆盖），连同其子树一并清除后再写入，不留残渣。
	// 源没有任何记录时不动目标 —— 否则一次无意义的改名会把目标的记录抹掉。
	Rename(oldKey, newKey string) error

	// Reap 扫全库，对每条记录调用 alive；返回 false 的记录被删除，返回删除条数。
	//
	// 用来回收**孤儿记录**：绕过 SMB 直接在 Windows 本机上删除/改名的对象，
	// 本包无从感知，记录会一直堆着。alive 收到的 key 是折叠后的共享内相对路径。
	//
	// 本包不自己起 goroutine、不自带定时器（noop 平台必须零开销，而在 Windows
	// 上「什么时候扫、扫多久」是策略，属于调用方）。调用方自行决定在启动时扫、
	// 还是挂个低频定时器。
	//
	// alive 在只读事务中被调用，里面**不要**回调本 Store 的任何方法（会死锁）。
	// 分批提交，中途失败已删除的部分不回滚 —— 删多了也只是退化成默认值。
	Reap(alive func(key string, rec Record) bool) (int, error)

	// Close 关闭底层存储。
	Close() error
}

// recordVersion 是记录编码的版本号，占首字节。
//
// 留版本号是为了将来加字段时老库不会被静默误读：解码遇到不认识的版本一律
// 当作「没有记录」（回退默认值），而不是按新布局去解老字节。
const recordVersion = 1

// recordLen 是 v1 记录的字节数：version(1) + uid(4) + gid(4) + mode(4) + fileID(8)。
//
// 显式使用**小端**。这是本地存储不是网络报文，但 AGENTS.md §5 要求所有二进制
// 编码都写死字节序，避免跨架构（amd64 → arm64）搬库时行为漂移。
const recordLen = 21

// modeMask 是允许存储的 mode 位：rwx*3 + setuid + setgid + sticky。
const modeMask = 0o7777

// encodeRecord 把记录编成定长 21 字节。
func encodeRecord(r Record) []byte {
	buf := make([]byte, recordLen)
	buf[0] = recordVersion
	binary.LittleEndian.PutUint32(buf[1:5], r.UID)
	binary.LittleEndian.PutUint32(buf[5:9], r.GID)
	binary.LittleEndian.PutUint32(buf[9:13], r.Mode&modeMask)
	binary.LittleEndian.PutUint64(buf[13:21], r.FileID)
	return buf
}

// decodeRecord 解析记录。
//
// 长度不足或版本不认识一律返回 ok=false 而不是 panic，也不是报错
// （AGENTS.md §5「二进制解析必须先校验长度再切片」）。损坏的记录退化成
// 「没有记录」，调用方用默认标签兜底，服务照常跑。
func decodeRecord(buf []byte) (Record, bool) {
	if len(buf) < recordLen || buf[0] != recordVersion {
		return Record{}, false
	}
	return Record{
		UID:    binary.LittleEndian.Uint32(buf[1:5]),
		GID:    binary.LittleEndian.Uint32(buf[5:9]),
		Mode:   binary.LittleEndian.Uint32(buf[9:13]) & modeMask,
		FileID: binary.LittleEndian.Uint64(buf[13:21]),
	}, true
}

// Fold 把一个路径分量（或整条路径）折叠成库内使用的形式。
//
// 折叠 = 小写。本存储只在 Windows 上启用，而 NTFS 默认大小写不敏感：客户端
// 用 Foo.txt 创建、用 FOO.TXT 打开是同一个文件，不折叠就会查不到自己刚写的记录。
//
// 反过来，Win10 1803+ 支持按目录打开大小写敏感标志，此时 Foo.txt 与 foo.txt
// 可以共存，折叠会让它们撞到同一个键。这个碰撞由 Record.StaleFor 的 FileID
// 校验兜住：印章对不上就当没记录，退化成默认值，而不会串味。
//
// 这里用 Go 的 Unicode 小写规则，与 NTFS 的大写映射表在个别字符上并不完全一致
// （例如土耳其语 İ）。无所谓：本包只要求**自己前后一致**，不一致的极少数情况
// 同样落到 FileID 校验上。
//
// GetDir 返回的 map 就是以 Fold 后的文件名为键，调用方查表前需要自己折叠。
func Fold(s string) string {
	return strings.ToLower(s)
}

// normKey 把共享内相对路径规范成库内键。
//
// 库内键一律带前导 '/'，根目录就是 "/"。这样「取 dir 的直接子项」永远是
// 一次 dir+"/" 前缀扫描（根目录也不例外），不用为根写特例；而且 "/a/" 这个
// 前缀天然不会误伤 "/ab"。
//
// 结果会经 Fold 折叠大小写（见 Fold）。**前提契约**：Fold 只在宿主文件系统
// 大小写不敏感时才正确——这正是 Windows/NTFS 的默认行为，本包本来也只在
// Windows 上启用（bolt.go 的 `windows || metabolt` 约束）。`metabolt` 这个 tag
// 仅供 CI 在 Linux 上**编译/测试**用，**不是生产配置**：若有人拿 `-tags metabolt`
// 在 Linux（大小写敏感）上跑真业务，a.txt 与 A.txt 会共用同一条记录，造成与
// Record.StaleFor 注释里同一性质的属主串味。这条假设由 bolt_test.go 的
// TestNormKeyCaseFoldingIsContract 钉死为显式契约，而非隐含行为。
func normKey(rel string) (string, error) {
	if strings.IndexByte(rel, 0) >= 0 {
		return "", ErrInvalidKey
	}
	// Windows 上 VFS 可能递过来 '\' 分隔的路径。
	rel = strings.ReplaceAll(rel, "\\", "/")

	var parts []string
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
			// 空分量（"a//b"、前后导 '/'）与 "." 都是无害的冗余，收敛掉。
		case "..":
			// 绝不能收敛：一旦让 ".." 参与前缀计算，Delete/Rename 会作用到
			// 错误的子树上。这是调用方 bug，直接拒绝。
			return "", ErrInvalidKey
		default:
			parts = append(parts, seg)
		}
	}
	if len(parts) == 0 {
		return "/", nil
	}
	return "/" + Fold(strings.Join(parts, "/")), nil
}

// childPrefix 返回「dirKey 的直接子项」所共有的键前缀。
//
// 根键已经是 "/"，再拼一个 '/' 会变成 "//"，这是唯一的特例。
func childPrefix(dirKey string) string {
	if dirKey == "/" {
		return "/"
	}
	return dirKey + "/"
}

// relFromKey 把库内键还原成共享内相对路径（去掉前导 '/'）。
//
// 只用于 Reap 回调：调用方需要拿它去 stat。注意它是折叠过的，
// 在大小写不敏感的 NTFS 上能正确命中，这也是 Reap 只在 Windows 上有意义的原因。
func relFromKey(key string) string {
	return strings.TrimPrefix(key, "/")
}
