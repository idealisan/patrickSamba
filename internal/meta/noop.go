//go:build !windows && !metabolt

package meta

// noop.go —— 非 Windows 平台的空实现。
//
// Linux/macOS 的宿主文件系统原生就有 uid/gid/mode，旁路存储纯属多余
// （AGENTS.md §5 P7：「Linux/macOS 原生能力足够，使用 noop 实现，零开销」）。
//
// 本文件与 bolt.go 的构建约束互斥，所以在这些平台上 bbolt **根本不参与编译**，
// 二进制体积不受影响。

// Enabled 是编译期常量，表示本平台是否需要旁路元数据存储。
//
// 之所以做成常量而不是运行时判断：调用方写 `if meta.Enabled { ... }` 时，
// 整个分支会被编译器死代码消除，连一次接口方法调用的开销都没有。
const Enabled = false

// Open 在非 Windows 上返回一个空实现。
//
// 刻意返回 noopStore 而不是 nil：nil 接口一旦被漏判就是 panic，
// 而「零开销」这件事已经由 Enabled 常量在调用点解决了，不需要靠 nil 来省。
//
// 不建文件、不建目录、不起 goroutine。
func Open(root, path string) (Store, error) {
	return noopStore{}, nil
}

// noopStore 是空结构体，不占内存。
type noopStore struct{}

func (noopStore) Get(string) (Record, bool)       { return Record{}, false }
func (noopStore) GetDir(string) map[string]Record { return nil }
func (noopStore) Put(string, Record) error        { return nil }
func (noopStore) Delete(string) error             { return nil }
func (noopStore) Rename(string, string) error     { return nil }
func (noopStore) Close() error                    { return nil }
func (noopStore) Reap(func(string, Record) bool) (int, error) {
	return 0, nil
}
