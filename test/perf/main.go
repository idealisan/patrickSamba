// Command perf 是 stupidSamba 的协议栈基准客户端。
//
// 基于第三方纯 Go 客户端栈 go-smb2 对真实服务端做端到端测量，
// 场景与口径对齐 test/reports/perf-v040-20260825.md（1 MiB chunk、
// 并发流各用独立 TCP 连接+会话+树连接），作为优化前后的统一度量。
// loopback 数字只用于相对比较，不代表真实网络部署的绝对吞吐。
//
// 用法：
//
//	go build -o /tmp/perf-bench ./test/perf
//	/tmp/perf-bench -server 127.0.0.1:4455 -mode all
//
// 输出：每轮一行 JSON（r0 为预热轮，分析时丢弃），末尾打印人读摘要。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hirochachacha/go-smb2"
)

var (
	serverAddr = flag.String("server", "127.0.0.1:4455", "SMB 服务端地址 host:port")
	share      = flag.String("share", "public", "共享名")
	user       = flag.String("user", "perf", "用户名")
	pass       = flag.String("pass", "perfpass123", "口令")
	domain     = flag.String("domain", "WORKGROUP", "域/工作组")

	mode      = flag.String("mode", "all", "场景: large | conc | small | all")
	size      = flag.Int64("size", 128<<20, "大文件总字节数")
	conc      = flag.Int("concurrency", 1, "大文件场景的并发流数")
	smallN    = flag.Int("files", 500, "小文件个数")
	smallSize = flag.Int("fsize", 8192, "小文件字节数")

	rounds = flag.Int("rounds", 3, "计量轮数（另有一轮预热 r0 丢弃）")
	tag    = flag.String("tag", "", "输出标签（如 git sha）")
)

const chunk = 1 << 20 // 1 MiB，与 v040 基线的 chunk 口径一致

// result 是单轮单相位的测量结果。
type result struct {
	Scenario string  `json:"scenario"`
	Tag      string  `json:"tag"`
	Round    int     `json:"round"`
	Phase    string  `json:"phase"` // write / read / create / delete
	Workers  int     `json:"workers,omitempty"`
	MBps     float64 `json:"mbps"`
	OpsPS    float64 `json:"ops_ps,omitempty"`
	P50us    float64 `json:"p50_us"`
	P95us    float64 `json:"p95_us"`
	// PDU 计数（本相位期间客户端收到的服务端响应帧数 / 发出的请求帧数）
	RxPDUs uint64 `json:"rx_pdus"`
	TxPDUs uint64 `json:"tx_pdus"`
}

func main() {
	flag.Parse()
	rand.Seed(42)

	switch *mode {
	case "large":
		runLarge()
	case "conc":
		*conc = 16
		runLarge()
	case "small":
		runSmall()
	case "all":
		runLarge()
		cc := *conc
		*conc = 16
		runLarge()
		*conc = cc
		runSmall()
	default:
		fmt.Fprintf(os.Stderr, "未知 mode: %s\n", *mode)
		os.Exit(2)
	}
}

// ---------------------------------------------------------------- 连接与会话

// pduCounter 包装 net.Conn，增量解析 Direct TCP 帧（4 字节头：0x00 + 3 字节大端长度，
// 与传输层封装无关，加密 TRANSFORM 帧同型），统计双向帧数与载荷字节数。
// 仅用于测量；不改变任何读写语义。
type pduCounter struct {
	net.Conn
	rx, tx frameState
}

type frameState struct {
	buf    []byte
	frames uint64
	bytes  uint64
}

func (fs *frameState) feed(b []byte) {
	fs.buf = append(fs.buf, b...)
	for len(fs.buf) >= 4 {
		n := int(fs.buf[1])<<16 | int(fs.buf[2])<<8 | int(fs.buf[3])
		if len(fs.buf) < 4+n {
			break
		}
		fs.frames++
		fs.bytes += uint64(n)
		fs.buf = fs.buf[4+n:]
	}
}

func (c *pduCounter) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.rx.feed(p[:n])
	}
	return n, err
}

func (c *pduCounter) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.tx.feed(p[:n])
	}
	return n, err
}

