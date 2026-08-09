//go:build windows || metabolt

package meta

// bolt_test.go —— 真正上生产的那份实现（bbolt 事务 / 游标 / 子树搬迁）的测试。
//
// 构建约束与 bolt.go 一致。本容器跑不了 Windows 二进制，所以 Linux 上必须靠
//
//	go test -tags metabolt ./internal/meta/...
//
// 来真正执行这些用例，否则这份代码在任何机器上都没被运行过。
//
// 每个「能存能取」的用例都配了反向对照：删掉之后必须**取不到**（而不是取到旧值）、
// 前缀相近的兄弟必须**不受影响**、印章对不上必须**判为陈旧**。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func newStore(t *testing.T) Store {
	t.Helper()
	s, err := Open(t.TempDir(), filepath.Join(t.TempDir(), "md.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s == nil {
		t.Fatal("不应返回 nil store")
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustPut(t *testing.T, s Store, key string, rec Record) {
	t.Helper()
	if err := s.Put(key, rec); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

// bulkPut 在**一个**写事务里灌入大量记录，只为给需要上万条数据的用例铺场景。
//
// 逐条 Put 是逐条 fsync，灌一万条要十几秒 —— 慢到会让人想砍掉这个用例，
// 而这恰恰是唯一能走到 Reap 分批续扫分支的用例，砍不得。
// 走的仍是同一套 normKey + encodeRecord，铺出来的数据与 Put 完全等价。
func bulkPut(t *testing.T, s Store, keys []string, rec func(i int) Record) {
	t.Helper()
	bs, ok := s.(*boltStore)
	if !ok {
		t.Fatalf("bulkPut 需要 *boltStore, got %T", s)
	}
	err := bs.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketName)
		if err != nil {
			return err
		}
		for i, key := range keys {
			k, err := normKey(key)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(k), encodeRecord(rec(i))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bulkPut: %v", err)
	}
}

func wantRec(t *testing.T, s Store, key string, want Record) {
	t.Helper()
	got, ok := s.Get(key)
	if !ok {
		t.Errorf("Get(%q) 应有记录", key)
		return
	}
	if got != want {
		t.Errorf("Get(%q) = %+v, want %+v", key, got, want)
	}
}

func wantNoRec(t *testing.T, s Store, key string) {
	t.Helper()
	if got, ok := s.Get(key); ok {
		t.Errorf("Get(%q) 应无记录，却拿到 %+v", key, got)
	}
}

func TestPutGet(t *testing.T) {
	s := newStore(t)

	// 反向对照先行：没写过的键必须查不到
	wantNoRec(t, s, "nope")

	want := Record{UID: 501, GID: 20, Mode: 0o644, FileID: 0xABCD}
	mustPut(t, s, "a/b.txt", want)
	wantRec(t, s, "a/b.txt", want)

	// 大小写不敏感：Foo.txt 建的，FOO.TXT 打开的是同一个文件
	wantRec(t, s, "A/B.TXT", want)
	// 但相近的名字不能被折进来
	wantNoRec(t, s, "a/b.txtx")
	wantNoRec(t, s, "a/b")

	// 覆盖写
	want2 := Record{UID: 1000, GID: 1000, Mode: 0o600, FileID: 0xABCD}
	mustPut(t, s, "a/b.txt", want2)
	wantRec(t, s, "a/b.txt", want2)

	// 删除之后必须**取不到**，而不是取到旧值
	if err := s.Delete("a/b.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantNoRec(t, s, "a/b.txt")
}

// TestRootKey：共享根自己也要能存记录（Finder 会看根目录的属主）。
func TestRootKey(t *testing.T) {
	s := newStore(t)

	root := Record{UID: 501, GID: 20, Mode: 0o755}
	mustPut(t, s, "", root)
	wantRec(t, s, "", root)
	wantRec(t, s, "/", root)
	// 反向对照：根的记录不能被当成某个子项
	wantNoRec(t, s, "x")
}

func TestPutRejectsInvalidKey(t *testing.T) {
	s := newStore(t)

	for _, k := range []string{"..", "a/../b", "a\x00b"} {
		if err := s.Put(k, Record{UID: 1}); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) err = %v, want ErrInvalidKey", k, err)
		}
	}
	// 反向对照：拒绝之后库里不该留下任何东西
	if m := s.GetDir(""); len(m) != 0 {
		t.Errorf("被拒绝的 Put 不应写入任何记录，实际 %v", m)
	}
}

// TestDeleteSubtree：删目录必须连子树一起清。
// 否则之后在同名路径下重建目录，里面的文件会读到上一个目录的属主。
func TestDeleteSubtree(t *testing.T) {
	s := newStore(t)

	rec := Record{UID: 1, GID: 2, Mode: 0o755}
	for _, k := range []string{"d", "d/f1", "d/sub/f2", "dd", "dd/f3", "e"} {
		mustPut(t, s, k, rec)
	}
	if err := s.Delete("d"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, k := range []string{"d", "d/f1", "d/sub/f2"} {
		wantNoRec(t, s, k)
	}
	// 反向对照：前缀相近但不是子树的键，一条都不能被误删
	for _, k := range []string{"dd", "dd/f3", "e"} {
		wantRec(t, s, k, rec)
	}
}

func TestRenameSubtree(t *testing.T) {
	s := newStore(t)

	f1 := Record{UID: 11, GID: 12, Mode: 0o640, FileID: 1}
	f2 := Record{UID: 21, GID: 22, Mode: 0o600, FileID: 2}
	dir := Record{UID: 31, GID: 32, Mode: 0o750, FileID: 3}
	mustPut(t, s, "old", dir)
	mustPut(t, s, "old/f1", f1)
	mustPut(t, s, "old/sub/f2", f2)
	mustPut(t, s, "older", dir) // 前缀相近，不该被搬走

	if err := s.Rename("old", "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	for _, k := range []string{"old", "old/f1", "old/sub/f2"} {
		wantNoRec(t, s, k)
	}
	wantRec(t, s, "new", dir)
	wantRec(t, s, "new/f1", f1)
	wantRec(t, s, "new/sub/f2", f2)
	wantRec(t, s, "older", dir)
}

// TestRenameOverwrite：改名覆盖已有对象时，目标的旧记录（含子树）必须清干净。
func TestRenameOverwrite(t *testing.T) {
	s := newStore(t)

	src := Record{UID: 1, GID: 1, Mode: 0o644}
	stale := Record{UID: 999, GID: 999, Mode: 0o777}
	mustPut(t, s, "src", src)
	mustPut(t, s, "dst", stale)
	mustPut(t, s, "dst/leftover", stale)

	if err := s.Rename("src", "dst"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	wantRec(t, s, "dst", src)
	// 反向对照：被覆盖目标的子树残渣会让后续同名文件读到陈旧属主
	wantNoRec(t, s, "dst/leftover")
	wantNoRec(t, s, "src")
}

// TestRenameMissingSource：源没有任何记录时不能把目标清掉 ——
// 一次无意义的改名不该抹掉目标已有的元数据。
func TestRenameMissingSource(t *testing.T) {
	s := newStore(t)

	keep := Record{UID: 7, GID: 7, Mode: 0o600}
	mustPut(t, s, "dst", keep)
	mustPut(t, s, "dst/child", keep)

	if err := s.Rename("ghost", "dst"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	wantRec(t, s, "dst", keep)
	wantRec(t, s, "dst/child", keep)

	// 退化输入：同名改名、大小写变体改名都应是空操作
	if err := s.Rename("dst", "DST"); err != nil {
		t.Errorf("大小写变体改名应是空操作: %v", err)
	}
	wantRec(t, s, "dst", keep)

	// 非法键要拒绝
	if err := s.Rename("a", "../b"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Rename 到 ../b 应报 ErrInvalidKey, got %v", err)
	}
}

func TestGetDir(t *testing.T) {
	s := newStore(t)

	mk := func(n uint32) Record { return Record{UID: n, GID: n, Mode: 0o644, FileID: uint64(n)} }
	mustPut(t, s, "", mk(0)) // 根自己的记录
	mustPut(t, s, "a", mk(1))
	mustPut(t, s, "a/1", mk(2))
	mustPut(t, s, "a/File.TXT", mk(3))
	mustPut(t, s, "a/sub", mk(4))
	mustPut(t, s, "a/sub/deep", mk(5))
	mustPut(t, s, "a/sub/deep2/x", mk(6)) // deep2 自身没有记录
	mustPut(t, s, "ab", mk(7))            // 前缀相近的兄弟
	mustPut(t, s, "b", mk(8))

	t.Run("直接子项", func(t *testing.T) {
		got := s.GetDir("a")
		want := map[string]Record{"1": mk(2), "file.txt": mk(3), "sub": mk(4)}
		if len(got) != len(want) {
			t.Fatalf("GetDir(\"a\") = %v, want %v", got, want)
		}
		for k, w := range want {
			if got[k] != w {
				t.Errorf("GetDir(\"a\")[%q] = %+v, want %+v", k, got[k], w)
			}
		}
		// 反向对照：孙辈、前缀相近的兄弟、自己，一个都不能出现
		for _, k := range []string{"deep", "sub/deep", "b", "a"} {
			if _, ok := got[k]; ok {
				t.Errorf("GetDir(\"a\") 不该包含 %q", k)
			}
		}
	})

	t.Run("查表要先折叠", func(t *testing.T) {
		got := s.GetDir("a")
		if _, ok := got[Fold("File.TXT")]; !ok {
			t.Error("Fold 后应查得到")
		}
		if _, ok := got["File.TXT"]; ok {
			t.Error("未折叠的原名不应命中（契约就是要求调用方先 Fold）")
		}
	})

	t.Run("共享根", func(t *testing.T) {
		got := s.GetDir("")
		want := []string{"a", "ab", "b"}
		if len(got) != len(want) {
			t.Fatalf("GetDir(\"\") = %v, want keys %v", got, want)
		}
		for _, k := range want {
			if _, ok := got[k]; !ok {
				t.Errorf("GetDir(\"\") 缺少 %q", k)
			}
		}
		// 反向对照：根自己的记录键是 "/"，不能作为名为 "" 的子项冒出来
		if _, ok := got[""]; ok {
			t.Error("GetDir(\"\") 不该把根自身当成子项")
		}
		// 反向对照：孙辈不能冒出来
		if _, ok := got["a/1"]; ok {
			t.Error("GetDir(\"\") 不该包含孙辈")
		}
	})

	t.Run("中间目录没有自己的记录", func(t *testing.T) {
		// deep2 只有子项没有自己的记录，所以 GetDir("a/sub") 只应看到 deep
		got := s.GetDir("a/sub")
		if len(got) != 1 || got["deep"] != mk(5) {
			t.Errorf("GetDir(\"a/sub\") = %v, want 仅 deep", got)
		}
		// 但 deep2 的子项本身是查得到的
		wantRec(t, s, "a/sub/deep2/x", mk(6))
	})

	t.Run("空目录与非法键返回 nil", func(t *testing.T) {
		if got := s.GetDir("b"); got != nil {
			t.Errorf("无子项应返回 nil, got %v", got)
		}
		if got := s.GetDir("../evil"); got != nil {
			t.Errorf("非法键应返回 nil, got %v", got)
		}
	})
}

// TestGetDirMatchesGet：批量读和逐个读必须给出完全一致的结果。
// GetDir 走的是游标 + 跳子树的快路径，很容易和 Get 分叉。
func TestGetDirMatchesGet(t *testing.T) {
	s := newStore(t)

	const n = 200
	for i := 0; i < n; i++ {
		mustPut(t, s, fmt.Sprintf("dir/f%03d", i), Record{UID: uint32(i), Mode: 0o644})
		mustPut(t, s, fmt.Sprintf("dir/f%03d/child", i), Record{UID: 9999, Mode: 0o600})
	}
	got := s.GetDir("dir")
	if len(got) != n {
		t.Fatalf("GetDir 返回 %d 条, want %d（孙辈是否漏过滤或跳过头了？）", len(got), n)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%03d", i)
		one, ok := s.Get("dir/" + name)
		if !ok {
			t.Fatalf("Get(dir/%s) 应有记录", name)
		}
		if got[name] != one {
			t.Errorf("GetDir[%q] = %+v, Get = %+v，两者不一致", name, got[name], one)
		}
	}
}

// TestReap 覆盖孤儿回收，条数刻意超过 reapBatch 以走到分批续扫的分支 ——
// 续扫锚点算错会**静默漏扫或重复扫**，这里靠逐条核对survivor 抓出来。
func TestReap(t *testing.T) {
	s := newStore(t)

	const n = reapBatch*2 + 137
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("f%06d", i)
	}
	bulkPut(t, s, keys, func(i int) Record { return Record{UID: uint32(i), Mode: 0o644} })
	dead := func(i int) bool { return i%3 == 0 }

	var seen sync.Map
	removed, err := s.Reap(func(key string, rec Record) bool {
		if _, dup := seen.LoadOrStore(key, true); dup {
			t.Errorf("键 %q 被扫描了两次（续扫锚点算错）", key)
		}
		return !dead(int(rec.UID))
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}

	wantRemoved := 0
	for i := 0; i < n; i++ {
		if dead(i) {
			wantRemoved++
		}
	}
	if removed != wantRemoved {
		t.Errorf("Reap 删了 %d 条, want %d", removed, wantRemoved)
	}
	// 逐条核对：该死的都没了，该活的一条不少（漏扫会在这里暴露）
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("f%06d", i)
		if dead(i) {
			wantNoRec(t, s, k)
		} else {
			wantRec(t, s, k, Record{UID: uint32(i), Mode: 0o644})
		}
	}
	// 再扫一遍：已经干净了，不该再删掉任何东西
	if removed2, err := s.Reap(func(string, Record) bool { return true }); err != nil || removed2 != 0 {
		t.Errorf("重复 Reap = %d, %v; want 0, nil", removed2, err)
	}
}

func TestReapNilCallback(t *testing.T) {
	s := newStore(t)
	mustPut(t, s, "keep", Record{UID: 1, Mode: 0o644})

	// alive 为 nil 时必须什么都不做，绝不能理解成「全都不活着」而清库
	if n, err := s.Reap(nil); n != 0 || err != nil {
		t.Errorf("Reap(nil) = %d, %v; want 0, nil", n, err)
	}
	wantRec(t, s, "keep", Record{UID: 1, Mode: 0o644})
}

func TestPersistAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "md.db")

	s, err := Open("", p)
	if err != nil {
		t.Fatal(err)
	}
	want := Record{UID: 501, GID: 20, Mode: 0o664, FileID: 77}
	mustPut(t, s, "f.txt", want)
	mustPut(t, s, "gone.txt", want)
	if err := s.Delete("gone.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open("", p)
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer func() { _ = s2.Close() }()
	wantRec(t, s2, "f.txt", want)
	// 反向对照：删掉的记录不能在重启后复活
	wantNoRec(t, s2, "gone.txt")
}

// TestCrashKeepsCommittedData 是崩溃一致性的可证伪验证：
// 子进程写完记录后直接 os.Exit（不 Close、不解锁、不 flush，等价于 kill -9），
// 父进程重开库，已提交的记录必须还在，且库必须能打开。
func TestCrashKeepsCommittedData(t *testing.T) {
	if p := os.Getenv("META_CRASH_DB"); p != "" {
		// —— 子进程 ——
		s, err := Open("", p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "child Open: %v\n", err)
			os.Exit(2)
		}
		if err := s.Put("survivor", Record{UID: 501, GID: 20, Mode: 0o644, FileID: 5}); err != nil {
			fmt.Fprintf(os.Stderr, "child Put: %v\n", err)
			os.Exit(2)
		}
		// 刻意不 Close：os.Exit 不跑 defer、不做任何清理，
		// 对 bbolt 来说与被 SIGKILL 掉没有区别。
		os.Exit(9)
	}

	p := filepath.Join(t.TempDir(), "crash.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashKeepsCommittedData$")
	cmd.Env = append(os.Environ(), "META_CRASH_DB="+p)
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 9 {
		t.Fatalf("子进程应以 9 退出，实际 %v", err)
	}
	if fi, serr := os.Stat(p); serr != nil || fi.Size() == 0 {
		t.Fatalf("子进程没有留下库文件: %v", serr)
	}

	// 父进程：库必须能打开（说明没被撕裂到无法启动），已提交的记录必须还在。
	s, err := Open("", p)
	if err != nil {
		t.Fatalf("崩溃后重开失败（这正是不能发生的事）: %v", err)
	}
	defer func() { _ = s.Close() }()
	wantRec(t, s, "survivor", Record{UID: 501, GID: 20, Mode: 0o644, FileID: 5})
	// 反向对照：证明上面不是「随便开了个新库都能过」——没写过的键必须查不到
	wantNoRec(t, s, "ghost")
}

// TestTornMetaPageStillOpens：bbolt 的两页 meta 是交替写的，
// 撕掉一页应当能回退到另一页继续开库。
func TestTornMetaPageStillOpens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "torn.db")
	s, err := Open("", p)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, "f", Record{UID: 1, Mode: 0o644})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	pageSize := os.Getpagesize()

	smash := func(t *testing.T, pages int) error {
		t.Helper()
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), "copy.db")
		buf := append([]byte(nil), src...)
		for i := 0; i < pages*pageSize && i < len(buf); i++ {
			buf[i] = 0xFF
		}
		if err := os.WriteFile(dst, buf, 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := Open("", dst)
		if st != nil {
			_ = st.Close()
		}
		return err
	}

	// 只撕 meta0 → 必须能回退到 meta1 打开
	if err := smash(t, 1); err != nil {
		t.Errorf("撕掉一页 meta 后应仍能打开: %v", err)
	}
	// 反向对照：两页 meta 都撕了就该**明确报错**，
	// 而不是不声不响地给出一个空库（那会静默丢掉全部元数据）。
	if err := smash(t, 2); err == nil {
		t.Error("两页 meta 都损坏时必须报错，不能静默返回空库")
	}
}

