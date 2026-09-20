//go:build windows

package vfs

// metadata_windows.go —— Windows 上的 POSIX 元数据旁路存储（AGENTS.md §5 P7）。
//
// NTFS 表达不了 POSIX 的 uid/gid/mode，而 macOS 的 Time Machine 会通过 SMB
// 设置并回读这些字段，所以在 Windows 上必须旁路存一份。
//
// 用 go.etcd.io/bbolt：纯 Go、MIT 许可、无 CGO、单文件、并发安全，
// 满足 AGENTS.md C1/C2/C6 与 §4 的依赖政策。
// **禁止**换成 mattn/go-sqlite3 这类需要 CGO 的方案。
//
// 本文件只在 Windows 编译进来，其余平台走 metadata_other.go 的 nil 实现，
// 连 bbolt 都不会被链接进二进制。

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/finalappstore/stupidsamba/internal/oscap"
)

// metadataBucket 是存放 POSIX 属主/权限位的 bucket 名。
var metadataBucket = []byte("posix")

// metadataRecordLen 是一条记录的字节数：uid(4) + gid(4) + mode(4)。
//
// 编码显式使用**小端**（AGENTS.md §5「所有涉及网络字节的代码必须显式写明字节序」；
// 这里虽是本地存储，同样明确写死，避免跨架构读库时行为漂移）。
const metadataRecordLen = 12

// openFlockTimeout 限制等待 bbolt 文件锁的时间。
//
// 不能无限等：同一个数据库被另一个 stupidsamba 实例占用时应当**快速失败**
// 并给出人话错误，而不是让服务卡在启动阶段。
const openFlockTimeout = 5 * time.Second

// boltMetadataStore 是 MetadataStore 的 bbolt 实现。
//
// 它不直接持有 *bolt.DB，而是持有一个**进程内共享**的 storeRef：
// 一个共享根被导出多次（例如同一目录一个可写共享、一个只读共享）时，
// 两个 LocalFS 会算出同一个库路径，而 bbolt 按 path 拿 flock —— 不共享句柄
// 的话第二个会卡满 flock 超时再报「是否已被另一个实例占用」，而占用者
// 就是自己，把人引向完全错误的方向（oscap builtin 侧 builtin/store.go 的
// openDBs 早已如此，这里与之对齐）。
//
// **共享的是句柄，不是视图**：每个 boltMetadataStore 各自持一份引用，
// 关闭时按引用计数归还，最后一个归还者才真正 Close。
type boltMetadataStore struct {
	ref  *storeRef
	path string
}

// storeRef 是一个被进程内共享的 bbolt 句柄及其引用计数。
type storeRef struct {
	db   *bolt.DB
	refs int
}

var (
	openStoresMu sync.Mutex
	openStores   = map[string]*storeRef{}
)

// acquireMetadataStore 取得库文件 p 的句柄，已打开过就复用并把引用计数加一。
// 第二个返回值为 true 表示本次是**新建**（调用方据此决定要不要初始化 bucket）。
func acquireMetadataStore(p string) (*storeRef, bool, error) {
	key, err := filepath.Abs(p)
	if err != nil {
		// 拿不到绝对路径就退回原样：宁可退化成「不复用」（老行为，顶多撞锁
		// 报错），也不能用一个可能与别人不一致的 key 去共用句柄。
		key = p
	}

	openStoresMu.Lock()
	defer openStoresMu.Unlock()

	if r, ok := openStores[key]; ok {
		r.refs++
		return r, false, nil
	}
	db, err := bolt.Open(p, 0o600, &bolt.Options{Timeout: openFlockTimeout})
	if err != nil {
		return nil, false, err
	}
	r := &storeRef{db: db, refs: 1}
	openStores[key] = r
	return r, true, nil
}

// releaseMetadataStore 归还句柄，最后一个使用者负责真正关闭。
func releaseMetadataStore(p string, ref *storeRef) error {
	key, err := filepath.Abs(p)
	if err != nil {
		key = p
	}

	openStoresMu.Lock()
	defer openStoresMu.Unlock()

	ref.refs--
	if ref.refs > 0 {
		return nil
	}
	// 只删自己那一条：同一个 key 有可能已经被后来者重新打开成另一个 storeRef。
	if cur, ok := openStores[key]; ok && cur == ref {
		delete(openStores, key)
	}
	return ref.db.Close()
}

