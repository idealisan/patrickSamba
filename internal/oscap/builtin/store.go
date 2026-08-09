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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/finalappstore/stupidsamba/internal/oscap"
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
type store struct {
	db       *bolt.DB
	path     string
	root     string
	readOnly bool
}

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
			return st, nil
		}
		db, err := bolt.Open(p, 0o600, &bolt.Options{Timeout: openFlockTimeout, ReadOnly: true})
		if err != nil {
			return nil, fmt.Errorf("oscap/builtin: 以只读方式打开旁路存储 %s 失败: %w", p, err)
		}
		st.db = db
		return st, nil
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("oscap/builtin: 创建旁路存储目录失败: %w", err)
	}
	db, err := bolt.Open(p, 0o600, &bolt.Options{Timeout: openFlockTimeout})
	if err != nil {
		return nil, fmt.Errorf("oscap/builtin: 打开旁路存储 %s 失败（是否已被另一个实例占用？）: %w", p, err)
	}
	st.db = db
	return st, nil
}

// resolveMetadataPath 决定库文件落在哪。
//
// 硬要求：**不能落在共享目录内部**，否则客户端会在共享里看见这个库文件，
// 甚至把它删掉或备份走。
func resolveMetadataPath(o oscap.Options) (string, error) {
	if o.MetadataPath == "" {
		return defaultMetadataPath(o.Root)
	}
	// 配置项给成目录也接受：这是系统边界（用户手写的 YAML），
	// 在这里判一次比让 bbolt 报一句 "is a directory" 友好得多。
	if fi, err := os.Stat(o.MetadataPath); err == nil && fi.IsDir() {
		return filepath.Join(o.MetadataPath, metadataFileName(o.Root)), nil
	}
	return o.MetadataPath, nil
}

// defaultMetadataPath 给出未配置 metadata_path 时的默认落点：共享目录的**兄弟**位置。
//
// 边界情形：共享根就是卷根（"/" 或 "C:\"）时它没有「旁边」，
// 落到 os.UserConfigDir() 下 —— 那里读的是环境变量，不是系统用户数据库，不违反 C8。
func defaultMetadataPath(root string) (string, error) {
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
	return filepath.Join(parent, metadataFileName(clean)), nil
}

// metadataFileName 用共享根的哈希区分多个共享，避免它们互相覆盖。
func metadataFileName(root string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(normalizePath(filepath.Clean(root))))
	return fmt.Sprintf(".stupidsamba-oscap-%016x.db", h.Sum64())
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
func (s *store) put(bucket, key, val []byte) error {
	if s.readOnly || s.db == nil {
		return oscap.ErrReadOnly
	}
	rec := make([]byte, 0, len(val)+1)
	rec = append(rec, recVersion)
	rec = append(rec, val...)
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		return b.Put(key, rec)
	})
}

// del 删一条记录，existed 报告它原本在不在。
//
// 区分「删掉了」与「本来就没有」是必须的：ports.go 要求删不存在的属性/流
// 返回 ErrNotFound，而不是静默成功。
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
	return existed, err
}

// kvPair 是一次前缀扫描的结果项。Key 是**去掉前缀之后**的部分。
type kvPair struct {
	Key string
	Val []byte
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
	return id, nil
}

// close 释放库句柄。可重复调用。
func (s *store) close() error {
	if s.db == nil {
		return nil
	}
	db := s.db
	s.db = nil
	return db.Close()
}
