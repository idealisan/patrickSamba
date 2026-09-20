package builtin

// store.go —— 旁路 KV 存储：**本包唯一接触 bbolt 的地方**。
//
// 六项能力全部只通过下面这几个方法读写元数据。这条纪律的用途见包注释：
// 万一将来要换掉 bbolt（例如遇到没有 mmap 的平台），改动只落在本文件。

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/idealisan/patrickSamba/internal/oscap"
)

// recVersion 是每条记录的版本前缀字节。
//
// 为什么要这一个字节：bbolt 的 Get 对「键不存在」和「键存在但值为空」
// 都可能返回 nil/零长切片，无法区分。而扩展属性「存在但为空」与「不存在」
// 是两种不同的答案（oscap/ports.go 明文规定前者返回 ([]byte{}, nil)）——
// 加一个前缀字节让记录**永不为空**，这个歧义就从根上消失了。
// 附带收益：将来改编码时有地方放版本号。
const recVersion byte = 1

// bucket 名。取值出现在磁盘上的库文件里，属于**落盘格式的一部分**，别顺手改名，
// 改了等于让所有既有共享的元数据凭空消失。
var (
	bucketXattr  = []byte("xattr")   // pathKey\x00name -> value
	bucketHoles  = []byte("holes")   // pathKey        -> []Range（16 字节一条）
	bucketStream = []byte("stream")  // pathKey\x00name -> 流内容
	bucketFileID = []byte("fileid")  // pathKey        -> uint64
	bucketTimes  = []byte("btime")   // pathKey        -> sec(int64)+nsec(int32)
	bucketDOS    = []byte("dosattr") // pathKey        -> uint32
)

// keySep 分隔 pathKey 与属性名/流名。
//
// 用 NUL：任何真实文件系统的路径分量都不允许含 NUL（POSIX 与 Win32 都如此），
// 所以它不可能出现在 pathKey 里，前缀扫描不会串到相邻对象上。
const keySep = 0x00

// openFlockTimeout 限制等待 bbolt 文件锁的时间。
//
// 不能无限等：库被另一个实例占着时应当**快速失败**并给人话错误，
// 而不是让服务卡在启动阶段（与 internal/vfs/metadata_windows.go 取同一策略）。
const openFlockTimeout = 5 * time.Second

// errCorrupt 表示读到一条编码不合法的记录。
var errCorrupt = errors.New("oscap/builtin: 旁路存储记录损坏")

// store 是旁路 KV 的句柄。
//
// db 允许为 nil —— 只读共享且库文件尚不存在时就是这种状态：
// 此时读一律「查不到」、写一律 ErrReadOnly，**绝不创建任何文件**
// （oscap.Options.ReadOnly 的契约：不得创建/写入任何旁路文件）。
//
// readOnly 是**本视图**的只读性，与底层 db 是不是以只读方式打开的无关：
// 同一个库可以同时被一个可写共享和一个只读共享使用（见 openDBs），
// 只读那一侧照样必须拒绝写入。
type store struct {
	db       *bolt.DB
	ref      *dbRef // 非 nil 时表示 db 来自共享登记表，close 要走引用计数
	path     string
	root     string
	readOnly bool
}

// dbRef 是一个被多方共用的 bbolt 句柄。
type dbRef struct {
	db *bolt.DB

	// readOnly 记录**打开时**用的模式。只读打开的句柄永远写不了，
	// 所以后来者要写就不能搭它的车。
	readOnly bool

	refs int
}

// openDBs 登记本进程已经打开的库文件，key 是库文件的**绝对路径**。
//
// # 为什么必须有这张表
//
// bbolt 用 flock 互斥，同一个文件在**同一个进程内**开第二次同样会被自己挡住：
// 第二次 bolt.Open 卡满 openFlockTimeout（5 秒）后报「是否已被另一个实例占用？」——
// 而占用者就是自己，这句提示会把排查引到完全错误的方向。
//
// 这不是假想场景，是**必然发生**的：一个共享根被导出两次（例如同一目录既有
// 可写共享又有只读共享）时，两个 LocalFS 会算出同一个库路径。
// 本仓库现有测试里就有 6 处这种用法（同一 root 再开一个只读 LocalFS）。
//
// 表里存的是句柄不是 store：**句柄可共用，视图不可共用** ——
// 每个 store 有自己的 root（决定 key 前缀）与 readOnly（决定能不能写）。
// 把这两者混在一起共用，只读共享就会跟着可写共享一起获得写权限。
var (
	openDBsMu sync.Mutex
	openDBs   = map[string]*dbRef{}
)

