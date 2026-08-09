package wire

// 抓包比对第四层：**FileNetworkOpenInformation（class 34，MS-FSCC §2.4.29）**。
//
// 为什么它需要单独一个文件：抓包里**根本没有**这个类。
// smbclient 的 allinfo 走 FileAllInformation(18)，从不查 34；查 34 的是
// Windows 资源管理器和 macOS Finder（它们用它一次拿全基本属性，比 allinfo 便宜），
// 而本容器跑不了 mount.cifs（AGENTS.md §10.3），也没装 smbd 可以现抓。
// 于是这个"字段多、偏移密"的类成了整条 QUERY_INFO 线上唯一没有真实字节兜底的类。
//
// 解决办法：**借道 FileAllInformation**。
// 34 的 7 个字段全部能在 18 的载荷里找到同名同值的对应物：
//
//	FILE_ALL_INFORMATION（真实 Samba 字节）      FILE_NETWORK_OPEN_INFORMATION（我们编码）
//	  [ 0: 8) CreationTime          ────────────►  [ 0: 8) CreationTime
//	  [ 8:16) LastAccessTime        ────────────►  [ 8:16) LastAccessTime
//	  [16:24) LastWriteTime         ────────────►  [16:24) LastWriteTime
//	  [24:32) ChangeTime            ────────────►  [24:32) ChangeTime
//	  [32:36) FileAttributes        ──────┐        [32:40) AllocationSize  ◄──┐
//	  [36:40) Reserved                    │        [40:48) EndOfFile       ◄──┤
//	  [40:48) AllocationSize        ──────┼──────► [48:52) FileAttributes  ◄──┘
//	  [48:56) EndOfFile             ──────┘        [52:56) Reserved（必须为 0）
//
// 注意两个结构里 FileAttributes 与两个 Size 的**先后顺序是反的** —— 这正是最容易
// 写错的地方，也是本用例的主要价值：字段值全部取自真实 Samba 报文，一旦有人把
// AllocationSize / EndOfFile 写反、把 FileAttributes 放错偏移、或漏掉尾部 Reserved，
// 这里立刻炸。
//
// 诚实说明本用例证明不了什么：它验证的是「同一份真实值在 34 里的摆放位置」，
// 不是「Samba 对 class 34 的逐字节应答」。真要那一层，需要装 smbd + 一个会查 34
// 的客户端现抓；等哪天环境里有了 Windows/macOS 客户端的 pcap，应当补上。

import (
	"bytes"
	"testing"
)

// TestNetworkOpenInfoLayoutAgainstCapturedAllInfo 用真实 Samba 的
// FileAllInformation 载荷交叉验证 FileNetworkOpenInfo 的字段摆放。
func TestNetworkOpenInfoLayoutAgainstCapturedAllInfo(t *testing.T) {
	msgs := loadCaptureMsgs(t)

	// QUERY_INFO 响应体里不带 InfoType/FileInfoClass，必须回查请求。
	reqs := map[infoKey]*QueryInfoRequest{}
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryInfo || m.hdr.IsResponse() {
			continue
		}
		r, err := ParseQueryInfoRequest(m.data)
		if err != nil {
			continue
		}
		if r.InfoType == InfoTypeFile && r.FileClass() == FileAllInformation {
			reqs[infoKey{m.frame.scenario, m.hdr.MessageID}] = r
		}
	}
	if len(reqs) == 0 {
		t.Fatal("抓包里没有 FileAllInformation 查询，fixture 不对")
	}

	var checked int
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryInfo || !m.hdr.IsResponse() || m.hdr.Status != 0 {
			continue
		}
		if _, ok := reqs[infoKey{m.frame.scenario, m.hdr.MessageID}]; !ok {
			continue
		}
		resp, err := ParseQueryInfoResponse(m.data)
		if err != nil {
			t.Errorf("%s: ParseQueryInfoResponse: %v", m, err)
			continue
		}
		all := resp.Buffer
		// Basic(40) + Standard(24) 是本用例要取的全部字段所在区间。
		if len(all) < 56 {
			t.Errorf("%s: FileAllInformation 载荷只有 %d 字节，短于 Basic+Standard", m, len(all))
			continue
		}

		parsed, err := ParseFileAllInfo(all)
		if err != nil {
			t.Errorf("%s: ParseFileAllInfo: %v", m, err)
			continue
		}

		// 用真实值组装 class 34，编码后逐字段与真实字节比对。
		got := FileNetworkOpenInfo{
			CreationTime:   parsed.Basic.CreationTime,
			LastAccessTime: parsed.Basic.LastAccessTime,
			LastWriteTime:  parsed.Basic.LastWriteTime,
			ChangeTime:     parsed.Basic.ChangeTime,
			AllocationSize: parsed.Standard.AllocationSize,
			EndOfFile:      parsed.Standard.EndOfFile,
			FileAttributes: parsed.Basic.FileAttributes,
		}.Encode()

		if len(got) != FileNetworkOpenInfoSize {
			t.Errorf("%s: 编码 %d 字节, 期望 %d", m, len(got), FileNetworkOpenInfoSize)
			continue
		}

		// 四个时间戳在两个结构里偏移相同，可以整段比。
		if !bytes.Equal(got[0:32], all[0:32]) {
			t.Errorf("%s: 时间戳段与真实字节不一致\n%s", m, hexDiff(all[0:32], got[0:32]))
		}
		// 两个 Size 与 FileAttributes 的顺序不同，逐段比。
		for _, c := range []struct {
			name       string
			got, want  []byte
			gotOff     int
			sambaWhere string
		}{
			{"AllocationSize", got[32:40], all[40:48], 32, "FILE_ALL_INFORMATION[40:48]"},
			{"EndOfFile", got[40:48], all[48:56], 40, "FILE_ALL_INFORMATION[48:56]"},
			{"FileAttributes", got[48:52], all[32:36], 48, "FILE_ALL_INFORMATION[32:36]"},
		} {
			if !bytes.Equal(c.got, c.want) {
				t.Errorf("%s: %s（编码偏移 %d，真实值取自 %s）不一致\n%s",
					m, c.name, c.gotOff, c.sambaWhere, hexDiff(c.want, c.got))
			}
		}
		// 尾部 Reserved 必须为 0（MS-FSCC §2.4.29）。留脏数据会泄漏内存内容。
		if !bytes.Equal(got[52:56], []byte{0, 0, 0, 0}) {
			t.Errorf("%s: Reserved = % x, 必须全 0", m, got[52:56])
		}

		// 往返：解析回来要拿到同样的值（解析侧偏移写错同样会在这里暴露）。
		back, err := ParseFileNetworkOpenInfo(got)
		if err != nil {
			t.Errorf("%s: ParseFileNetworkOpenInfo: %v", m, err)
			continue
		}
		if back.AllocationSize != parsed.Standard.AllocationSize ||
			back.EndOfFile != parsed.Standard.EndOfFile ||
			back.FileAttributes != parsed.Basic.FileAttributes ||
			back.CreationTime != parsed.Basic.CreationTime ||
			back.LastAccessTime != parsed.Basic.LastAccessTime ||
			back.LastWriteTime != parsed.Basic.LastWriteTime ||
			back.ChangeTime != parsed.Basic.ChangeTime {
			t.Errorf("%s: 往返后字段不一致\n got  %+v\n want %+v/%+v",
				m, back, parsed.Basic, parsed.Standard)
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("一条 FileAllInformation 响应都没比对到，抓包关联可能出错")
	}
	t.Logf("借 FileAllInformation 的真实字节交叉验证 FileNetworkOpenInformation 布局 %d 条", checked)
}
