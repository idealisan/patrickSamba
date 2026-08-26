package wire

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// 本文件钉的是 v0.5 读路径去双拷贝（perf-v050 报告 §4/§8 移交项）：
// ReserveReadResponse + Commit 组装的 READ Response 必须与经典
// ReadResponse.Append **逐字节一致** —— 线上序列不变是这次优化的
// 正确性底线，这里用穷举对照当机器证明。

// referenceAppend 是经典 Append 路径的等价调用（保持可读性）。
func referenceAppend(t *testing.T, prefix []byte, data []byte, remaining, flags uint32) []byte {
	t.Helper()
	out, err := (&ReadResponse{Data: data, DataRemaining: remaining, Flags: flags}).
		Append(append([]byte{}, prefix...))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return out
}

// TestReserveCommitMatchesAppend：满窗、部分填充、零填充三种定稿结果
// 都必须与 Append 逐字节一致；前缀（模拟 64 字节响应头 / 复合链前序
// 消息）之后预留同样必须对齐到正确位置。
func TestReserveCommitMatchesAppend(t *testing.T) {
	prefix := make([]byte, HeaderSize)
	for i := range prefix {
		prefix[i] = byte(i)
	}
	sizes := []int{1, 2, 7, 16, 63, 100, 4096, 1 << 20}

	for _, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i * 7)
		}
		for _, fill := range []string{"full", "partial", "zero"} {
			n := size
			switch fill {
			case "partial":
				n = size / 2
				if n == 0 {
					n = 1 // size==1 时退化为 full，见 sizes 首项
					if size == 1 {
						continue
					}
				}
			case "zero":
				n = 0
			}
			window := data[:n]

			want := referenceAppend(t, prefix, window, 0, 0)

			got0 := append([]byte{}, prefix...)
			out, rsv, err := ReserveReadResponse(got0, size)
			if err != nil {
				t.Fatalf("size=%d %s: Reserve: %v", size, fill, err)
			}
			if len(rsv.Data) != size {
				t.Fatalf("size=%d: 窗口长度 %d ≠ %d", size, len(rsv.Data), size)
			}
			copy(rsv.Data, window) // 模拟 ReadAt 直写窗口
			got, err := rsv.Commit(out, n)
			if err != nil {
				t.Fatalf("size=%d %s: Commit: %v", size, fill, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("size=%d %s: 字节序列不一致\n got=% x\nwant=% x", size, fill, got, want)
			}
		}
	}
}

// TestReserveZeroWindowMatchesAppend：maxData=0 的退化用法也必须与
// Append 的空数据输出一致（16 固定 + 1 占位）。
func TestReserveZeroWindowMatchesAppend(t *testing.T) {
	want := referenceAppend(t, nil, nil, 0, 0)
	out, rsv, err := ReserveReadResponse(nil, 0)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(rsv.Data) != 0 {
		t.Fatalf("窗口长度应为 0，实际 %d", len(rsv.Data))
	}
	got, err := rsv.Commit(out, 0)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("字节序列不一致:\n got=% x\nwant=% x", got, want)
	}
}

// TestReserveRejectsBadSizes：负数窗口必须被拒（先校验后切片，
// AGENTS.md §5）；超 uint32 的申请同样拒绝。
func TestReserveRejectsBadSizes(t *testing.T) {
	if _, _, err := ReserveReadResponse(nil, -1); err == nil {
		t.Fatal("负数窗口应报错")
	}
	if math.MaxUint32 <= math.MaxInt {
		if _, _, err := ReserveReadResponse(nil, math.MaxInt); err == nil {
			t.Fatal("超 uint32 的窗口应报错")
		}
	}
}

// TestCommitMisuseDetected：契约违例必须报错而不是产出错位报文。
func TestCommitMisuseDetected(t *testing.T) {
	out, rsv, err := ReserveReadResponse(make([]byte, HeaderSize), 64)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fill(t, rsv.Data)

	if _, err := rsv.Commit(out, -1); err == nil {
		t.Fatal("n<0 应报错")
	}
	if _, err := rsv.Commit(out, 65); err == nil {
		t.Fatal("n>窗口 应报错")
	}
	// 中途追加字节 = 缓冲不再是 Reserve 返回的那个 → 必须拒绝。
	if _, err := rsv.Commit(append(out, 0xAB), 64); err == nil {
		t.Fatal("缓冲被追加后 Commit 应报错")
	}
	// 部分定稿后缓冲已截短，再 Commit 长度对不上 → 拒绝。
	got, err := rsv.Commit(out, 10)
	if err != nil {
		t.Fatalf("部分 Commit 不应失败: %v", err)
	}
	if _, err := rsv.Commit(got, 64); err == nil {
		t.Fatal("重复 Commit 应报错")
	}
	want := referenceAppend(t, make([]byte, HeaderSize), rsv.Data[:10], 0, 0)
	if !bytes.Equal(got, want) {
		t.Fatal("部分 Commit 输出与 Append 不一致")
	}
}