// sess 是一条已挂载共享的工作会话。
type sess struct {
	conn *pduCounter
	st   *smb2.Session
	fs   *smb2.Share
}

func connect() (*sess, error) {
	raw, err := net.Dial("tcp", *serverAddr)
	if err != nil {
		return nil, err
	}
	c := &pduCounter{Conn: raw}
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{User: *user, Password: *pass, Domain: *domain},
	}
	st, err := d.Dial(c)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("dial: %w", err)
	}
	fs, err := st.Mount(*share)
	if err != nil {
		_ = st.Logoff()
		raw.Close()
		return nil, fmt.Errorf("mount %s: %w", *share, err)
	}
	return &sess{conn: c, st: st, fs: fs}, nil
}

func (s *sess) close() {
	_ = s.fs.Umount()
	_ = s.st.Logoff()
	_ = s.conn.Close()
}

func (s *sess) removeQuiet(name string) { _ = s.fs.Remove(name) }

// ---------------------------------------------------------------- 工具

// pattern 模板：确定性伪随机内容，避免全零触发稀疏/去零等旁路。
var pattern []byte

func initPattern() {
	pattern = make([]byte, chunk)
	rnd := rand.New(rand.NewSource(20260826))
	rnd.Read(pattern)
}

type latencies []float64 // 单位 µs

func (ls latencies) p50() float64 { return ls.percentile(50) }
func (ls latencies) p95() float64 { return ls.percentile(95) }

func (ls latencies) percentile(p float64) float64 {
	if len(ls) == 0 {
		return 0
	}
	s := append(latencies(nil), ls...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p / 100)
	return s[i]
}

func emit(r result) { b, _ := json.Marshal(r); fmt.Println(string(b)) }

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// ---------------------------------------------------------------- 大文件

func runLarge() {
	initPattern()
	name := "perf_large.bin"
	workers := *conc
	if workers < 1 {
		workers = 1
	}
	perWorker := *size / int64(workers)

	scenario := fmt.Sprintf("large_c%d", workers)
	var wMBps, rMBps, wP50, wP95, rP50, rP95 []float64
	var wRx, wTx, rRx, rTx []uint64

	for r := 0; r <= *rounds; r++ {
		wr, rd := largeRound(name, workers, perWorker)
		record := func(round int) {
			emit(result{Scenario: scenario, Tag: *tag, Round: round, Phase: "write",
				Workers: workers, MBps: wr.mbps, P50us: wr.p50, P95us: wr.p95,
				RxPDUs: wr.rxPDUs, TxPDUs: wr.txPDUs})
			emit(result{Scenario: scenario, Tag: *tag, Round: round, Phase: "read",
				Workers: workers, MBps: rd.mbps, P50us: rd.p50, P95us: rd.p95,
				RxPDUs: rd.rxPDUs, TxPDUs: rd.txPDUs})
		}
		if r == 0 {
			record(0)
			continue
		}
		wMBps = append(wMBps, wr.mbps)
		rMBps = append(rMBps, rd.mbps)
		wP50 = append(wP50, wr.p50)
		wP95 = append(wP95, wr.p95)
		rP50 = append(rP50, rd.p50)
		rP95 = append(rP95, rd.p95)
		wRx = append(wRx, wr.rxPDUs)
		wTx = append(wTx, wr.txPDUs)
		rRx = append(rRx, rd.rxPDUs)
		rTx = append(rTx, rd.txPDUs)
		record(r)
	}

	fmt.Printf("SUMMARY %-12s write=%.1fMB/s [p50=%.0fus p95=%.0fus] read=%.1fMB/s [p50=%.0fus p95=%.0fus] rx_pdu(w/r)=%.0f/%.0f tx_pdu(w/r)=%.0f/%.0f\n",
		scenario, median(wMBps), median(wP50), median(wP95),
		median(rMBps), median(rP50), median(rP95),
		medianU64(wRx), medianU64(rRx), medianU64(wTx), medianU64(rTx))
}

type phaseStat struct {
	mbps, p50, p95 float64
	rxPDUs, txPDUs uint64
}

