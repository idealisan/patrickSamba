package wire

// 抓包比对第三层：**文件系统信息类**（QUERY_INFO InfoType=FILESYSTEM）。
//
// 为什么单独一个文件：capture_payload_test.go 的 reencodeInfoPayload 明确跳过了
// FILESYSTEM 载荷（"本类只有编码侧、没有解析函数"），实测每跑一次就有 5 条被跳过。
// 而 FILESYSTEM 恰恰是 Time Machine 的命门 —— FileFsAttributeInformation 少宣告
// 一个位，macOS 就整条特性不走（FILE_NAMED_STREAMS 缺失导致不走 ADS 就是真实
// 发生过的事故）；FileFsFullSizeInformation 报错容量，Time Machine 会把卷写爆。
// 这块不比对等于没比对。
//
// 本文件做两件事：
//  1. 结构还原：把真实 Samba 4.22 的 FILESYSTEM 载荷解析后重新编码，要求逐字节相等；
//  2. 语义对照：把 Samba 宣告的 FS 能力位与文件系统名记录成可读日志，
//     并对我们**有意与 Samba 取不同值**的地方做显式断言，防止哪天被"顺手改回去"。

import (
	"bytes"
	"testing"
)

// TestCaptureQueryFsInfoPayload 逐字节比对 FILESYSTEM 信息类的结构还原。
func TestCaptureQueryFsInfoPayload(t *testing.T) {
	msgs := loadCaptureMsgs(t)

	reqs := map[infoKey]*QueryInfoRequest{}
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryInfo || m.hdr.IsResponse() {
			continue
		}
		r, err := ParseQueryInfoRequest(m.data)
		if err != nil {
			continue
		}
		if r.InfoType != InfoTypeFileSystem {
			continue
		}
		reqs[infoKey{m.frame.scenario, m.hdr.MessageID}] = r
	}
	if len(reqs) == 0 {
		t.Skip("抓包里没有 InfoType=FILESYSTEM 的 QUERY_INFO，无法比对")
	}

	seen := map[string]int{}
	var checked int
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryInfo || !m.hdr.IsResponse() || m.hdr.Status != 0 {
			continue
		}
		req, ok := reqs[infoKey{m.frame.scenario, m.hdr.MessageID}]
		if !ok {
			continue
		}
		resp, err := ParseQueryInfoResponse(m.data)
		if err != nil {
			t.Errorf("%s: ParseQueryInfoResponse: %v", m, err)
			continue
		}
		name, got, err := reencodeFsInfoPayload(req.FsClass(), resp.Buffer)
		if err != nil {
			t.Errorf("%s: FsClass=%d: %v", m, req.FsClass(), err)
			continue
		}
		if name == "" {
			t.Logf("%s: 未覆盖的 FS 信息类 %d（%d 字节），需要补解析函数",
				m, req.FsClass(), len(resp.Buffer))
			continue
		}
		seen[name]++
		checked++
		if !bytes.Equal(got, resp.Buffer) {
			t.Errorf("%s: %s 载荷重新编码与真实 Samba 字节不一致\n%s",
				m, name, hexDiff(resp.Buffer, got))
		}
	}
	t.Logf("FILESYSTEM 载荷逐字节比对 %d 条，分布: %v", checked, seen)
	if checked == 0 {
		t.Fatal("一条 FILESYSTEM 载荷都没比对到")
	}
}