// openMetadataStore 打开（必要时创建）旁路存储。
//
// metadataPath 来自配置的 share.metadata_path；config 层已校验它是绝对路径
// 且父目录存在。留空时由本层决定默认落点 —— 与 mdns agent 约定好的分工。
// instanceID 语义见 LocalConfig.InstanceID：非空时编进默认库文件名，
// 让共享同一目录的多个服务进程各用各的 bbolt 库（bbolt 按 path 拿 flock）。
func openMetadataStore(root, metadataPath, instanceID string) (MetadataStore, error) {
	p := metadataPath
	if p == "" {
		var err error
		if p, err = defaultMetadataPath(root, instanceID); err != nil {
			return nil, err
		}
	}

	// 只有默认落点才由我们建目录；配置显式指定时 config 已经校验过父目录存在，
	// 这里再 MkdirAll 一次是幂等的，成本可忽略。
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, mapError(err)
	}

	ref, created, err := acquireMetadataStore(p)
	if err != nil {
		return nil, fmt.Errorf("vfs: 打开元数据存储 %s 失败（是否已被另一个实例占用？）: %w", p, err)
	}
	if created {
		if err := ref.db.Update(func(tx *bolt.Tx) error {
			_, e := tx.CreateBucketIfNotExists(metadataBucket)
			return e
		}); err != nil {
			_ = releaseMetadataStore(p, ref)
			return nil, fmt.Errorf("vfs: 初始化元数据存储 %s 失败: %w", p, err)
		}
	}
	return &boltMetadataStore{ref: ref, path: p}, nil
}

// defaultMetadataPath 给出未配置 metadata_path 时的默认落点。
//
// 刻意**不放在共享目录内部** —— 否则客户端会看见这个 DB 文件，
// 甚至可能把它删掉或备份走（mdns agent 的 Warnings() 也会对此告警）。
//
// 落点取 os.UserConfigDir()（Windows 上是 %AppData%）下的
// stupidsamba\metadata-<root 哈希>[-<实例片段>].db：root 哈希区分多个共享，
// 实例片段（oscap.InstanceIDSuffix，清洗标签+原始值哈希，规则与 builtin
// 旁路共用同一份）区分共享同一目录的多个服务进程 —— bbolt 按 path 拿
// flock，两个进程算出同一个文件时第二个会卡满超时后启动失败（OI-1 的
// Windows 形态）。instanceID 为空时不带实例片段，与旧版本逐字节一致；
// 显式配置的 MetadataPath 优先于实例区分，用户手写落点时唯一性由用户负责。
//
// 注意这里读的是 APPDATA 环境变量，不是系统用户数据库查询，不违反 C8。
func defaultMetadataPath(root, instanceID string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("vfs: 无法确定元数据存储的默认落点，请在配置里显式设置 metadata_path: %w", err)
	}
	h := fnv.New32a()
	// 用小写形式做哈希：Windows 路径大小写不敏感，
	// 同一个共享写成不同大小写不应该产生两个库。
	_, _ = h.Write([]byte(strings.ToLower(filepath.Clean(root))))
	name := fmt.Sprintf("metadata-%08x", h.Sum32())
	if suffix, ok := oscap.InstanceIDSuffix(instanceID); ok {
		name += "-" + suffix
	}
	return filepath.Join(dir, "stupidsamba", name+".db"), nil
}

// Get 实现 MetadataStore。
//
// 查不到、记录损坏、事务出错一律返回 ok=false —— 调用方（LocalFS.applyMetadata）
// 此时退回到配置里的 UID/GID 默认标签。旁路存储不可用**绝不能**导致文件共享不可用。
func (s *boltMetadataStore) Get(key string) (Metadata, bool) {
	var (
		md Metadata
		ok bool
	)
	_ = s.ref.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadataBucket)
		if b == nil {
			return nil
		}
		md, ok = decodeMetadata(b.Get([]byte(key)))
		return nil
	})
	return md, ok
}