// openStore 打开（必要时创建）旁路存储。
func openStore(o oscap.Options) (*store, error) {
	p, err := resolveMetadataPath(o)
	if err != nil {
		return nil, err
	}
	st := &store{path: p, root: filepath.Clean(o.Root), readOnly: o.ReadOnly}

	if o.ReadOnly {
		if _, err := os.Stat(p); err != nil {
			// 库不存在：只读共享不许建它，退化成「空库」。
			// 这不是降级失败，而是如实反映「这个共享没有任何旁路元数据」。
			//
			// 注意这一步要在 acquireDB 之前：登记表里可能有一个刚被别人
			// 关掉、文件也已不在的条目，先 stat 才是对磁盘现状的判断。
			return st, nil
		}
	} else if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("oscap/builtin: 创建旁路存储目录失败: %w", err)
	}

	ref, err := acquireDB(p, o.ReadOnly)
	if err != nil {
		return nil, err
	}
	st.ref = ref
	st.db = ref.db
	return st, nil
}

// warnNoSyncOnce 保证「旁路库运行于 NoSync 模式」的提示每个进程至多一行。
// 多个共享各开各的库时会多次走到实际打开处，运维只需要被告知一次。
var warnNoSyncOnce sync.Once

// acquireDB 取得库文件 p 的句柄，已经打开过就复用并把引用计数加一。
func acquireDB(p string, readOnly bool) (*dbRef, error) {
	key, err := filepath.Abs(p)
	if err != nil {
		// 拿不到绝对路径就退回原样：宁可退化成「不复用」（老行为，
		// 顶多撞锁报错），也不能用一个可能与别人不一致的 key 去共用句柄。
		key = p
	}

	openDBsMu.Lock()
	defer openDBsMu.Unlock()

	if r, ok := openDBs[key]; ok {
		if !readOnly && r.readOnly {
			// 已有句柄是只读打开的，写不了，也不能就地升级
			// （bbolt 不支持）。**立刻报错，不要去 bolt.Open 白等 5 秒**：
			// 那 5 秒既解决不了问题，还会给出「被另一个实例占用」这句
			// 指向错误方向的提示。
			return nil, fmt.Errorf(
				"oscap/builtin: 旁路存储 %s 已被本进程以只读方式打开，"+
					"无法再以可写方式使用（请让可写共享先于只读共享装配，"+
					"或给它们配置不同的 metadata_path）: %w", key, oscap.ErrReadOnly)
		}
		r.refs++
		return r, nil
	}

	db, err := bolt.Open(p, 0o600, &bolt.Options{
		Timeout:  openFlockTimeout,
		ReadOnly: readOnly,
		// NoSync（持久化语义边界见 bucketIsRegenerable 的成段注释）：
		// 提交只进页缓存 —— 进程崩溃不丢，掉电只威胁可再生桶；
		// 内容类桶在每笔写事务后显式 Sync() 补回真持久性，
		// 关闭路径统一再 Sync 一次（releaseDB）。
		NoSync: !readOnly,
	})
	if err != nil {
		if readOnly {
			return nil, fmt.Errorf("oscap/builtin: 以只读方式打开旁路存储 %s 失败: %w", p, err)
		}
		return nil, fmt.Errorf(
			"oscap/builtin: 打开旁路存储 %s 失败（是否已被另一个进程占用？）: %w", p, err)
	}
	if !readOnly {
		warnNoSyncOnce.Do(func() {
			slog.Warn("oscap/builtin: 旁路库以 NoSync 模式运行",
				"path", key,
				"语义", "进程崩溃不丢；内核崩溃/掉电仅可能丢最近的 DOS 属性/创建时间"+
					"（可再生元数据，按合成基线回落）；命名流等内容按事务强制落盘")
		})
	}
	r := &dbRef{db: db, readOnly: readOnly, refs: 1}
	openDBs[key] = r
	return r, nil
}