// largeRound 跑一轮「全部 worker 写完 → 全部 worker 读回校验」。
func largeRound(name string, workers int, perWorker int64) (wr, rd phaseStat) {
	ss := make([]*sess, workers)
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	errCh := make(chan error, workers*2)
	for i := range ss {
		s, err := connect()
		if err != nil {
			fmt.Fprintf(os.Stderr, "worker %d connect: %v\n", i, err)
			os.Exit(1)
		}
		defer s.close()
		ss[i] = s
	}

	// —— 写相位 ——
	rx0, tx0 := totalRX(ss), totalTX(ss)
	t0 := time.Now()
	var wLat latencies
	var wMu sync.Mutex
	for i, s := range ss {
		wg.Add(1)
		go func(id int, s *sess) {
			defer wg.Done()
			<-startBarrier
			workerName := name
			if workers > 1 {
				workerName = fmt.Sprintf("%s.w%02d", name, id)
			}
			s.removeQuiet(workerName)
			f, err := s.fs.OpenFile(workerName, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				errCh <- fmt.Errorf("create: %w", err)
				return
			}
			var la latencies
			for off := int64(0); off < perWorker; off += chunk {
				n := int64(chunk)
				if off+n > perWorker {
					n = perWorker - off
				}
				tb := time.Now()
				if _, err := f.WriteAt(pattern[:n], off); err != nil {
					errCh <- fmt.Errorf("write: %w", err)
					return
				}
				la = append(la, float64(time.Since(tb).Microseconds()))
			}
			if err := f.Close(); err != nil {
				errCh <- fmt.Errorf("close: %w", err)
				return
			}
			wMu.Lock()
			wLat = append(wLat, la...)
			wMu.Unlock()
		}(i, s)
	}
	close(startBarrier)
	wg.Wait()
	wall := time.Since(t0)
	if len(errCh) > 0 {
		fmt.Fprintf(os.Stderr, "写相位失败: %v\n", <-errCh)
		os.Exit(1)
	}
	totalBytes := perWorker * int64(workers)
	wr = phaseStat{
		mbps: float64(totalBytes) / (1024 * 1024) / wall.Seconds(),
		p50:  wLat.p50(), p95: wLat.p95(),
		rxPDUs: totalRX(ss) - rx0, txPDUs: totalTX(ss) - tx0,
	}

	// —— 读相位（逐字节校验）——
	rx0, tx0 = totalRX(ss), totalTX(ss)
	t0 = time.Now()
	var rLat latencies
	var rMu sync.Mutex
	got := make([]byte, chunk)
	for i, s := range ss {
		wg.Add(1)
		go func(id int, s *sess) {
			defer wg.Done()
			workerName := name
			if workers > 1 {
				workerName = fmt.Sprintf("%s.w%02d", name, id)
			}
			f, err := s.fs.Open(workerName)
			if err != nil {
				errCh <- fmt.Errorf("open: %w", err)
				return
			}
			var la latencies
			for off := int64(0); off < perWorker; off += chunk {
				n := int64(chunk)
				if off+n > perWorker {
					n = perWorker - off
				}
				tb := time.Now()
				k, err := f.ReadAt(got[:n], off)
				if err != nil {
					errCh <- fmt.Errorf("read: %w", err)
					return
				}
				la = append(la, float64(time.Since(tb).Microseconds()))
				if k != int(n) {
					errCh <- fmt.Errorf("短读 %d != %d", k, n)
					return
				}
				if string(got[:n]) != string(pattern[:n]) {
					errCh <- fmt.Errorf("数据校验失败 off=%d", off)
					return
				}
			}
			if err := f.Close(); err != nil {
				errCh <- fmt.Errorf("close: %w", err)
				return
			}
			rMu.Lock()
			rLat = append(rLat, la...)
			rMu.Unlock()
		}(i, s)
	}
	wg.Wait()
	wall = time.Since(t0)
	if len(errCh) > 0 {
		fmt.Fprintf(os.Stderr, "读相位失败: %v\n", <-errCh)
		os.Exit(1)
	}
	rd = phaseStat{
		mbps: float64(totalBytes) / (1024 * 1024) / wall.Seconds(),
		p50:  rLat.p50(), p95: rLat.p95(),
		rxPDUs: totalRX(ss) - rx0, txPDUs: totalTX(ss) - tx0,
	}

	// 清理本轮文件
	if workers == 1 {
		ss[0].removeQuiet(name)
	} else {
		for i, s := range ss {
			s.removeQuiet(fmt.Sprintf("%s.w%02d", name, i))
		}
	}
	return wr, rd
}