// Put 实现 MetadataStore。
func (s *boltMetadataStore) Put(key string, md Metadata) error {
	if key == "" {
		return ErrInvalidArg
	}
	return s.ref.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(metadataBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), encodeMetadata(md))
	})
}

// Delete 实现 MetadataStore，同时清理 key 的整棵子树。
//
// 删目录时必须把子树一起清掉，否则后续在同名路径下新建文件会读到
// 上一个对象的属主 —— 这是个真实会踩到的信息泄露/行为诡异问题。
func (s *boltMetadataStore) Delete(key string) error {
	if key == "" {
		return nil
	}
	return s.ref.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadataBucket)
		if b == nil {
			return nil
		}
		if err := b.Delete([]byte(key)); err != nil {
			return err
		}
		for _, k := range subtreeKeys(b, key) {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// Rename 实现 MetadataStore，把 oldKey 及其子树整体迁到 newKey 下。
func (s *boltMetadataStore) Rename(oldKey, newKey string) error {
	if oldKey == "" || newKey == "" || oldKey == newKey {
		return nil
	}
	return s.ref.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(metadataBucket)
		if b == nil {
			return nil
		}
		// 先把要搬的记录收集出来再改动 bucket：
		// bbolt 明确不允许在游标遍历过程中做写操作。
		moves := make(map[string][]byte)
		if v := b.Get([]byte(oldKey)); v != nil {
			moves[newKey] = append([]byte(nil), v...)
		}
		prefix := oldKey + "/"
		for _, k := range subtreeKeys(b, oldKey) {
			v := b.Get(k)
			if v == nil {
				continue
			}
			dst := newKey + "/" + strings.TrimPrefix(string(k), prefix)
			moves[dst] = append([]byte(nil), v...)
		}
		if len(moves) == 0 {
			return nil
		}
		// 目标可能是被覆盖的旧对象，连同它的子树一起清掉。
		if err := b.Delete([]byte(newKey)); err != nil {
			return err
		}
		for _, k := range subtreeKeys(b, newKey) {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		if err := b.Delete([]byte(oldKey)); err != nil {
			return err
		}
		for _, k := range subtreeKeys(b, oldKey) {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		for k, v := range moves {
			if err := b.Put([]byte(k), v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close 实现 MetadataStore。
//
// 按引用计数归还共享句柄：只有最后一个使用者才真的关库、还回 flock。
func (s *boltMetadataStore) Close() error {
	return releaseMetadataStore(s.path, s.ref)
}

// subtreeKeys 返回 key 的所有后代键（不含 key 自身）的**副本**。
//
// 必须复制：bbolt 游标返回的字节切片只在事务内有效，
// 而且我们随后要在同一个事务里改动 bucket。
func subtreeKeys(b *bolt.Bucket, key string) [][]byte {
	prefix := []byte(key + "/")
	var out [][]byte
	c := b.Cursor()
	for k, _ := c.Seek(prefix); k != nil && hasBytePrefix(k, prefix); k, _ = c.Next() {
		out = append(out, append([]byte(nil), k...))
	}
	return out
}

func hasBytePrefix(b, prefix []byte) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == string(prefix)
}

// encodeMetadata 把记录编成定长 12 字节（小端）。
func encodeMetadata(md Metadata) []byte {
	buf := make([]byte, metadataRecordLen)
	binary.LittleEndian.PutUint32(buf[0:4], md.UID)
	binary.LittleEndian.PutUint32(buf[4:8], md.GID)
	binary.LittleEndian.PutUint32(buf[8:12], md.Mode)
	return buf
}

// decodeMetadata 解析记录。长度不足一律当作「没有记录」而不是 panic
// （AGENTS.md §5「二进制解析必须先校验长度再切片」）。
func decodeMetadata(buf []byte) (Metadata, bool) {
	if len(buf) < metadataRecordLen {
		return Metadata{}, false
	}
	return Metadata{
		UID:  binary.LittleEndian.Uint32(buf[0:4]),
		GID:  binary.LittleEndian.Uint32(buf[4:8]),
		Mode: binary.LittleEndian.Uint32(buf[8:12]),
	}, true
}