// releaseDB 归还句柄，最后一个使用者负责真正关闭。
func releaseDB(p string, ref *dbRef) error {
	key, err := filepath.Abs(p)
	if err != nil {
		key = p
	}

	openDBsMu.Lock()
	defer openDBsMu.Unlock()

	ref.refs--
	if ref.refs > 0 {
		return nil
	}
	// 只删自己那一条：同一个 key 有可能已经被后来者重新打开成另一个 dbRef
	// （前一个全部关闭 → 文件仍在 → 又被 acquireDB 开了一次）。
	if cur, ok := openDBs[key]; ok && cur == ref {
		delete(openDBs, key)
	}
	// 关闭前强制落盘：这是**所有关闭路径**（服务优雅退出、共享卸载、测试收尾）
	// 共用的最后一道闸。NoSync 库的已提交数据此刻可能还在页缓存里，这里补一次
	// 真 fdatasync，把「优雅退出=全量落盘」钉死。只读句柄没写过任何页，跳过
	// （Windows 上 FlushFileBuffers 还要求句柄有写权限，对只读句柄调用必失败）。
	var closeErr error
	if !ref.readOnly {
		closeErr = ref.db.Sync()
	}
	if err := ref.db.Close(); err != nil && closeErr == nil {
		closeErr = err
	}
	return closeErr
}

// resolveMetadataPath 决定库文件落在哪。
//
// 硬要求：**不能落在共享目录内部**，否则客户端会在共享里看见这个库文件，
// 甚至把它删掉或备份走。
func resolveMetadataPath(o oscap.Options) (string, error) {
	if o.MetadataPath == "" {
		return defaultMetadataPath(o.Root, o.InstanceID)
	}
	// 配置项给成目录也接受：这是系统边界（用户手写的 YAML），
	// 在这里判一次比让 bbolt 报一句 "is a directory" 友好得多。
	if fi, err := os.Stat(o.MetadataPath); err == nil && fi.IsDir() {
		return filepath.Join(o.MetadataPath, metadataFileName(o.Root, o.InstanceID)), nil
	}
	return o.MetadataPath, nil
}

// defaultMetadataPath 给出未配置 metadata_path 时的默认落点：共享目录的**兄弟**位置。
//
// instanceID 的语义见 oscap.Options.InstanceID：
//   - 空 ⇒ 历史文件名，与旧版本逐字节一致（测试与库直连场景零变化）；
//   - 非空 ⇒ 把实例编进文件名。bbolt 按 path 拿 flock，多个服务进程共享同一
//     共享目录时（scripts/acceptance.sh 起的就是这种部署），若都算出同一个库
//     文件，第二个进程会卡满 flock 超时（5 秒）后启动失败。监听端点在每个
//     进程上必然不同（同一 addr:port 不可能同时被两个进程 bind），拿它当
//     InstanceID 就让每实例各开各的旁路库，互不阻塞；同一配置重启又得到
//     同一路径，元数据照常复用。
//
// 显式配置的 MetadataPath **优先于** InstanceID（resolveMetadataPath）：
// 用户一旦手写落点，「多个进程别指向同一个文件」就交给用户自己负责 ——
// 这是已拍板的契约，本函数不替显式路径做任何区分。
//
// 边界情形：共享根就是卷根（"/" 或 "C:\"）时它没有「旁边」，
// 落到 os.UserConfigDir() 下 —— 那里读的是环境变量，不是系统用户数据库，不违反 C8。
func defaultMetadataPath(root, instanceID string) (string, error) {
	clean := filepath.Clean(root)
	parent := filepath.Dir(clean)
	if parent == clean {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf(
				"oscap/builtin: 共享根是卷根且无法确定默认落点，请显式配置 metadata_path: %w", err)
		}
		parent = filepath.Join(dir, "stupidsamba")
	}
	return filepath.Join(parent, metadataFileName(clean, instanceID)), nil
}