func totalRX(ss []*sess) uint64 {
	var v uint64
	for _, s := range ss {
		v += s.conn.rx.frames
	}
	return v
}

func totalTX(ss []*sess) uint64 {
	var v uint64
	for _, s := range ss {
		v += s.conn.tx.frames
	}
	return v
}

func medianU64(xs []uint64) float64 {
	f := make([]float64, len(xs))
	for i, x := range xs {
		f[i] = float64(x)
	}
	return median(f)
}

// ---------------------------------------------------------------- 小文件

func runSmall() {
	dir := "perf_small"
	scenario := "small"

	var cOps, dOps []float64
	var cP50, cP95, dP50, dP95 []float64

	for r := 0; r <= *rounds; r++ {
		cs, ds := smallRound(dir)
		record := func(round int) {
			emit(result{Scenario: scenario, Tag: *tag, Round: round, Phase: "create",
				OpsPS: cs.opsPS, P50us: cs.p50, P95us: cs.p95})
			emit(result{Scenario: scenario, Tag: *tag, Round: round, Phase: "delete",
				OpsPS: ds.opsPS, P50us: ds.p50, P95us: ds.p95})
		}
		if r == 0 {
			record(0)
			continue
		}
		cOps = append(cOps, cs.opsPS)
		cP50 = append(cP50, cs.p50)
		cP95 = append(cP95, cs.p95)
		dOps = append(dOps, ds.opsPS)
		dP50 = append(dP50, ds.p50)
		dP95 = append(dP95, ds.p95)
		record(r)
	}

	fmt.Printf("SUMMARY %-12s create=%.0fops/s [p50=%.0fus p95=%.0fus] delete=%.0fops/s [p50=%.0fus p95=%.0fus]\n",
		scenario, median(cOps), median(cP50), median(cP95),
		median(dOps), median(dP50), median(dP95))
}

type opStat struct {
	opsPS, p50, p95 float64
}

func smallRound(dir string) (cs, ds opStat) {
	s, err := connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer s.close()

	buf := make([]byte, *smallSize)
	for i := range buf {
		buf[i] = byte(i * 31)
	}

	_ = s.fs.Remove(dir) // 忽略不存在
	names := make([]string, *smallN)

	// create + write + close
	t0 := time.Now()
	var cLat latencies
	for i := 0; i < *smallN; i++ {
		names[i] = fmt.Sprintf("%s/f%04d.bin", dir, i)
		tb := time.Now()
		f, err := s.fs.Create(names[i])
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", names[i], err)
			os.Exit(1)
		}
		if _, err := f.Write(buf); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", names[i], err)
			os.Exit(1)
		}
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close %s: %v\n", names[i], err)
			os.Exit(1)
		}
		cLat = append(cLat, float64(time.Since(tb).Microseconds()))
	}
	wall := time.Since(t0)
	cs = opStat{
		opsPS: float64(*smallN) / wall.Seconds(),
		p50:   cLat.p50(), p95: cLat.p95(),
	}

	// delete-all
	t0 = time.Now()
	var dLat latencies
	for _, n := range names {
		tb := time.Now()
		if err := s.fs.Remove(n); err != nil {
			fmt.Fprintf(os.Stderr, "remove %s: %v\n", n, err)
			os.Exit(1)
		}
		dLat = append(dLat, float64(time.Since(tb).Microseconds()))
	}
	wall = time.Since(t0)
	ds = opStat{
		opsPS: float64(*smallN) / wall.Seconds(),
		p50:   dLat.p50(), p95: dLat.p95(),
	}
	_ = s.fs.Remove(dir)
	return cs, ds
}
