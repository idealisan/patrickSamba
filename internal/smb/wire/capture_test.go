package wire

// 真实 Samba 抓包比对（AGENTS.md §3「关键路径要有抓包比对」）。
//
// 为什么需要这个：其余单测都是「自己编码 → 自己解码 → 对得上」，
// 这只能证明 wire 包**自洽**，证明不了真实客户端/服务器能看懂我们的字节。
// 这里的 fixture 是把真实 smbclient / go-smb2 与真实 smbd 之间的流量
// 逐帧转储下来的（采集方式见 test/capture/proxy.go 与 test/capture/capture.sh），
// 于是可以做两件单测做不到的事：
//
//	方向一（解析）：真实客户端请求 → ParseXxxRequest 必须成功，字段值合理。
//	方向二（编码）：真实 smbd 响应 → Parse → Append → **必须逐字节等于原始报文**。
//
// 方向二是强约束：它等价于「给定同样的字段值，我们编出来的字节和 Samba
// 一模一样」，比手工挑几个字段 diff 严格得多，而且不需要伪造 ServerGuid、
// SystemTime 这类服务端私有值 —— 它们从真实报文里解析出来后原样写回。
// 真正对不上的地方在 exemptions 里逐条列出并说明原因。
//
// 本测试**不需要**本机装 samba：fixture 已经提交进仓库。

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const captureRoot = "testdata/capture"

// captureFrame 是一帧抓包。Data 是去掉 4 字节 Direct TCP 长度前缀之后的报文体。
type captureFrame struct {
	scenario string
	file     string // 形如 002-s2c-NEGOTIATE.bin
	response bool   // 由文件名里的 s2c 决定，仅用于挑选，不参与断言
	data     []byte
}

func (f captureFrame) String() string { return f.scenario + "/" + f.file }

// loadCapture 读出所有场景的所有帧。
func loadCapture(t *testing.T) []captureFrame {
	t.Helper()
	scenarios, err := os.ReadDir(captureRoot)
	if err != nil {
		t.Fatalf("读 %s: %v（fixture 应当随代码一起提交）", captureRoot, err)
	}
	var frames []captureFrame
	for _, s := range scenarios {
		if !s.IsDir() {
			continue
		}
		dir := filepath.Join(captureRoot, s.Name())
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读 %s: %v", dir, err)
		}
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".bin") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatalf("读 %s: %v", n, err)
			}
			frames = append(frames, captureFrame{
				scenario: s.Name(),
				file:     n,
				response: strings.Contains(n, "-s2c-"),
				data:     b,
			})
		}
	}
	if len(frames) == 0 {
		t.Fatalf("%s 下一帧都没有", captureRoot)
	}
	return frames
}

// TestCaptureFixturesSane 先确认 fixture 本身没坏：要么是 SMB2 报文，
// 要么是 SMB3 加密报文（TRANSFORM，wire 层不负责解密）。
func TestCaptureFixturesSane(t *testing.T) {
	frames := loadCapture(t)
	var smb2, transform int
	for _, f := range frames {
		switch {
		case IsTransform(f.data):
			transform++
			if !strings.Contains(f.file, "TRANSFORM") {
				t.Errorf("%s: 内容是 TRANSFORM 但文件名不是", f)
			}
		case IsSMB2(f.data):
			smb2++
			if _, err := ParseHeader(f.data); err != nil {
				t.Errorf("%s: ParseHeader: %v", f, err)
			}
		default:
			t.Errorf("%s: 既不是 SMB2 也不是 TRANSFORM，前 4 字节 % X", f, f.data[:min(4, len(f.data))])
		}
	}
	t.Logf("共 %d 帧：SMB2 明文 %d，SMB3 加密 %d", len(frames), smb2, transform)
	if smb2 == 0 {
		t.Fatal("没有任何明文 SMB2 帧，fixture 采集姿势不对")
	}
}

// TestCaptureHeaderRoundTrip 单独验证头：ParseHeader → Append 必须逐字节还原。
// 头是所有报文共用的，先把它钉死，后面的体比对才有意义。
func TestCaptureHeaderRoundTrip(t *testing.T) {
	for _, f := range loadCapture(t) {
		if !IsSMB2(f.data) {
			continue
		}
		for _, m := range splitChain(t, f) {
			h, err := ParseHeader(m)
			if err != nil {
				t.Errorf("%s: ParseHeader: %v", f, err)
				continue
			}
			if got := h.Append(nil); !bytes.Equal(got, m[:HeaderSize]) {
				t.Errorf("%s: 头 round-trip 不一致\n%s", f, hexDiff(m[:HeaderSize], got))
			}
		}
	}
}