// metadataFileName 用「共享根哈希 [+ 实例片段]」区分多个库文件：
// 根哈希避免多个共享互相覆盖，实例片段（oscap.InstanceIDSuffix）避免多个
// 服务进程共享同一共享目录时抢同一个 bbolt 文件锁（OI-1）。
//
// instanceID 为空时返回历史文件名 ".stupidsamba-oscap-<root哈希>.db"，
// 与旧版本逐字节一致。
func metadataFileName(root, instanceID string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(normalizePath(filepath.Clean(root))))
	name := fmt.Sprintf(".stupidsamba-oscap-%016x", h.Sum64())
	if suffix, ok := oscap.InstanceIDSuffix(instanceID); ok {
		name += "-" + suffix
	}
	return name + ".db"
}

// normalizePath 把宿主路径规范成 key 用的形式。
//
// Windows 路径大小写不敏感：同一个文件写成不同大小写必须落到同一个 key，
// 否则客户端换个大小写访问就看不见自己刚设的扩展属性了。
func normalizePath(p string) string {
	s := filepath.ToSlash(p)
	if runtime.GOOS == "windows" {
		s = strings.ToLower(s)
	}
	return s
}

// pathKey 把 Ref.Path 映射成库内的 key。
//
// 取**相对共享根**的路径：这样整个库跟着共享目录搬家也依然有效
// （备份/迁移场景很常见），而绝对路径会在换机器后全部作废。
//
// 越界的路径（理论上不该出现 —— oscap 包注释的「路径契约」说明 vfs 层已经做过
// 根目录约束校验）退回用规范化后的绝对路径当 key：**不报错、不 panic**，
// 依旧是确定性映射。这里不重复做安全校验，也不假装自己是安全边界。
func (s *store) pathKey(p string) string {
	clean := filepath.Clean(p)
	if rel, err := filepath.Rel(s.root, clean); err == nil &&
		rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		clean = rel
	}
	return normalizePath(clean)
}

// objKey 是「某个对象」的 key。
func (s *store) objKey(p string) []byte { return []byte(s.pathKey(p)) }

// subKey 是「某个对象的某个具名子项」（扩展属性 / 命名流）的 key。
func (s *store) subKey(p, name string) []byte {
	k := s.pathKey(p)
	b := make([]byte, 0, len(k)+1+len(name))
	b = append(b, k...)
	b = append(b, keySep)
	b = append(b, name...)
	return b
}

// subPrefix 是对象下全部具名子项的公共前缀。
func (s *store) subPrefix(p string) []byte {
	k := s.pathKey(p)
	b := make([]byte, 0, len(k)+1)
	b = append(b, k...)
	b = append(b, keySep)
	return b
}

// get 读一条记录。ok=false 表示键不存在（**不是**错误）。
//
// 返回值一定是拷贝：bbolt 的 Get 返回的切片只在事务内有效，
// 带出事务后继续用是经典的野指针型 bug。
func (s *store) get(bucket, key []byte) (val []byte, ok bool, err error) {
	if s.db == nil {
		return nil, false, nil
	}
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		v := b.Get(key)
		if v == nil {
			return nil
		}
		if len(v) < 1 || v[0] != recVersion {
			return fmt.Errorf("%w: bucket=%s key=%q", errCorrupt, bucket, key)
		}
		val = append([]byte{}, v[1:]...)
		ok = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return val, ok, nil
}

// put 写一条记录（已存在则覆盖）。
//
// 持久性按桶分类（bucketIsRegenerable）：可再生桶的提交只进页缓存
// （NoSync 的收益所在）；其余桶提交成功后立即 db.Sync() 强制真落盘，
// 持久性与 NoSync 化之前完全一致。
func (s *store) put(bucket, key, val []byte) error {
	if s.readOnly || s.db == nil {
		return oscap.ErrReadOnly
	}
	rec := make([]byte, 0, len(val)+1)
	rec = append(rec, recVersion)
	rec = append(rec, val...)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		return b.Put(key, rec)
	}); err != nil {
		return err
	}
	return s.syncUnlessRegenerable(bucket)
}