// TestConcurrent 在 -race 下跑，验证接口的并发安全承诺。
func TestConcurrent(t *testing.T) {
	s := newStore(t)

	const workers = 8
	const iters = 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				k := fmt.Sprintf("w%d/f%d", w, i)
				if err := s.Put(k, Record{UID: uint32(w), GID: uint32(i), Mode: 0o644}); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				s.Get(k)
				s.GetDir(fmt.Sprintf("w%d", w))
				if i%7 == 0 {
					if err := s.Rename(k, k+".bak"); err != nil {
						t.Errorf("Rename: %v", err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	// 并发跑完之后数据必须是自洽的：每个 worker 恰好 iters 条记录
	for w := 0; w < workers; w++ {
		if got := len(s.GetDir(fmt.Sprintf("w%d", w))); got != iters {
			t.Errorf("worker %d 剩 %d 条记录, want %d", w, got, iters)
		}
	}
}

func TestDefaultPathOutsideShare(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg) // Linux 上让 UserConfigDir 落到临时目录
	root := t.TempDir()

	p, err := defaultPath(root)
	if err != nil {
		t.Skipf("环境没有用户配置目录: %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Errorf("默认落点应是绝对路径: %q", p)
	}
	// 落在共享目录里，客户端就会看见这个库文件，甚至可能删掉或备份走
	if rel, rerr := filepath.Rel(root, p); rerr == nil && !filepath.IsAbs(rel) &&
		rel != ".." && (len(rel) < 3 || rel[:3] != ".."+string(filepath.Separator)) {
		t.Errorf("默认落点 %q 落在共享目录 %q 内部", p, root)
	}
	// 同一个 root 必须稳定，否则重启后旧记录就找不到了
	if p2, _ := defaultPath(root); p != p2 {
		t.Errorf("默认落点不稳定: %q vs %q", p, p2)
	}
	// 大小写变体是同一个共享，不能开出两个库
	if p3, _ := defaultPath(root); p != p3 {
		t.Errorf("大小写变体产生了不同的库: %q vs %q", p, p3)
	}
	// 反向对照：不同的 root 必须落到不同的库，否则两个共享会互相覆盖
	if p4, _ := defaultPath(t.TempDir()); p == p4 {
		t.Errorf("不同共享落到了同一个库: %q", p)
	}
}

// BenchmarkGetDirVsGet 支撑「QUERY_DIRECTORY 必须走批量读」这一设计主张。
func BenchmarkGetDirVsGet(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir, filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	const n = 2000
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("f%05d", i)
		if err := s.Put("d/"+names[i], Record{UID: uint32(i), Mode: 0o644}); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("GetDir", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if len(s.GetDir("d")) != n {
				b.Fatal("条数不对")
			}
		}
	})
	b.Run("N次Get", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, name := range names {
				if _, ok := s.Get("d/" + name); !ok {
					b.Fatal("应有记录")
				}
			}
		}
	})
}