// TestCaptureBodyRoundTrip 是核心用例：
// 真实报文 → Parse → Append → 必须逐字节等于原始报文。
func TestCaptureBodyRoundTrip(t *testing.T) {
	var checked, skipped, exempted int
	bad := map[string]int{}

	for _, f := range loadCapture(t) {
		if !IsSMB2(f.data) {
			continue
		}
		for _, m := range splitChain(t, f) {
			h, err := ParseHeader(m)
			if err != nil {
				t.Errorf("%s: ParseHeader: %v", f, err)
				continue
			}
			// 错误响应（体只有 9 字节的 ERROR_RESPONSE）与命令自身的响应结构不同，
			// 按 MS-SMB2 §2.2.2 统一走 ErrorResponse。
			// STATUS_MORE_PROCESSING_REQUIRED / STATUS_BUFFER_OVERFLOW 例外：
			// 它们仍然带正常的命令响应体。
			isErr := h.IsResponse() && isErrorStatus(h.Status)

			out, err := reencode(h, isErr, m)
			if err != nil {
				if isSkip(err) {
					skipped++
					continue
				}
				t.Errorf("%s [%s mid=%d]: %v", f, h.Command, h.MessageID, err)
				bad[h.Command.String()]++
				continue
			}
			checked++
			if bytes.Equal(out, m) {
				continue
			}
			if why := exempt(h, m, out); why != "" {
				exempted++
				t.Logf("%s [%s mid=%d]: 已豁免 —— %s", f, h.Command, h.MessageID, why)
				continue
			}
			t.Errorf("%s [%s mid=%d resp=%v]: 重新编码与真实字节不一致\n%s",
				f, h.Command, h.MessageID, h.IsResponse(), hexDiff(m, out))
			bad[h.Command.String()]++
		}
	}
	t.Logf("逐字节比对 %d 条消息：完全一致 %d，豁免 %d，跳过 %d 条（wire 层不解析的命令）",
		checked, checked-exempted-len(bad), exempted, skipped)
	if len(bad) > 0 {
		t.Logf("失败分布: %v", bad)
	}
	if checked < 100 {
		t.Fatalf("只比对了 %d 条消息，样本太少，fixture 可能没采全", checked)
	}
}

// isErrorStatus 判断该 NTSTATUS 是否意味着「响应体是 ERROR_RESPONSE」。
//
// MS-SMB2 §2.2.2：出错时服务端返回 ERROR_RESPONSE。有几个坑：
//   - STATUS_MORE_PROCESSING_REQUIRED (0xC0000016)：NTLM 第二段，
//     带**完整的** SESSION_SETUP Response，不是 ERROR_RESPONSE；
//   - STATUS_BUFFER_OVERFLOW (0x80000005)：READ/IOCTL 部分数据，带正常体；
//   - STATUS_PENDING (0x00000103)：严重级是 SUCCESS，看着像成功，
//     但它是**异步中间响应**（MS-SMB2 §3.3.4.2），体是 9 字节的 ERROR_RESPONSE，
//     头里带 SMB2_FLAGS_ASYNC_COMMAND + AsyncId。
//     抓包为证：testdata/capture/tree-connect/012-s2c-IOCTL.bin。
func isErrorStatus(status uint32) bool {
	switch status {
	case 0x00000000, 0xC0000016, 0x80000005:
		return false
	case 0x00000103: // STATUS_PENDING：异步中间响应，体是 ERROR_RESPONSE
		return true
	}
	// 其余：只有 ERROR/WARNING 级（最高位置位）才是错误。
	return status&0x80000000 != 0
}

// exempt 是**显式豁免清单**：列出我们与真实实现故意不一致、且确认无害的地方。
// want 是真实字节，got 是我们重新编码出来的。返回非空字符串表示豁免，内容是原因。
//
// 加新条目的门槛：必须先确认「不一致的是对端可以忽略的填充/冗余字节」，
// 而不是「我们算错了偏移或长度」。有疑问一律当成我们错（AGENTS.md §9）。
func exempt(h Header, want, got []byte) string {
	// —— 豁免 1：ERROR Response 的 ErrorData 占位字节内容未定义 ——
	//
	// MS-SMB2 §2.2.2 只要求「ByteCount 为 0 时 ErrorData 仍占 1 字节」，
	// 没规定这个字节的值。Samba 会把上一次用过的缓冲区残留写出去
	// （抓包里见过 0x21），我们写 0。对端按 ByteCount=0 处理，不会读它。
	// 例：testdata/capture/tree-connect/012-s2c-IOCTL.bin（STATUS_PENDING 中间响应）。
	if h.IsResponse() && len(want) == HeaderSize+9 && len(got) == len(want) &&
		bytes.Equal(want[:len(want)-1], got[:len(got)-1]) {
		return fmt.Sprintf("ERROR Response 占位字节：真实 0x%02X，我们 0x%02X（MS-SMB2 §2.2.2 未定义其值）",
			want[len(want)-1], got[len(got)-1])
	}

	// —— 豁免 2：smbclient 在 CREATE Request 文件名后多发一个 UTF-16 NUL ——
	//
	// NameLength 不包含这 2 字节，服务端按 NameLength 取名字，多出来的被忽略。
	// 这是 smbclient 的习惯，同一份抓包里的 go-smb2 客户端就不发
	// （testdata/capture/gosmb2/017-c2s-CREATE.bin 能逐字节对上）。
	// 我们作为服务端不需要复刻客户端的习惯。
	if !h.IsResponse() && h.Command == CommandCreate &&
		len(want) == len(got)+2 && bytes.Equal(want[:len(got)], got) &&
		want[len(want)-2] == 0 && want[len(want)-1] == 0 {
		return "smbclient 在 CREATE Request 文件名后多发 2 字节 UTF-16 NUL（不计入 NameLength）"
	}

	return ""
}