// syncUnlessRegenerable 在非可再生桶的写事务之后补真落盘。
func (s *store) syncUnlessRegenerable(bucket []byte) error {
	if bucketIsRegenerable(bucket) {
		return nil
	}
	return s.db.Sync()
}

// del 删一条记录，existed 报告它原本在不在。
//
// 区分「删掉了」与「本来就没有」是必须的：ports.go 要求删不存在的属性/流
// 返回 ErrNotFound，而不是静默成功。
//
// 现有的两个调用方（RemoveXattr/RemoveStream）都落在关键桶上，删除照 put 的
// 规则补真落盘；将来若有人给可再生桶加单条删除，同样由分类器自动豁免。
func (s *store) del(bucket, key []byte) (existed bool, err error) {
	if s.readOnly || s.db == nil {
		return false, oscap.ErrReadOnly
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		if b.Get(key) == nil {
			return nil
		}
		existed = true
		return b.Delete(key)
	})
	if err != nil {
		return existed, err
	}
	return existed, s.syncUnlessRegenerable(bucket)
}

// kvPair 是一次前缀扫描的结果项。Key 是**去掉前缀之后**的部分。
type kvPair struct {
	Key string
	Val []byte
}

// allBuckets 是**按路径记账**的全部 bucket（落盘格式的一部分，与上面的
// 单桶常量同步维护）。rename/remove 的元数据迁移必须扫全这里：
// 漏掉任何一个桶就是一条「改名后属性凭空消失/串到别人身上」的 bug。
var allBuckets = [...][]byte{
	bucketXattr, bucketHoles, bucketStream, bucketFileID, bucketTimes, bucketDOS,
}

// bucketIsRegenerable 判定一个桶的内容是否属于「可再生派生元数据」。
//
// 这是**持久化语义边界**的落点（v0.5 team-lead 拍板，本文件全部写路径据此
// 决定要不要强制真落盘；不得自行放宽，也不得自行收窄）：
//
//  1. 库以 NoSync 打开（见 acquireDB）：事务提交只把脏页 write() 进内核页缓存，
//     **进程崩溃不丢**（SMB 服务崩了属性还在），只有内核崩溃/掉电才可能丢最近写入。
//  2. btime / dosattr 两桶是可再生派生元数据：丢记录后读路径按既有合成基线回落
//     （bh3-F5 的「存储值优先」语义不变，回落行为已有单测覆盖），因此允许吃这个
//     掉电窗口 —— 这正是本文件引入 NoSync 要换的东西：小文件 CREATE/DELETE 路径
//     不再被逐事务 fsync 钳死（perf-v050 报告 §4 F4：小文件 IOPS 的天花板）。
//     掉电回退的最坏后果是「同路径新对象继承了旧对象的陈旧属性值」，
//     它与「记录丢失后回落合成基线」是同一枚硬币的两面。
//  3. 其余桶（stream/xattr/holes/fileid）**每次写事务提交成功后立即 db.Sync()**
//     强制真 fdatasync —— bbolt 的 Sync 在 NoSync 下照常落盘，文档明说就是给
//     这种用法留的通道。命名流内容=用户数据，持久性一寸不让；这四类桶的
//     持久性与引入 NoSync 之前逐字节一致。
//
// 判据用 string 比较：bucket 名是包内常量，比较成本可忽略，且不必关心切片容量。
func bucketIsRegenerable(bucket []byte) bool {
	return string(bucket) == string(bucketTimes) || string(bucket) == string(bucketDOS)
}

