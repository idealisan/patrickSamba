//go:build windows || metabolt

package meta

// bolt.go —— Windows 上的 POSIX 元数据旁路存储实现。
//
// # 选型：go.etcd.io/bbolt
//
// 已在 go.mod 里（v1.5.0），**不是新依赖**。对照 AGENTS.md §4 依赖政策：
// 纯 Go、MIT、无 CGO、无外部动态库、维护活跃（etcd 的底层存储）、
// 能交叉编译到 windows/linux/darwin。
//
// 落选方案：
//
//	dgraph-io/badger —— Apache-2.0 合规，但是为高写入吞吐设计的 LSM，依赖树大
//	                   （protobuf 等），常驻内存明显更高。我们的负载是「几十字节的
//	                   记录、读远多于写」，用 LSM 属于杀鸡用牛刀。
//	自己写追加日志   —— AGENTS.md §C6 说「功能不足时才自己写」。bbolt 的功能不是
//	                   不足，是绰绰有余；自己写就得自己搞定 fsync 顺序、崩溃恢复、
//	                   日志压实，纯属给自己造 bug。
//	mattn/go-sqlite3 —— 需要 CGO，违反 C1，AGENTS.md §5 P7 已明令禁止。
//
// # 崩溃一致性（保证到哪一步）
//
// bbolt 是写时复制 B+ 树，两页 meta 交替写并带校验和，每个写事务提交时 fsync。
// 由此得到：
//
//	✅ Put/Delete/Rename **已经返回 nil** 的，kill -9 之后仍在。
//	✅ 半途被杀不会让库损坏到打不开：撕裂的 meta 页校验和过不去，
//	   自动回退到上一个 meta 页，即上一个已提交事务的状态。
//	✅ 每个方法各自是一个事务，Delete/Rename 的子树搬迁要么整体生效要么整体不生效，
//	   不会留下「搬了一半」的库。
//	⚠️ 尚未返回的那次写可能丢失。调用方不该假设「调用发出去了就一定持久」。
//	❌ **文件系统和本库是两个独立的持久化域，没有跨域原子性。**
//	   「建文件」和「写它的元数据记录」之间被杀，只可能是这两种残留：
//	     · 有文件、没记录 → 读到配置里的默认 uid/gid/mode，安全的退化；
//	     · 有记录、没文件 → 孤儿记录，被 Reap 回收；即使在回收前有新对象占了
//	       同一路径，也会因 FileID 印章对不上而被 Record.StaleFor 判为陈旧。
//	   要消除这一条就得上跨域事务（WAL + 重放），代价远超收益，明确不做。

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Enabled 是编译期常量，表示本平台需要旁路元数据存储。见 noop.go 的对应定义。
const Enabled = true

// bucketName 是存放 POSIX 属主/权限位的 bucket 名。
//
// v0.2.0 从 posix 改名为 posix.v2。**不写迁移代码**。理由是一条当初定方案时
// 容易漏掉的事实：v0.1.0 的旁路实现（internal/vfs/metadata_windows.go）受
// `//go:build windows` 约束，而项目没有任何 Windows runner，那份代码在地球上
// 从未被真正执行过一行（连 Linux CI 都不编译它）；v0.1.0 本身是私有仓库的
// 预发布内测包，线上不存在用旧 12 字节布局写出来的 .db 文件。
//
// 为不存在的数据写一段同样无法验证的迁移逻辑，收益为零，风险是新增第二个
// 不可验证的代码路径。不如改名了事：
//
//   - 改名真正防御的是**反方向**：用户先跑 v0.2.0（21 字节记录进 posix2）再回退到
//     v0.1.0，旧二进制看到的是「没有 posix bucket」→ 无记录 → 回退默认值，是安全的
//     降级，而非把 21 字节按 12 字节解出错误的 uid/gid/mode。
//   - 旧 bucket 名 posix **原样保留、本包不读不写**。将来万一真发现用户数据，可凭
//     真实数据补迁移——选项没有被关掉，只是推迟到有证据的时候。
var bucketName = []byte("posix.v2")

// openTimeout 限制等待 bbolt 文件锁的时间。
//
// 不能无限等：同一个库被另一个 stupidsamba 实例占着时应当**快速失败**并给出
// 人话错误，而不是让服务卡死在启动阶段（AGENTS.md §6「人话错误信息」）。
const openTimeout = 5 * time.Second

// reapBatch 是 Reap 单个事务内扫描的记录条数上限。
//
// 分批的理由不是内存，是**不要长时间持有读事务**：bbolt 的空闲页要等到没有
// 读事务引用旧版本才能回收，一个跨越百万条记录的长读事务会让库在扫描期间
// 一直膨胀。而且 alive 回调里通常要 stat 文件系统，快慢不受本包控制。
const reapBatch = 4096

