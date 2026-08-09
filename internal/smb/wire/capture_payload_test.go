package wire

// 抓包比对第二层：**信息类载荷**的逐字节还原。
//
// capture_test.go 只比对了报文的**外层信封**（QUERY_INFO / SET_INFO /
// QUERY_DIRECTORY 的 offset/length 字段），里面的 OutputBuffer / Buffer 是当作
// 不透明 []byte 原样搬运的 —— 搬运当然能对上，这证明不了我们对
// FileAllInformation、FileStreamInformation、FileRenameInformation、
// FileIdBothDirectoryInformation 这些**结构本身**的理解是对的。
//
// 本文件补上这一层：把真实 Samba 4.22 报文里的载荷解析成结构体，再编码回去，
// 要求**逐字节等于原始载荷**。字段少写一个、Reserved 位置错一处、对齐算错，
// 这里都会立刻炸出来。
//
// 关联方式：请求与响应按「场景目录 + MessageId」配对，从请求里取
// InfoType / FileInfoClass，才知道响应的 OutputBuffer 该按哪个结构解。

import (
	"bytes"
	"fmt"
	"testing"
)

// captureMsg 是拆开复合链之后的一条消息。
type captureMsg struct {
	frame captureFrame
	hdr   Header
	data  []byte
}

func (m captureMsg) String() string {
	return fmt.Sprintf("%s [%s mid=%d]", m.frame, m.hdr.Command, m.hdr.MessageID)
}

// loadCaptureMsgs 读出所有帧并拆链，只保留明文 SMB2 消息。
func loadCaptureMsgs(t *testing.T) []captureMsg {
	t.Helper()
	var out []captureMsg
	for _, f := range loadCapture(t) {
		if !IsSMB2(f.data) {
			continue
		}
		for _, m := range splitChain(t, f) {
			h, err := ParseHeader(m)
			if err != nil {
				continue
			}
			out = append(out, captureMsg{frame: f, hdr: h, data: m})
		}
	}
	return out
}

// infoKey 唯一标识一次请求/响应配对：同一场景目录内 MessageId 不重复。
type infoKey struct {
	scenario string
	msgID    uint64
}

// TestCaptureQueryInfoPayload 比对 QUERY_INFO 响应里 OutputBuffer 的结构还原。
//
// 覆盖的类来自 `smbclient allinfo`：FileAllInformation(18)、
// FileStreamInformation(22)、FileBasicInformation(4) 等。
func TestCaptureQueryInfoPayload(t *testing.T) {
	msgs := loadCaptureMsgs(t)

	// 先建请求索引：响应体里没有 InfoType/FileInfoClass，必须回查请求。
	reqs := map[infoKey]*QueryInfoRequest{}
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryInfo || m.hdr.IsResponse() {
			continue
		}
		r, err := ParseQueryInfoRequest(m.data)
		if err != nil {
			t.Errorf("%s: ParseQueryInfoRequest: %v", m, err)
			continue
		}
		reqs[infoKey{m.frame.scenario, m.hdr.MessageID}] = r
	}
	if len(reqs) == 0 {
		t.Fatal("抓包里没有 QUERY_INFO 请求，fixture 不对")
	}

	seen := map[string]int{}
	var checked, skipped int
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryInfo || !m.hdr.IsResponse() {
			continue
		}
		// 只比对成功且带完整载荷的响应。
		if m.hdr.Status != 0 {
			continue
		}
		req, ok := reqs[infoKey{m.frame.scenario, m.hdr.MessageID}]
		if !ok {
			t.Errorf("%s: 找不到对应的请求", m)
			continue
		}
		resp, err := ParseQueryInfoResponse(m.data)
		if err != nil {
			t.Errorf("%s: ParseQueryInfoResponse: %v", m, err)
			continue
		}
		name, got, err := reencodeInfoPayload(req.InfoType, req.FileInfoClass, resp.Buffer)
		if err != nil {
			t.Errorf("%s: %v", m, err)
			continue
		}
		if name == "" {
			// 本类只有编码侧、没有解析函数（FILESYSTEM/SECURITY 载荷，
			// 服务端不需要解析客户端发来的这些结构）。
			skipped++
			continue
		}
		seen[name]++
		checked++
		if !bytes.Equal(got, resp.Buffer) {
			t.Errorf("%s: %s 载荷重新编码与真实字节不一致\n%s",
				m, name, hexDiff(resp.Buffer, got))
		}
	}
	t.Logf("QUERY_INFO 载荷逐字节比对 %d 条（跳过 %d 条无解析函数的类），分布: %v",
		checked, skipped, seen)
	if checked == 0 {
		t.Fatal("一条 QUERY_INFO 载荷都没比对到")
	}
	// allinfo 一定会查这两个类，缺了说明索引逻辑坏了。
	for _, must := range []string{"FileAllInformation", "FileStreamInformation"} {
		if seen[must] == 0 {
			t.Errorf("没有比对到 %s，抓包关联可能出错", must)
		}
	}
}