// keyRanges 是一个路径在库内的全部键边界：
//   - exact：本对象的 obj 键（btime/dosattr/holes/fileid）；
//   - subPre（pathKey+NUL）：本对象具名子项的前缀（xattr / stream）；
//   - childPre（pathKey+"/"）：整棵子树的前缀（目录改名要带走全部后代）。
//
// 两个前缀的边界字符不同（NUL 与 '/'），这正是 keySep 注释里
// 「前缀扫描不会串到相邻对象」的兑现处："a/f.txt" 与 "a/f.txt.bak"
// 共享字符串前缀但路径不同，childPre 的 '/' 边界把它们分开。
type keyRanges struct {
	exact    []byte
	subPre   []byte
	childPre []byte
}

func newKeyRanges(key string) keyRanges {
	r := keyRanges{exact: []byte(key)}
	r.subPre = append(r.subPre, key...)
	r.subPre = append(r.subPre, keySep)
	r.childPre = append(r.childPre, key...)
	r.childPre = append(r.childPre, '/')
	return r
}

// lowerBound 返回扫描本范围的起始键。exact 是另外两个前缀的真前缀，
// 字节序必然最小，Seek 到它即可覆盖全部三个范围。
func (r keyRanges) lowerBound() []byte { return r.exact }

// within 报告游标是否还可能命中本范围。三个范围都以 exact 的字节开头，
// 所以它就是安全的停机条件（兄弟路径如 "a/f.txt.bak" 也会通过本检查，
// 由 hits 精确排除 —— 多扫几条是可接受的浪费，漏扫才是 bug）。
func (r keyRanges) within(k []byte) bool { return hasPrefix(k, r.exact) }

func (r keyRanges) hits(k []byte) bool {
	return string(k) == string(r.exact) ||
		hasPrefix(k, r.subPre) ||
		hasPrefix(k, r.childPre)
}