// subtreeSkipByte 用于在游标扫描时**跳过整棵子树**。
//
// 键的分隔符是 '/'（0x2F），字典序上紧随其后的字节是 '0'（0x30）。
// 因此 Seek(prefix + name + "0") 会直接落到 prefix+name 子树的后面，
// 免去逐条走完子孙的开销 —— 列共享根目录时这是 O(全库) 和 O(直接子项) 的差别。
const subtreeSkipByte = "0"

// boltStore 是 Store 的 bbolt 实现。
//
// bbolt 自身的 API 就是并发安全的（单写多读 + 内部锁），不再叠一层 mutex。
type boltStore struct {
	db *bolt.DB
}

// Open 打开（必要时创建）旁路存储。
//
// path 来自配置的 share.metadata_path；config 层已校验它是绝对路径且父目录存在。
// 留空时由本层用 defaultPath(root) 决定落点。
func Open(root, path string) (Store, error) {
	p := path
	if p == "" {
		var err error
		if p, err = defaultPath(root); err != nil {
			return nil, err
		}
	}
	// 只有默认落点才轮到我们建目录；配置显式指定时 config 已校验过父目录存在，
	// 这里再 MkdirAll 一次是幂等的，成本可忽略。
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("meta: 创建元数据存储目录 %s 失败: %w", filepath.Dir(p), err)
	}

	db, err := bolt.Open(p, 0o600, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("meta: 打开元数据存储 %s 失败（是否已被另一个实例占用？）: %w", p, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucketName)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("meta: 初始化元数据存储 %s 失败: %w", p, err)
	}
	return &boltStore{db: db}, nil
}

// defaultPath 给出未配置 metadata_path 时的默认落点。
//
// 刻意**不放在共享目录内部**：否则客户端会看见这个库文件，甚至可能把它删掉、
// 或者被 Time Machine 当成备份内容一起搬走。
//
// 落点是 os.UserConfigDir()（Windows 上即 %AppData%）下的
// stupidsamba/metadata-<root 哈希>.db，用哈希区分多个共享避免互相覆盖。
//
// 注意这里读的是 APPDATA 环境变量，**不是系统用户数据库查询**，不违反 C8。
func defaultPath(root string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("meta: 无法确定元数据存储的默认落点，请在配置里显式设置 metadata_path: %w", err)
	}
	h := fnv.New32a()
	// 用折叠形式做哈希：Windows 路径大小写不敏感，同一个共享写成不同大小写
	// 不应该产生两个库。
	_, _ = h.Write([]byte(Fold(filepath.Clean(root))))
	return filepath.Join(dir, "stupidsamba", fmt.Sprintf("metadata-%08x.db", h.Sum32())), nil
}

// Get 实现 Store。
func (s *boltStore) Get(key string) (Record, bool) {
	k, err := normKey(key)
	if err != nil {
		return Record{}, false
	}
	var (
		rec Record
		ok  bool
	)
	// 错误一律吞掉退化成 ok=false：旁路存储出问题不能让文件共享跟着不可用。
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}
		rec, ok = decodeRecord(b.Get([]byte(k)))
		return nil
	})
	return rec, ok
}

// GetDir 实现 Store。
func (s *boltStore) GetDir(dir string) map[string]Record {
	dk, err := normKey(dir)
	if err != nil {
		return nil
	}
	prefix := []byte(childPrefix(dk))

	out := make(map[string]Record)
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		k, v := c.Seek(prefix)
		for k != nil && bytes.HasPrefix(k, prefix) {
			name := string(k[len(prefix):])
			if name == "" {
				// 只有列共享根时会撞上：根自己的记录键就是 "/"，
				// 它是 dir 本身而不是 dir 的子项。
				k, v = c.Next()
				continue
			}
			if i := strings.IndexByte(name, '/'); i >= 0 {
				// 子孙不是直接子项，整棵跳过。
				k, v = c.Seek(append(append([]byte(nil), prefix...), name[:i]+subtreeSkipByte...))
				continue
			}
			if rec, ok := decodeRecord(v); ok {
				out[name] = rec
			}
			k, v = c.Next()
		}
		return nil
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// Put 实现 Store。
func (s *boltStore) Put(key string, rec Record) error {
	k, err := normKey(key)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketName)
		if err != nil {
			return err
		}
		return b.Put([]byte(k), encodeRecord(rec))
	})
}

// Delete 实现 Store，连带清掉 key 的整棵子树。
func (s *boltStore) Delete(key string) error {
	k, err := normKey(key)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}
		return deleteTree(b, k)
	})
}

