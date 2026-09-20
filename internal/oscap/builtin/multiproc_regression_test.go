package builtin

// multiproc_regression_test.go —— OI-1 的回归测试：多个**真实 OS 进程**
// 共享同一 share 根目录时，portable/builtin 模式必须都能打开旁路存储。
//
// # 修复前的行为（可证伪点）
//
// bbolt 按 path 拿 flock。修复前所有进程都推导出同一个默认库文件，
// 第二个进程 bolt.Open 等满 5 秒锁超时后启动失败 ——
// scripts/acceptance.sh 起 3 个共享同一目录的服务进程时必然踩中。
// 修复机制：装配层给每个实例传不同的 InstanceID（监听端点）⇒ 各开各的库。
//
// # helper-process 模式
//
// 用 Go 标准做法：TestMain 检查环境变量，命中则本进程作为「子实例」执行
// 打开/写入/读取动作后直接退出，否则照常跑测试。测试代码里用 os/exec 是
// 允许的（C3 只约束产品代码；scripts/check-constraints.sh 也明确排除
// _test.go）。变异验证见 store_instance_test.go 同级的 overlay 流程：
// 删掉 InstanceID 区分逻辑后，本文件的两个用例必须变红。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idealisan/patrickSamba/internal/oscap"
	"github.com/idealisan/patrickSamba/internal/oscap/native"
)

// 子进程与父进程之间的约定。名字带全前缀，避免与其他包的环境变量撞车。
const (
	helperModeEnv = "STUPIDSAMBA_OSCAP_HELPER_MODE"
	helperRootEnv = "STUPIDSAMBA_OSCAP_HELPER_ROOT"
	helperInstEnv = "STUPIDSAMBA_OSCAP_HELPER_INST"
	helperValEnv  = "STUPIDSAMBA_OSCAP_HELPER_VAL"

	modeOpen  = "open"  // 打开后立刻关闭：只证明「抢得到锁」
	modeWrite = "write" // 打开 + 在共享内文件上写一条 xattr 标记
	modeRead  = "read"  // 打开 + 读那条标记并与 VAL 比对
	// modeServe 模拟**真实服务进程**的持有形态：打开后按 VAL（Go duration，
	// 默认略长于 openFlockTimeout）持有一段时间再关。修复前两个这样的进程
	// 并发启动，输家会卡满 5 秒 flock 超时后启动失败 —— 这正是 acceptance.sh
	// 起多进程时看到的原始故障。
	modeServe = "serve"

	markerXattr = "test.stupidsamba-marker"
	markerFile  = "zz_marker.txt"
)

func TestMain(m *testing.M) {
	switch mode := os.Getenv(helperModeEnv); mode {
	case "":
		os.Exit(m.Run())
	case modeOpen, modeWrite, modeRead, modeServe:
		os.Exit(runHelperProcess(mode))
	default:
		fmt.Fprintf(os.Stderr, "builtin: 未知的 helper 模式 %q\n", mode)
		os.Exit(2)
	}
}

// runHelperProcess 以「一个独立的服务实例」的身份打开旁路存储。
// 走 oscap.Open(ModePortable) 而不是直接 New：与 acceptance 场景同构，
// 把 Options → SelectMatrix → builtin 工厂整条链都覆盖进来。
func runHelperProcess(mode string) int {
	root := os.Getenv(helperRootEnv)
	inst := os.Getenv(helperInstEnv)
	val := os.Getenv(helperValEnv)
	if root == "" {
		fmt.Fprintln(os.Stderr, "helper: ROOT 为空")
		return 2
	}

	set, err := oscap.Open(oscap.ModePortable, oscap.Options{
		Root:       root,
		InstanceID: inst,
	}, native.New, New)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: 打开旁路存储失败（instance=%q）: %v\n", inst, err)
		return 1
	}
	defer func() { _ = set.Close() }()

	switch mode {
	case modeOpen:
		return 0
	case modeServe:
		d := openFlockTimeout + time.Second
		if v := os.Getenv(helperValEnv); v != "" {
			var err error
			if d, err = time.ParseDuration(v); err != nil {
				fmt.Fprintf(os.Stderr, "helper: 非法的持有时长 %q: %v\n", v, err)
				return 2
			}
		}
		time.Sleep(d)
		return 0
	case modeWrite:
		f := filepath.Join(root, markerFile)
		if err := os.WriteFile(f, []byte("m"), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "helper: 建标记文件失败: %v\n", err)
			return 3
		}
		if err := set.Xattr().SetXattr(oscap.Ref{Path: f}, markerXattr, []byte(val)); err != nil {
			fmt.Fprintf(os.Stderr, "helper: 写标记失败: %v\n", err)
			return 3
		}
		return 0
	case modeRead:
		v, err := set.Xattr().GetXattr(oscap.Ref{Path: filepath.Join(root, markerFile)}, markerXattr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: 读标记失败（instance=%q）: %v\n", inst, err)
			return 4
		}
		if string(v) != val {
			fmt.Fprintf(os.Stderr, "helper: 标记不符 got=%q want=%q\n", v, val)
			return 5
		}
		return 0
	}
	return 2
}