func fill(t *testing.T, b []byte) {
	t.Helper()
	for i := range b {
		b[i] = byte(i + 1)
	}
}

// TestParsedReservedResponseRoundTrip：新路径产物要能被 ParseReadResponse
// 正确读回（字段位置没有写错——DataLength 尤其容易指错地方）。
func TestParsedReservedResponseRoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte("smb2"), 300) // 1200 B
	msg, rsv, err := ReserveReadResponse(make([]byte, HeaderSize), len(data))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	copy(rsv.Data, data)
	msg, err = rsv.Commit(msg, len(data))
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// StructureSize 与 DataOffset 必须在绝对偏移 64/66 处（含头）。
	if got := binary.LittleEndian.Uint16(msg[HeaderSize:]); got != 17 {
		t.Fatalf("StructureSize=%d，应为 17", got)
	}
	if got := msg[HeaderSize+2]; got != HeaderSize+16 {
		t.Fatalf("DataOffset=%#x，应为 %#x", got, HeaderSize+16)
	}
	r, err := ParseReadResponse(msg)
	if err != nil {
		t.Fatalf("ParseReadResponse: %v", err)
	}
	if !bytes.Equal(r.Data, data) {
		t.Fatalf("读回数据不一致：len=%d want=%d", len(r.Data), len(data))
	}
}

// ---------------------------------------------------------------------------
// 分配对照基准：旧路径（临时缓冲 + Append 二次拷贝）vs 新路径
// （Reserve + ReadAt 直写 + Commit）。运行：
//
//	go test ./internal/smb/wire/ -bench BenchmarkReadResponse -run '^$' -benchmem
// ---------------------------------------------------------------------------

func benchFill(b []byte) {
	for i := range b {
		b[i] = byte(i)
	}
}

// readInto 模拟一次 pread 直写窗口（memcpy 语义，与 os.File.ReadAt 的
// 用户态成本同级；不用逐字节循环 —— 那会把内存带宽测成 CPU 噪声）。
func readInto(dst, src []byte) {
	copy(dst, src)
}

func BenchmarkReadResponseClassic1MiB(b *testing.B) {
	data := make([]byte, 1<<20)
	benchFill(data)
	prefix := make([]byte, HeaderSize)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := (&ReadResponse{Data: data}).Append(prefix)
		if err != nil {
			b.Fatal(err)
		}
		sink = out
	}
}

func BenchmarkReadResponseClassic64KiB(b *testing.B) {
	data := make([]byte, 64<<10)
	benchFill(data)
	prefix := make([]byte, HeaderSize)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := (&ReadResponse{Data: data}).Append(prefix)
		if err != nil {
			b.Fatal(err)
		}
		sink = out
	}
}

func BenchmarkReadResponseReserved1MiB(b *testing.B) {
	data := make([]byte, 1<<20)
	benchFill(data)
	prefix := make([]byte, HeaderSize)
	b.SetBytes(1 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, rsv, err := ReserveReadResponse(prefix, 1<<20)
		if err != nil {
			b.Fatal(err)
		}
		readInto(rsv.Data, data) // 模拟 ReadAt 把文件内容直写进最终缓冲
		out, err = rsv.Commit(out, 1<<20)
		if err != nil {
			b.Fatal(err)
		}
		sink = out
	}
}

func BenchmarkReadResponseReserved64KiB(b *testing.B) {
	data := make([]byte, 64<<10)
	benchFill(data)
	prefix := make([]byte, HeaderSize)
	b.SetBytes(64 << 10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, rsv, err := ReserveReadResponse(prefix, 64<<10)
		if err != nil {
			b.Fatal(err)
		}
		readInto(rsv.Data, data)
		out, err = rsv.Commit(out, 64<<10)
		if err != nil {
			b.Fatal(err)
		}
		sink = out
	}
}

// sink 防止编译器把基准工作整段优化掉。
var sink []byte