// TestCaptureSetInfoPayload 比对 SET_INFO 请求里 Buffer 的结构还原。
//
// 覆盖来自 `smbclient rename/setmode/rm`：FileRenameInformation(10)、
// FileBasicInformation(4)、FileDispositionInformation(13)。
func TestCaptureSetInfoPayload(t *testing.T) {
	seen := map[string]int{}
	var checked, skipped int
	for _, m := range loadCaptureMsgs(t) {
		if m.hdr.Command != CommandSetInfo || m.hdr.IsResponse() {
			continue
		}
		req, err := ParseSetInfoRequest(m.data)
		if err != nil {
			t.Errorf("%s: ParseSetInfoRequest: %v", m, err)
			continue
		}
		name, got, err := reencodeInfoPayload(req.InfoType, req.FileInfoClass, req.Buffer)
		if err != nil {
			t.Errorf("%s: %v", m, err)
			continue
		}
		if name == "" {
			skipped++
			continue
		}
		seen[name]++
		checked++
		if !bytes.Equal(got, req.Buffer) {
			t.Errorf("%s: %s 载荷重新编码与真实字节不一致\n%s",
				m, name, hexDiff(req.Buffer, got))
		}
	}
	t.Logf("SET_INFO 载荷逐字节比对 %d 条（跳过 %d 条无解析函数的类），分布: %v",
		checked, skipped, seen)
	if checked == 0 {
		t.Fatal("一条 SET_INFO 载荷都没比对到")
	}
}

// reencodeInfoPayload 把一段信息类载荷解析后重新编码。
//
// 返回的 name 为空表示该类目前只有编码侧实现、没有解析函数，无法做往返比对
// （不是错误 —— 服务端只需要能编码 FsXxxInformation，不需要解析它们）。
func reencodeInfoPayload(infoType InfoType, class uint8, buf []byte) (string, []byte, error) {
	if len(buf) == 0 {
		return "", nil, nil
	}
	if infoType != InfoTypeFile {
		// SMB2_0_INFO_FILESYSTEM / SECURITY / QUOTA 的载荷本包只提供编码侧
		// （服务端不需要解析客户端发来的这些结构）。
		return "", nil, nil
	}

	switch FileInfoClass(class) {
	case FileBasicInformation:
		v, err := ParseFileBasicInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileBasicInfo: %w", err)
		}
		return "FileBasicInformation", v.Encode(), nil

	case FileStandardInformation:
		v, err := ParseFileStandardInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileStandardInfo: %w", err)
		}
		return "FileStandardInformation", v.Encode(), nil

	case FileAllInformation:
		v, err := ParseFileAllInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileAllInfo: %w", err)
		}
		return "FileAllInformation", v.Encode(), nil

	case FileStreamInformation:
		v, err := ParseStreamInfoChain(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseStreamInfoChain: %w", err)
		}
		return "FileStreamInformation", AppendStreamInfoChain(nil, v), nil

	case FileNetworkOpenInformation:
		v, err := ParseFileNetworkOpenInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileNetworkOpenInfo: %w", err)
		}
		return "FileNetworkOpenInformation", v.Encode(), nil

	case FileNameInformation, FileNormalizedNameInformation, FileAlternateNameInformation:
		v, err := ParseFileNameInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileNameInfo: %w", err)
		}
		return "FileNameInformation", EncodeFileNameInfo(v), nil

	case FilePositionInformation:
		v, err := ParseFilePositionInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFilePositionInfo: %w", err)
		}
		return "FilePositionInformation", EncodeFilePositionInfo(v), nil

	case FileRenameInformation:
		v, err := ParseFileRenameInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileRenameInfo: %w", err)
		}
		return "FileRenameInformation", v.Encode(), nil

	case FileLinkInformation:
		v, err := ParseFileLinkInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileLinkInfo: %w", err)
		}
		return "FileLinkInformation", v.Encode(), nil

	case FileDispositionInformation:
		v, err := ParseFileDispositionInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileDispositionInfo: %w", err)
		}
		return "FileDispositionInformation", EncodeFileDispositionInfo(v), nil

	case FileEndOfFileInformation:
		v, err := ParseFileEndOfFileInfo(buf)
		if err != nil {
			return "", nil, fmt.Errorf("ParseFileEndOfFileInfo: %w", err)
		}
		return "FileEndOfFileInformation", EncodeFileEndOfFileInfo(v), nil
	}
	return "", nil, nil
}