// errSkip 表示这个命令 wire 层有意不解析（LOCK / CHANGE_NOTIFY / OPLOCK_BREAK）。
type errSkip struct{ cmd Command }

func (e errSkip) Error() string { return "wire 层未实现该命令: " + e.cmd.String() }

func isSkip(err error) bool { _, ok := err.(errSkip); return ok }

// reencode 把一条完整消息（含 64 字节头）解析后重新编码出来。
func reencode(h Header, isErr bool, m []byte) ([]byte, error) {
	dst := h.Append(nil)

	if isErr {
		r, err := ParseErrorResponse(m)
		if err != nil {
			return nil, fmt.Errorf("ParseErrorResponse: %w", err)
		}
		return r.Append(dst)
	}

	resp := h.IsResponse()
	switch h.Command {
	case CommandNegotiate:
		if resp {
			r, err := ParseNegotiateResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseNegotiateResponse: %w", err)
			}
			return r.Append(dst)
		}
		r, err := ParseNegotiateRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseNegotiateRequest: %w", err)
		}
		return r.Append(dst)

	case CommandSessionSetup:
		if resp {
			r, err := ParseSessionSetupResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseSessionSetupResponse: %w", err)
			}
			return r.Append(dst)
		}
		r, err := ParseSessionSetupRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseSessionSetupRequest: %w", err)
		}
		return r.Append(dst)

	case CommandLogoff:
		if resp {
			r, err := ParseLogoffResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseLogoffResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseLogoffRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseLogoffRequest: %w", err)
		}
		return r.Append(dst), nil

	case CommandTreeConnect:
		if resp {
			r, err := ParseTreeConnectResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseTreeConnectResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseTreeConnectRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseTreeConnectRequest: %w", err)
		}
		return r.Append(dst)

	case CommandTreeDisconnect:
		if resp {
			r, err := ParseTreeDisconnectResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseTreeDisconnectResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseTreeDisconnectRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseTreeDisconnectRequest: %w", err)
		}
		return r.Append(dst), nil

	case CommandCreate:
		if resp {
			r, err := ParseCreateResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseCreateResponse: %w", err)
			}
			return r.Append(dst)
		}
		r, err := ParseCreateRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseCreateRequest: %w", err)
		}
		return r.Append(dst)

	case CommandClose:
		if resp {
			r, err := ParseCloseResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseCloseResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseCloseRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseCloseRequest: %w", err)
		}
		return r.Append(dst), nil

	case CommandFlush:
		if resp {
			r, err := ParseFlushResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseFlushResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseFlushRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseFlushRequest: %w", err)
		}
		return r.Append(dst), nil

	case CommandRead:
		if resp {
			r, err := ParseReadResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseReadResponse: %w", err)
			}
			return r.Append(dst)
		}
		r, err := ParseReadRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseReadRequest: %w", err)
		}
		return r.Append(dst)

	case CommandWrite:
		if resp {
			r, err := ParseWriteResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseWriteResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseWriteRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseWriteRequest: %w", err)
		}
		return r.Append(dst)

	case CommandIoctl:
		if resp {
			r, err := ParseIoctlResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseIoctlResponse: %w", err)
			}
			return r.Append(dst)
		}
		r, err := ParseIoctlRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseIoctlRequest: %w", err)
		}
		return r.Append(dst)

	case CommandCancel:
		r, err := ParseCancelRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseCancelRequest: %w", err)
		}
		return r.Append(dst), nil

	case CommandEcho:
		if resp {
			r, err := ParseEchoResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseEchoResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseEchoRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseEchoRequest: %w", err)
		}
		return r.Append(dst), nil

	case CommandQueryDirectory:
		if resp {
			r, err := ParseQueryDirectoryResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseQueryDirectoryResponse: %w", err)
			}
			return r.Append(dst)
		}
		r, err := ParseQueryDirectoryRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseQueryDirectoryRequest: %w", err)
		}
		return r.Append(dst)

	case CommandQueryInfo:
		if resp {
			r, err := ParseQueryInfoResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseQueryInfoResponse: %w", err)
			}
			return r.Append(dst)
		}
		r, err := ParseQueryInfoRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseQueryInfoRequest: %w", err)
		}
		return r.Append(dst)

	case CommandSetInfo:
		if resp {
			r, err := ParseSetInfoResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseSetInfoResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseSetInfoRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseSetInfoRequest: %w", err)
		}
		return r.Append(dst)

	case CommandLock:
		if resp {
			r, err := ParseLockResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseLockResponse: %w", err)
			}
			return r.Append(dst), nil
		}
		r, err := ParseLockRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseLockRequest: %w", err)
		}
		return r.Append(dst)

	case CommandChangeNotify:
		if resp {
			r, err := ParseChangeNotifyResponse(m)
			if err != nil {
				return nil, fmt.Errorf("ParseChangeNotifyResponse: %w", err)
			}
			return r.Append(dst)
		}
		r, err := ParseChangeNotifyRequest(m)
		if err != nil {
			return nil, fmt.Errorf("ParseChangeNotifyRequest: %w", err)
		}
		return r.Append(dst), nil

	case CommandOplockBreak:
		switch PeekOplockBreakKind(m) {
		case OplockBreakKindOplock:
			r, err := ParseOplockBreak(m)
			if err != nil {
				return nil, fmt.Errorf("ParseOplockBreak: %w", err)
			}
			return r.Append(dst), nil
		case OplockBreakKindLease:
			r, err := ParseLeaseBreakAck(m)
			if err != nil {
				return nil, fmt.Errorf("ParseLeaseBreakAck: %w", err)
			}
			return r.Append(dst), nil
		}
		// 44 字节的 Lease Break Notification 只会由服务端发出。
		r, err := ParseLeaseBreakNotification(m)
		if err != nil {
			return nil, fmt.Errorf("ParseLeaseBreakNotification: %w", err)
		}
		return r.Append(dst), nil
	}

	return nil, errSkip{h.Command}
}