// Rename 实现 Store，把 oldKey 及其子树整体迁到 newKey 下。
func (s *boltStore) Rename(oldKey, newKey string) error {
	ok1, err := normKey(oldKey)
	if err != nil {
		return err
	}
	nk, err := normKey(newKey)
	if err != nil {
		return err
	}
	if ok1 == nk {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return nil
		}
		// 先把要搬的记录整体读出来再动 bucket：
		// bbolt 不允许在游标遍历过程中改动同一个 bucket，
		// 而且游标返回的切片在改动后可能失效，所以下面全是副本。
		type move struct {
			key []byte
			val []byte
		}
		var moves []move
		if v := b.Get([]byte(ok1)); v != nil {
			moves = append(moves, move{key: []byte(nk), val: append([]byte(nil), v...)})
		}
		oldPrefix := childPrefix(ok1)
		newPrefix := childPrefix(nk)
		c := b.Cursor()
		for k, v := c.Seek([]byte(oldPrefix)); k != nil && bytes.HasPrefix(k, []byte(oldPrefix)); k, v = c.Next() {
			dst := newPrefix + string(k[len(oldPrefix):])
			moves = append(moves, move{key: []byte(dst), val: append([]byte(nil), v...)})
		}
		if len(moves) == 0 {
			// 源一条记录都没有。此时**不能**去动目标：一次无意义的改名
			// 不该把目标已有的记录抹掉。
			return nil
		}
		// 目标可能是被覆盖掉的旧对象，连它的子树一起清干净，不留残渣。
		if err := deleteTree(b, nk); err != nil {
			return err
		}
		if err := deleteTree(b, ok1); err != nil {
			return err
		}
		for _, m := range moves {
			if err := b.Put(m.key, m.val); err != nil {
				return err
			}
		}
		return nil
	})
}

// Reap 实现 Store。
func (s *boltStore) Reap(alive func(key string, rec Record) bool) (int, error) {
	if alive == nil {
		return 0, nil
	}
	var (
		removed int
		cursor  []byte // 上一批扫到的最后一个键，作为续扫锚点
		first   = true
	)
	for {
		var (
			doomed  [][]byte
			scanned int
		)
		err := s.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketName)
			if b == nil {
				return nil
			}
			c := b.Cursor()
			var k, v []byte
			if first {
				k, v = c.First()
			} else {
				k, v = c.Seek(cursor)
				// Seek 落回锚点自身说明它还在（没被上一批删掉），跳过它；
				// 若它已被删除，Seek 直接给出后继，正好不能跳。
				if k != nil && bytes.Equal(k, cursor) {
					k, v = c.Next()
				}
			}
			for ; k != nil; k, v = c.Next() {
				cursor = append(cursor[:0], k...)
				scanned++
				rec, ok := decodeRecord(v)
				// 解不开的记录留着不动：删掉别人看不懂的数据太激进
				// （比如未来版本写的新格式被老二进制扫到）。它读出来本就是
				// 「没有记录」，只是占 21 字节。
				if ok && !alive(relFromKey(string(k)), rec) {
					doomed = append(doomed, append([]byte(nil), k...))
				}
				if scanned >= reapBatch {
					return nil
				}
			}
			return nil
		})
		if err != nil {
			return removed, err
		}
		if len(doomed) > 0 {
			err = s.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(bucketName)
				if b == nil {
					return nil
				}
				for _, k := range doomed {
					if e := b.Delete(k); e != nil {
						return e
					}
					removed++
				}
				return nil
			})
			if err != nil {
				return removed, err
			}
		}
		first = false
		if scanned < reapBatch {
			return removed, nil
		}
	}
}

// Close 实现 Store。
func (s *boltStore) Close() error {
	return s.db.Close()
}

// deleteTree 删除 key 自身及其整棵子树，必须在写事务内调用。
//
// 先收集再删除，不用 Cursor.Delete()：bbolt 的 Cursor.Delete 会把当前节点的
// inode 就地摘掉，而随后的 Next() 仍然无脑 index++，于是**恰好跳过一条记录**
// （cursor.go 的 next() 实现）。用它做批量清理会留下漏网的残记录。
func deleteTree(b *bolt.Bucket, key string) error {
	var keys [][]byte
	if b.Get([]byte(key)) != nil {
		keys = append(keys, []byte(key))
	}
	prefix := []byte(childPrefix(key))
	c := b.Cursor()
	for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
		// 必须复制：游标返回的切片指向事务内的页，接下来就要改动 bucket。
		keys = append(keys, append([]byte(nil), k...))
	}
	for _, k := range keys {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