// TestCaptureQueryDirectoryPayload 比对 QUERY_DIRECTORY 响应里目录项链的还原。
//
// 这是对齐逻辑最容易出错的地方：条目之间 **8 字节对齐**、NextEntryOffset
// 相对本条起点、最后一条为 0。用真实 Samba 的链做往返，能同时验证
// DirEntryWriter 的对齐与 ParseDirEntries 的字段偏移。
func TestCaptureQueryDirectoryPayload(t *testing.T) {
	msgs := loadCaptureMsgs(t)

	// 响应体里没有 FileInformationClass，回查请求。
	reqs := map[infoKey]*QueryDirectoryRequest{}
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryDirectory || m.hdr.IsResponse() {
			continue
		}
		r, err := ParseQueryDirectoryRequest(m.data)
		if err != nil {
			t.Errorf("%s: ParseQueryDirectoryRequest: %v", m, err)
			continue
		}
		reqs[infoKey{m.frame.scenario, m.hdr.MessageID}] = r
	}
	if len(reqs) == 0 {
		t.Fatal("抓包里没有 QUERY_DIRECTORY 请求，fixture 不对")
	}

	var checked, entries int
	classes := map[FileInfoClass]int{}
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryDirectory || !m.hdr.IsResponse() || m.hdr.Status != 0 {
			continue
		}
		req, ok := reqs[infoKey{m.frame.scenario, m.hdr.MessageID}]
		if !ok {
			t.Errorf("%s: 找不到对应的请求", m)
			continue
		}
		resp, err := ParseQueryDirectoryResponse(m.data)
		if err != nil {
			t.Errorf("%s: ParseQueryDirectoryResponse: %v", m, err)
			continue
		}
		if len(resp.Buffer) == 0 {
			continue
		}
		class := req.FileInformationClass
		got, err := ParseDirEntries(resp.Buffer, class)
		if err != nil {
			t.Errorf("%s: ParseDirEntries(class=%d): %v", m, class, err)
			continue
		}

		// 用 DirEntryWriter 重新拼链：对齐与 NextEntryOffset 全部由它算。
		w := NewDirEntryWriter(class, len(resp.Buffer)+1024)
		for i, e := range got {
			ok, err := w.Add(e)
			if err != nil {
				t.Errorf("%s: 重新编码条目[%d]: %v", m, i, err)
				break
			}
			if !ok {
				t.Errorf("%s: 重新编码条目[%d] 空间不足", m, i)
				break
			}
		}
		checked++
		entries += len(got)
		classes[class]++
		if !bytes.Equal(w.Bytes(), resp.Buffer) {
			t.Errorf("%s: 目录项链重新编码与真实字节不一致（class=%d, %d 条）\n%s",
				m, class, len(got), hexDiff(resp.Buffer, w.Bytes()))
		}
	}
	t.Logf("QUERY_DIRECTORY 目录项链逐字节比对 %d 条响应、%d 个条目，class 分布: %v",
		checked, entries, classes)
	if checked == 0 {
		t.Fatal("一条 QUERY_DIRECTORY 目录项链都没比对到")
	}
}