// splitChain 把复合链拆成一条条独立消息。
//
// 链中每条消息的 NextCommand 是**相对本条消息头起点**的偏移；最后一条为 0，
// 其后的字节属于最后一条（可能有尾部对齐填充）。
func splitChain(t *testing.T, f captureFrame) [][]byte {
	t.Helper()
	var out [][]byte
	b := f.data
	for i := 0; ; i++ {
		if len(b) < HeaderSize {
			t.Errorf("%s: 复合链第 %d 条不足 64 字节", f, i)
			return out
		}
		next := int(binary.LittleEndian.Uint32(b[0x14:]))
		if next == 0 {
			out = append(out, b)
			return out
		}
		if next < HeaderSize || next > len(b) {
			t.Errorf("%s: 复合链第 %d 条 NextCommand=%d 越界（总长 %d）", f, i, next, len(b))
			return out
		}
		out = append(out, b[:next])
		b = b[next:]
		if i > 64 {
			t.Errorf("%s: 复合链过长，疑似成环", f)
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// 诊断输出
// ---------------------------------------------------------------------------

// hexDiff 打印两段字节的差异，只展示第一处不同附近的窗口，避免刷屏。
func hexDiff(want, got []byte) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "  长度: 真实=%d 我们=%d\n", len(want), len(got))
	at := -1
	for i := 0; i < len(want) && i < len(got); i++ {
		if want[i] != got[i] {
			at = i
			break
		}
	}
	if at < 0 {
		if len(want) == len(got) {
			return sb.String() + "  （内容相同）\n"
		}
		at = min(len(want), len(got))
	}
	fmt.Fprintf(&sb, "  首个不同字节在偏移 %d (0x%X)", at, at)
	if at >= HeaderSize {
		fmt.Fprintf(&sb, "，即报文体内偏移 %d", at-HeaderSize)
	}
	sb.WriteByte('\n')

	lo := max(0, at-16)
	hi := min(max(len(want), len(got)), at+48)
	sb.WriteString("  真实: " + hexWindow(want, lo, hi) + "\n")
	sb.WriteString("  我们: " + hexWindow(got, lo, hi) + "\n")
	return sb.String()
}

func hexWindow(b []byte, lo, hi int) string {
	if lo > len(b) {
		return "(超出末尾)"
	}
	hi = min(hi, len(b))
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%d..%d]", lo, hi)
	for i := lo; i < hi; i++ {
		fmt.Fprintf(&sb, " %02X", b[i])
	}
	return sb.String()
}