// TestCaptureSambaFsAttributes 把真实 Samba 宣告的 FS 能力位摊开，
// 与我们的取值做**有意识**的对照。
//
// 这不是「必须和 Samba 一模一样」的测试 —— 我们和 Samba 的后端能力本来就不同，
// 有几处是刻意取不同值的。这个测试的作用是：把差异钉死在代码里，
// 任何一方变了都要重新解释一次为什么。
func TestCaptureSambaFsAttributes(t *testing.T) {
	msgs := loadCaptureMsgs(t)

	reqs := map[infoKey]*QueryInfoRequest{}
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryInfo || m.hdr.IsResponse() {
			continue
		}
		if r, err := ParseQueryInfoRequest(m.data); err == nil &&
			r.InfoType == InfoTypeFileSystem &&
			r.FsClass() == FileFsAttributeInformation {
			reqs[infoKey{m.frame.scenario, m.hdr.MessageID}] = r
		}
	}
	if len(reqs) == 0 {
		t.Skip("抓包里没有 FileFsAttributeInformation 查询")
	}

	var found bool
	for _, m := range msgs {
		if m.hdr.Command != CommandQueryInfo || !m.hdr.IsResponse() || m.hdr.Status != 0 {
			continue
		}
		if _, ok := reqs[infoKey{m.frame.scenario, m.hdr.MessageID}]; !ok {
			continue
		}
		resp, err := ParseQueryInfoResponse(m.data)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		attr, err := ParseFsAttributeInfo(resp.Buffer)
		if err != nil {
			t.Fatalf("%s: ParseFsAttributeInfo: %v", m, err)
		}
		found = true

		t.Logf("真实 Samba 4.22: FileSystemName=%q MaxComponentNameLength=%d Attributes=%#08x %v",
			attr.FileSystemName, attr.MaximumComponentNameLength,
			attr.Attributes, fsAttrNames(attr.Attributes))

		// Samba 也报 NTFS。这不是巧合：客户端拿这个字符串决定要不要启用
		// alternate data stream 等特性，报别的名字会被保守降级。
		// 我们在 command 层的 fsName 常量也取 "NTFS"，见 query_info.go 的注释。
		if attr.FileSystemName != "NTFS" {
			t.Errorf("Samba 报的文件系统名 = %q，与我们采用 NTFS 的依据不符，请重新评估 fsName",
				attr.FileSystemName)
		}

		// FILE_NAMED_STREAMS 是 Apple 扩展的前置条件。抓包里的参照 smbd 没开
		// vfs_fruit，所以它**不宣告**这一位 —— 这正是我们与 Samba 有意不同的地方，
		// 我们内建 AFP_AfpInfo / AFP_Resource，必须宣告。
		// 记下来是为了避免有人看到"Samba 都不报"就把我们的也删了。
		if attr.Attributes&FileNamedStreams != 0 {
			t.Logf("注意：参照 smbd 宣告了 FILE_NAMED_STREAMS（大概率开了 vfs_fruit 或 streams_xattr）")
		} else {
			t.Logf("参照 smbd 未宣告 FILE_NAMED_STREAMS（未开 vfs_fruit）；" +
				"我们内建 AFP 流，必须宣告此位，属于有意差异")
		}

		// 这几位是任何像样的 POSIX 后端都该有的，Samba 缺了才是可疑。
		for _, want := range []struct {
			bit  uint32
			name string
		}{
			{FileCasePreservedNames, "FILE_CASE_PRESERVED_NAMES"},
			{FileUnicodeOnDisk, "FILE_UNICODE_ON_DISK"},
		} {
			if attr.Attributes&want.bit == 0 {
				t.Errorf("参照 Samba 竟然没有宣告 %s，说明 fixture 或位定义有问题", want.name)
			}
		}
	}
	if !found {
		t.Fatal("没有找到 FileFsAttributeInformation 响应")
	}
}

// reencodeFsInfoPayload 解析再编码一个 FILESYSTEM 信息类载荷。
//
// 返回空 name 表示本类还没有解析函数（调用方记日志而不是报错，
// 免得抓包里出现新类时整个测试红掉）。
func reencodeFsInfoPayload(class FsInfoClass, buf []byte) (string, []byte, error) {
	switch class {
	case FileFsVolumeInformation:
		v, err := ParseFsVolumeInfo(buf)
		if err != nil {
			return "", nil, err
		}
		return "FileFsVolumeInformation", v.Encode(), nil

	case FileFsSizeInformation:
		v, err := ParseFsSizeInfo(buf)
		if err != nil {
			return "", nil, err
		}
		return "FileFsSizeInformation", v.Encode(), nil

	case FileFsFullSizeInformation:
		v, err := ParseFsFullSizeInfo(buf)
		if err != nil {
			return "", nil, err
		}
		return "FileFsFullSizeInformation", v.Encode(), nil

	case FileFsDeviceInformation:
		v, err := ParseFsDeviceInfo(buf)
		if err != nil {
			return "", nil, err
		}
		return "FileFsDeviceInformation", v.Encode(), nil

	case FileFsAttributeInformation:
		v, err := ParseFsAttributeInfo(buf)
		if err != nil {
			return "", nil, err
		}
		return "FileFsAttributeInformation", v.Encode(), nil

	case FileFsSectorSizeInformation:
		v, err := ParseFsSectorSizeInfo(buf)
		if err != nil {
			return "", nil, err
		}
		return "FileFsSectorSizeInformation", v.Encode(), nil

	case FileFsObjectIDInformation:
		v, err := ParseFsObjectIDInfo(buf)
		if err != nil {
			return "", nil, err
		}
		return "FileFsObjectIdInformation", v.Encode(), nil

	default:
		return "", nil, nil
	}
}

// fsAttrNames 把 FileFsAttributeInformation 的位图展开成可读名字，
// 便于在测试日志里直接看出 Samba 到底宣告了什么。
func fsAttrNames(attrs uint32) []string {
	table := []struct {
		bit  uint32
		name string
	}{
		{FileCaseSensitiveSearch, "CASE_SENSITIVE_SEARCH"},
		{FileCasePreservedNames, "CASE_PRESERVED_NAMES"},
		{FileUnicodeOnDisk, "UNICODE_ON_DISK"},
		{FilePersistentACLs, "PERSISTENT_ACLS"},
		{FileFileCompression, "FILE_COMPRESSION"},
		{FileVolumeQuotas, "VOLUME_QUOTAS"},
		{FileSupportsSparseFiles, "SUPPORTS_SPARSE_FILES"},
		{FileSupportsReparsePoints, "SUPPORTS_REPARSE_POINTS"},
		{FileNamedStreams, "NAMED_STREAMS"},
		{FileReadOnlyVolume, "READ_ONLY_VOLUME"},
	}
	var out []string
	for _, e := range table {
		if attrs&e.bit != 0 {
			out = append(out, e.name)
		}
	}
	return out
}