// renameKeys 把 oldPath 名下（含子树与具名子项）的全部记录搬进 newPath，
// **同一个写事务里**先清掉 newPath 上的陈旧记录再迁入 —— 原子性由 bbolt
// 事务保证：要么全搬完，要么全没动。
//
// 为什么先清目标：replace 改名覆盖了别的文件时，目标路径上留着前任的
// 记录。若不清理，「迁入」会与「残留」在同一个 pathKey 下拼成一份
// 谁也不该看到的组合 —— 恰是 B5 要堵的那类跨对象泄漏的反向形态。
//
// 值原样搬运不做解码：记录编码归各能力的读写方管，这里只当不透明字节，
// 迁移永远不会把一条损坏记录变得更坏。
func (s *store) renameKeys(oldPath, newPath string) error {
	if s.readOnly || s.db == nil {
		return oscap.ErrReadOnly
	}
	src := newKeyRanges(s.pathKey(oldPath))
	dst := newKeyRanges(s.pathKey(newPath))
	if string(src.exact) == string(dst.exact) {
		return nil // 同名 no-op（大小写折叠等场景）
	}
	// 空账快速路：两侧都无任何旁路记录时，下面的迁移事务是纯 no-op
	// （收集不到任何键、也无陈旧记录可清）。原子保存式改名（临时名→正式名）
	// 是客户端高频操作，没账的改名不再为它付一次 fsync。
	srcHas, err := s.rangeHasRecords(src)
	if err != nil {
		return err
	}
	dstHas := true // 已知 src 有账就必须进事务；dst 无须再查
	if !srcHas {
		if dstHas, err = s.rangeHasRecords(dst); err != nil {
			return err
		}
	}
	if !srcHas && !dstHas {
		return nil
	}
	dsrc := string(src.exact)
	ddst := string(dst.exact)
	needSync := false
	err = s.db.Update(func(tx *bolt.Tx) error {
		for _, bn := range allBuckets {
			b := tx.Bucket(bn)
			if b == nil {
				continue
			}
			// 收集阶段只读；删写统一放在遍历结束之后，
			// 避免边遍历边改同一张表。
			var staleKeys [][]byte
			collectKeys(b, dst, func(k []byte) {
				staleKeys = append(staleKeys, append([]byte{}, k...))
			})
			type move struct{ from, to, val []byte }
			var moves []move
			collectPairs(b, src, func(full, val []byte) {
				nk := relabel(string(full), dsrc, ddst)
				moves = append(moves, move{
					from: append([]byte{}, full...),
					to:   []byte(nk),
					val:  append([]byte{}, val...),
				})
			})
			if (len(staleKeys) > 0 || len(moves) > 0) && !bucketIsRegenerable(bn) {
				needSync = true
			}
			for _, k := range staleKeys {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
			for _, m := range moves {
				if err := b.Delete(m.from); err != nil {
					return err
				}
				if err := b.Put(m.to, m.val); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if needSync {
		// 关键桶有账被搬动/清除：整体落盘。若不 Sync，掉电回退可能让旧路径的
		// 流/xattr 记录复活 —— 旧路径上如今住着的若是另一个对象，就是跨对象
		// 元数据泄漏（B5 要堵的那类），所以这里一寸不让。
		return s.db.Sync()
	}
	return nil
}

// deleteKeys 删掉 path 名下（含子树与具名子项）的全部记录。幂等：
// 一条都没有也是成功 —— 调用方刚 os.Remove 完，旁路里可能本来就没账。
//
// 两层写放大治理（都不改变可观测语义）：
//
//  1. **空账快速路**：绝大多数被删对象在旁路库里根本没账。此前这里照样开一个
//     空写事务并 fsync；现在先做只读预检，没账直接返回 —— 与「开一个空事务再
//     提交」一样是幂等 no-op。
//  2. **按桶决定要不要 Sync**：只清了 btime/dosattr 的常见情形不付 fsync ——
//     这些删除即使因掉电回退，也只是「已删对象的可再生属性记录复活」，
//     对象本身已随宿主 unlink 消失，记录不会被任何读路径看到（除非同一路径
//     后来被重建，那是 bucketIsRegenerable 注释里拍板接受的掉电窗口）。
//     只要动过 stream/xattr/holes/fileid 任一关键桶，就整体 Sync，
//     杜绝「旧流内容复活到新对象头上」这类跨对象泄漏窗口（B5 同源）。
func (s *store) deleteKeys(path string) error {
	if s.readOnly || s.db == nil {
		return oscap.ErrReadOnly
	}
	rng := newKeyRanges(s.pathKey(path))
	has, err := s.rangeHasRecords(rng)
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	needSync := false
	err = s.db.Update(func(tx *bolt.Tx) error {
		for _, bn := range allBuckets {
			b := tx.Bucket(bn)
			if b == nil {
				continue
			}
			var keys [][]byte
			collectKeys(b, rng, func(k []byte) {
				keys = append(keys, append([]byte{}, k...))
			})
			if len(keys) > 0 && !bucketIsRegenerable(bn) {
				needSync = true
			}
			for _, k := range keys {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if needSync {
		// 同一个事务里顺带删掉的可再生记录一并被这次 fdatasync 覆盖。
		return s.db.Sync()
	}
	return nil
}

// rangeHasRecords 只读地报告 keyRanges 范围内是否存在至少一条记录。
// deleteKeys / renameKeys 用它做空账快速路，避免为「其实无东西可做」的
// 写事务支付一次 fsync。
func (s *store) rangeHasRecords(rng keyRanges) (bool, error) {
	if s.db == nil {
		return false, nil
	}
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, bn := range allBuckets {
			b := tx.Bucket(bn)
			if b == nil {
				continue
			}
			c := b.Cursor()
			for k, _ := c.Seek(rng.lowerBound()); k != nil && rng.within(k); k, _ = c.Next() {
				if rng.hits(k) {
					found = true
					return nil // 找到一条就够了
				}
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// collectKeys 遍历 b 中落在 rng 范围内的键交给 fn。只读，不改表。
func collectKeys(b *bolt.Bucket, rng keyRanges, fn func(k []byte)) {
	c := b.Cursor()
	for k, _ := c.Seek(rng.lowerBound()); k != nil && rng.within(k); k, _ = c.Next() {
		if rng.hits(k) {
			fn(k)
		}
	}
}

// collectPairs 同 collectKeys，但把值一并交给 fn。只读，不改表。
func collectPairs(b *bolt.Bucket, rng keyRanges, fn func(full, val []byte)) {
	c := b.Cursor()
	for k, v := c.Seek(rng.lowerBound()); k != nil && rng.within(k); k, v = c.Next() {
		if rng.hits(k) {
			fn(k, v)
		}
	}
}

// relabel 把键 full 里开头的 old 路径段换成 new。
// 三种边界分别对应 exact / subPre / childPre，与 keyRanges 的判定一致。
func relabel(full, old, new string) string {
	switch {
	case full == old:
		return new
	case len(full) > len(old) && (full[len(old)] == byte(keySep) || full[len(old)] == '/'):
		return new + full[len(old):]
	default:
		// 不该发生（调用方只会喂命中范围内的键）；保守返回原键，
		// 宁可少迁一条也不能把别人的键改坏。
		return full
	}
}

// scanPrefix 列出某前缀下的全部记录，按 key 升序（bbolt 游标天然有序）。
func (s *store) scanPrefix(bucket, prefix []byte) ([]kvPair, error) {
	if s.db == nil {
		return nil, nil
	}
	var out []kvPair
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, v = c.Next() {
			if len(v) < 1 || v[0] != recVersion {
				return fmt.Errorf("%w: bucket=%s key=%q", errCorrupt, bucket, k)
			}
			out = append(out, kvPair{
				Key: string(k[len(prefix):]),
				Val: append([]byte{}, v[1:]...),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func hasPrefix(b, prefix []byte) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == string(prefix)
}

// idFallbackBase 是「库里分配的 FileID」的起始值。
//
// 真实 inode 永远落在 [1, 2^63) 内，分配值从 2^63 起，两个空间**天然不相交** ——
// 这样同一个卷上「有 inode 的对象」与「只能靠库分配的对象」不会撞号，
// 兑现 ports.go 那条「同一卷内不同对象的值不得相同」的硬要求。
const idFallbackBase uint64 = 1 << 63

// lookupFileID 只读地查一个已分配的 ID。
func (s *store) lookupFileID(key []byte) (uint64, bool, error) {
	v, ok, err := s.get(bucketFileID, key)
	if err != nil || !ok {
		return 0, false, err
	}
	if len(v) != 8 {
		return 0, false, fmt.Errorf("%w: fileid 记录长度 %d", errCorrupt, len(v))
	}
	return binary.LittleEndian.Uint64(v), true, nil
}

// assignFileID 取出或分配一个 ID，**在同一个写事务里完成**。
//
// 必须是一个事务：先 get 再 put 的写法在并发下会给同一个对象发两个不同的 ID，
// 而「同一对象多次查询返回相同值」是 ports.go 的硬契约。
func (s *store) assignFileID(key []byte) (uint64, error) {
	if s.readOnly || s.db == nil {
		return 0, oscap.ErrReadOnly
	}
	var id uint64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketFileID)
		if err != nil {
			return err
		}
		if v := b.Get(key); v != nil {
			if len(v) != 9 || v[0] != recVersion {
				return fmt.Errorf("%w: fileid 记录长度 %d", errCorrupt, len(v))
			}
			id = binary.LittleEndian.Uint64(v[1:])
			return nil
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		id = idFallbackBase + seq
		rec := make([]byte, 9)
		rec[0] = recVersion
		binary.LittleEndian.PutUint64(rec[1:], id)
		return b.Put(key, rec)
	})
	if err != nil {
		return 0, err
	}
	// fileid 是关键桶（bucketIsRegenerable 之外）：一个发出去的 ID 若因掉电
	// 回退而「失忆」，重启后会重新分配出新号 —— 「同一对象多次查询返回相同值」
	// 的硬契约就被打穿。这里补真落盘；本路径本身极冷（仅宿主给不出 inode 时走到）。
	if err := s.db.Sync(); err != nil {
		return 0, err
	}
	return id, nil
}

// close 释放库句柄。可重复调用。
func (s *store) close() error {
	if s.ref == nil {
		// 只读且库文件不存在时压根没开过东西（db 恒为 nil）。
		return nil
	}
	ref := s.ref
	s.ref, s.db = nil, nil
	return releaseDB(s.path, ref)
}