// runStoreHelper 重新 exec 测试二进制自身，把它变成一个指定模式的子实例。
func runStoreHelper(t *testing.T, ctx context.Context, root, inst, mode, val string) error {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(),
		helperModeEnv+"="+mode,
		helperRootEnv+"="+root,
		helperInstEnv+"="+inst,
		helperValEnv+"="+val,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("子实例(mode=%s inst=%q): %v\n%s", mode, inst, err, out)
	}
	return nil
}

// TestMultiProcessSharedRootEachOpensOwnStore：≥2 个真实 OS 进程在同一
// share 根上以 portable 模式并发打开旁路存储，全部成功且快速 ——
// 修复前第二个进程会卡满 5 秒 flock 超时后失败，两条判据都会红。
func TestMultiProcessSharedRootEachOpensOwnStore(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过多进程回归")
	}
	root := t.TempDir()
	insts := []string{"127.0.0.1:4461", "127.0.0.1:4462", "192.168.20.20:4463"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	errs := make(chan error, len(insts))
	var wg sync.WaitGroup
	for _, inst := range insts {
		wg.Add(1)
		go func(inst string) {
			defer wg.Done()
			errs <- runStoreHelper(t, ctx, root, inst, modeOpen, "")
		}(inst)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// 判据一（时间）：修复后各实例各开各的库，毫秒级完成；
	// 若仍在共享同一个库文件，输家要等满 openFlockTimeout（5 秒）才报错。
	if el := time.Since(start); el > 4*time.Second {
		t.Errorf("3 个实例并发打开耗时 %v —— 疑似仍在排队等同一把 flock", el)
	}

	// 判据二（落盘证据）：兄弟目录里应出现恰好 len(insts) 个互不相同的旁路库。
	parent := filepath.Dir(root)
	ents, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var dbs []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".stupidsamba-oscap-") && strings.HasSuffix(e.Name(), ".db") {
			dbs = append(dbs, e.Name())
		}
	}
	if len(dbs) != len(insts) {
		t.Errorf("预期 %d 个旁路库，实际 %d 个: %v", len(insts), len(dbs), dbs)
	}
}

// TestMultiProcessConcurrentServersHoldTheirOwnStores：两个**长期持有**
// 旁路存储的服务进程并发启动（真实服务器的形态），必须都能成功。
// 这是 OI-1 原始故障的直接复现：修复前两者算出同一个库文件，
// 输家等满 openFlockTimeout（5 秒）后 bolt.Open 报错退出；
// 修复后各开各的库，互不相干。判据是子进程的成败本身，
// 不看墙钟（持有时长本来就超过 flock 超时）。
func TestMultiProcessConcurrentServersHoldTheirOwnStores(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过多进程回归")
	}
	root := t.TempDir()
	insts := []string{"127.0.0.1:4481", "127.0.0.1:4482"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	errs := make(chan error, len(insts))
	var wg sync.WaitGroup
	for _, inst := range insts {
		wg.Add(1)
		go func(inst string) {
			defer wg.Done()
			errs <- runStoreHelper(t, ctx, root, inst, modeServe, "")
		}(inst)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// TestSameInstanceReusesStoreAcrossProcessesDifferentInstanceIsIsolated：
// 同一 InstanceID 的两个先后进程复用同一份元数据（写→读通）；
// 不同 InstanceID 读不到别人的标记（按实例分库后互相隔离）。
// 后半条同时是变异体的反向对照：若 InstanceID 区分被删掉，
// 「隔离」会消失而「复用」碰巧仍成立 —— 本用例的隔离断言负责变红。
func TestSameInstanceReusesStoreAcrossProcessesDifferentInstanceIsIsolated(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过多进程回归")
	}
	root := t.TempDir()
	const (
		inst  = "127.0.0.1:4471"
		other = "127.0.0.1:4472"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := runStoreHelper(t, ctx, root, inst, modeWrite, "v1"); err != nil {
		t.Fatalf("第一个实例写入失败: %v", err)
	}
	if err := runStoreHelper(t, ctx, root, inst, modeRead, "v1"); err != nil {
		t.Fatalf("同一实例重启后必须复用同一份元数据: %v", err)
	}
	if err := runStoreHelper(t, ctx, root, other, modeRead, "v1"); err == nil {
		t.Fatal("不同实例读到了别人的旁路库 —— 实例隔离失效")
	}
}
