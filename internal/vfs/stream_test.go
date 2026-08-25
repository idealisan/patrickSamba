package vfs

// stream_test.go —— alternate data stream 的端到端行为测试。
//
// 这些用例走完整路径（Open → ReadAt/WriteAt → Close → 重新打开），
// 目的是验证「落盘格式」与「SMB 语义」两层真的接上了，
// 而不只是各自的单元测试通过。

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// finderInfoPattern 造一个可辨识的 32 字节 FinderInfo。
// 前 8 字节在真实数据里是「文件类型 + 创建者代码」，这里用两个
// 有意义的四字符码，方便在 hexdump 里一眼认出来。
func finderInfoPattern() []byte {
	fi := make([]byte, FinderInfoSize)
	copy(fi[0:4], "TEXT") // 文件类型
	copy(fi[4:8], "ttxt") // 创建者代码（SimpleText）
	for i := 8; i < FinderInfoSize; i++ {
		fi[i] = byte(i)
	}
	return fi
}

func openStreamH(t *testing.T, fs *LocalFS, path, stream string, flags OpenFlags, d Disposition) (Handle, Action) {
	t.Helper()
	h, act, err := fs.Open(&OpenRequest{
		Path: path, Stream: stream, Flags: flags, Disposition: d,
	})
	if err != nil {
		t.Fatalf("打开流 %s:%s: %v", path, stream, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, act
}

// requireXattr 确认这个 LocalFS 的扩展属性能力**可用**。
//
// 历史：本函数原本在宿主不支持 xattr 时 t.Skip。接上 oscap 之后那两条
// skip 已经不可达 —— 宿主没有扩展属性时由 builtin 适配器兜住，能力永远可用。
// 因此改成**硬断言**：真失败了就说明接线坏了，必须红。
// （AGENTS.md：失败用例不许 skip，一条会自己跳过的断言等于没有断言。）
func requireXattr(t *testing.T, fs *LocalFS) {
	t.Helper()
	p := filepath.Join(fs.Root(), ".xattrprobe")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(p) }()
	if err := fs.xattrAt(p, nil).Set("probe", []byte{1}); err != nil {
		t.Fatalf("扩展属性能力不可用（matrix=%s）: %v", fs.caps.Matrix(), err)
	}
}

// TestAfpInfoStreamRoundTrip 是 FinderInfo 的完整闭环：
// 写入 → 关闭 → 重新打开读出 → 落盘格式是 netatalk 兼容的。
func TestAfpInfoStreamRoundTrip(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "doc.txt", "hello")

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	blob := ai.Marshal()

	h, act := openStreamH(t, fs, "doc.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	if act != ActionCreated {
		t.Errorf("首次打开 AFP_AfpInfo 应是 ActionCreated，得到 %v", act)
	}
	if n, err := h.WriteAt(blob, 0); err != nil || n != AfpInfoSize {
		t.Fatalf("写 AfpInfo: n=%d err=%v", n, err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重新打开读回来
	h2, act2 := openStreamH(t, fs, "doc.txt", StreamAFPInfo, OpenRead, OpenExisting)
	if act2 != ActionOpened {
		t.Errorf("再次打开应是 ActionOpened，得到 %v", act2)
	}
	got := make([]byte, AfpInfoSize)
	if _, err := h2.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("读 AfpInfo: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Errorf("往返不一致:\n got % x\nwant % x", got, blob)
	}

	// 落盘的必须是 netatalk 兼容的 402 字节 AppleDouble blob，
	// 而不是我们自己发明的格式 —— 否则 Netatalk / 既有 Samba 共享读不出来。
	x := fs.xattrAt(filepath.Join(fs.Root(), "doc.txt"), nil)
	raw, err := x.Get(netatalkMetaXattr)
	if err != nil {
		t.Fatalf("落盘的 xattr %q 不存在: %v", netatalkMetaXattr, err)
	}
	if len(raw) != 402 {
		t.Errorf("落盘 blob = %d 字节, want 402 (AD_DATASZ_XATTR)", len(raw))
	}
	fi, err := parseMetaXattr(raw)
	if err != nil {
		t.Fatalf("落盘 blob 解析失败: %v", err)
	}
	if !bytes.Equal(fi[:], finderInfoPattern()) {
		t.Errorf("落盘的 FinderInfo 不对: % x", *fi)
	}
}

// TestAfpInfoStreamDeleteByZero 对应 Samba 的
// "delete AFP_AfpInfo by writing all 0"：客户端靠写全零 FinderInfo 删流。
func TestAfpInfoStreamDeleteByZero(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "doc.txt", "x")
	hostPath := filepath.Join(fs.Root(), "doc.txt")

	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	h, _ := openStreamH(t, fs, "doc.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	if _, err := h.WriteAt(ai.Marshal(), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	x := fs.xattrAt(hostPath, nil)
	if _, err := x.Get(netatalkMetaXattr); err != nil {
		t.Fatalf("前置条件不成立，xattr 应存在: %v", err)
	}

	// 写一个 FinderInfo 全零的 AfpInfo = 删除
	h2, _, err := fs.Open(&OpenRequest{
		Path: "doc.txt", Stream: StreamAFPInfo, Flags: OpenRead | OpenWrite, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h2.WriteAt(NewAfpInfo().Marshal(), 0); err != nil {
		t.Fatal(err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := x.Get(netatalkMetaXattr); !errors.Is(err, ErrNotFound) {
		t.Errorf("写全零 FinderInfo 后 xattr 应被删除，得到 %v", err)
	}
}

// TestAfpInfoStreamFixedLength：AFP_AfpInfo 是定长 60 字节，
// 越界写必须拒绝而不是悄悄扩容（扩容会写出对端解析不了的 blob）。
func TestAfpInfoStreamFixedLength(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "doc.txt", "x")

	h, _ := openStreamH(t, fs, "doc.txt", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)

	if _, err := h.WriteAt(make([]byte, 4), AfpInfoSize-2); !errors.Is(err, ErrInvalidArg) {
		t.Errorf("跨过 60 字节末尾的写应被拒绝，得到 %v", err)
	}
	if _, err := h.WriteAt(make([]byte, 1), AfpInfoSize); !errors.Is(err, ErrInvalidArg) {
		t.Errorf("从 60 开始写应被拒绝，得到 %v", err)
	}
	// 读到末尾应给 io.EOF
	if n, err := h.ReadAt(make([]byte, 8), AfpInfoSize-4); n != 4 || err != io.EOF {
		t.Errorf("末尾读 = (%d,%v), want (4, io.EOF)", n, err)
	}
	if a, err := h.Stat(); err != nil || a.Size != AfpInfoSize {
		t.Errorf("流长度应恒为 %d，得到 %v (%v)", AfpInfoSize, a.Size, err)
	}
}

// TestAfpInfoStreamRejectsGarbage：客户端写进来的必须是合法 AfpInfo，
// 否则拒绝并保留磁盘上的旧值（Samba 的 validate_afpinfo 行为）。
//
// 注意这里断言的是**写时**拒绝。本测试原先断言「写入被接受、校验推迟到
// 落盘」，那正是被 info agent 在真实链路上抓到的 bug：ErrBadAfpInfo 只能
// 从 Close 出去，而 SMB2 CLOSE 响应没有地方承载它，于是非法写入表现为
// 「一路成功但数据蒸发」。详见 afpinfo_write_test.go。
func TestAfpInfoStreamRejectsGarbage(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "doc.txt", "x")

	h, _, err := fs.Open(&OpenRequest{
		Path: "doc.txt", Stream: StreamAFPInfo, Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	})
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, AfpInfoSize)
	copy(junk, "NOPE")
	if _, err := h.WriteAt(junk, 0); !errors.Is(err, ErrBadAfpInfo) {
		t.Fatalf("整块写非法 AfpInfo 应当场回 ErrBadAfpInfo，得到 %v", err)
	}
	// 写被拒之后 Close 干净收尾：没有脏缓冲要落，也就没有错误可报。
	if err := h.Close(); err != nil {
		t.Errorf("写入已被拒，Close 应成功，得到 %v", err)
	}
}

// TestResourceForkRoundTrip 覆盖资源派生的完整闭环，
// 并确认落盘的是 macOS/Netatalk 认得的 ._ AppleDouble 文件。
func TestResourceForkRoundTrip(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "app.bin", "main data")

	payload := bytes.Repeat([]byte("RSRC"), 300) // 1200 字节

	h, act := openStreamH(t, fs, "app.bin", StreamAFPResource, OpenRead|OpenWrite, OpenAlways)
	if act != ActionCreated {
		t.Errorf("首次打开资源派生应是 ActionCreated，得到 %v", act)
	}
	if n, err := h.WriteAt(payload, 0); err != nil || n != len(payload) {
		t.Fatalf("写资源派生: n=%d err=%v", n, err)
	}
	if a, err := h.Stat(); err != nil || a.Size != int64(len(payload)) {
		t.Errorf("流长度 = %v (err=%v), want %d", a.Size, err, len(payload))
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// 落盘位置必须是 ._app.bin
	adPath := filepath.Join(fs.Root(), "._app.bin")
	raw, err := os.ReadFile(adPath)
	if err != nil {
		t.Fatalf("资源派生应落在 %s: %v", adPath, err)
	}
	if len(raw) != adRsrcOffData+len(payload) {
		t.Errorf("._ 文件 = %d 字节, want %d (82 头 + %d 数据)",
			len(raw), adRsrcOffData+len(payload), len(payload))
	}
	if !bytes.Equal(raw[adRsrcOffData:], payload) {
		t.Error("._ 文件里 82 字节之后应当就是资源数据")
	}
	// 头里的 RFORK 长度必须被更新，否则 macOS 读到旧长度
	_, n, err := parseRsrcHeader(raw)
	if err != nil {
		t.Fatalf("._ 头解析失败: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("._ 头里的资源长度 = %d, want %d", n, len(payload))
	}

	// 重新打开读回
	h2, _ := openStreamH(t, fs, "app.bin", StreamAFPResource, OpenRead, OpenExisting)
	got := make([]byte, len(payload))
	if _, err := h2.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("资源派生往返不一致")
	}

	// 主数据流不能被污染 —— 这是最要命的失败模式
	main, err := os.ReadFile(filepath.Join(fs.Root(), "app.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(main) != "main data" {
		t.Errorf("写资源派生污染了主数据流: %q", main)
	}
}

// TestResourceForkHiddenFromReadDir：._ 旁路文件不能作为独立条目出现在
// 目录枚举里，否则 Finder 显示重影、Time Machine band 计数翻倍。
func TestResourceForkHiddenFromReadDir(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "a.bin", "x")

	h, _ := openStreamH(t, fs, "a.bin", StreamAFPResource, OpenRead|OpenWrite, OpenAlways)
	if _, err := h.WriteAt([]byte("resource"), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// 前置条件：._ 文件确实存在于磁盘上
	if _, err := os.Lstat(filepath.Join(fs.Root(), "._a.bin")); err != nil {
		t.Fatalf("._a.bin 应存在: %v", err)
	}

	d, _, err := fs.Open(&OpenRequest{Path: "", Flags: OpenRead | OpenDirectory, Disposition: OpenExisting})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	entries, err := d.ReadDir("*", true, 0)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "._a.bin" {
			t.Error("._ 旁路文件不应出现在目录枚举里")
		}
	}
	// 但基础文件必须还在
	var found bool
	for _, e := range entries {
		if e.Name == "a.bin" {
			found = true
		}
	}
	if !found {
		t.Error("基础文件 a.bin 应出现在枚举里")
	}
}

// TestStreamsListing 覆盖 FileStreamInformation 的内容。
func TestStreamsListing(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)
	writeFile(t, fs, "f.bin", "12345")

	// 只有主数据流
	streams, err := fs.Streams("f.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0].Name != DefaultStreamName || streams[0].Size != 5 {
		t.Fatalf("初始应只有主数据流，得到 %+v", streams)
	}

	// 加上 FinderInfo
	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	h, _ := openStreamH(t, fs, "f.bin", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	if _, err := h.WriteAt(ai.Marshal(), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// 加上资源派生
	h2, _ := openStreamH(t, fs, "f.bin", StreamAFPResource, OpenRead|OpenWrite, OpenAlways)
	if _, err := h2.WriteAt(bytes.Repeat([]byte{7}, 99), 0); err != nil {
		t.Fatal(err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}

	streams, err = fs.Streams("f.bin")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]StreamInfo{}
	for _, s := range streams {
		byName[s.Name] = s
	}
	if s, ok := byName[DefaultStreamName]; !ok || s.Size != 5 {
		t.Errorf("主数据流缺失或大小不对: %+v", byName)
	}
	if s, ok := byName[StreamName(StreamAFPInfo)]; !ok || s.Size != AfpInfoSize {
		t.Errorf("AFP_AfpInfo 流缺失或大小不对: %+v", byName)
	}
	if s, ok := byName[StreamName(StreamAFPResource)]; !ok || s.Size != 99 {
		t.Errorf("AFP_Resource 流缺失或大小不对: %+v", byName)
	}
}

// TestStreamPathSyntax 确认 "file:stream:$DATA" 这种路径写法能直接用，
// 不必让 server 层去拆 —— macOS 客户端常这么发。
func TestStreamPathSyntax(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f.bin", "data")

	h, _, err := fs.Open(&OpenRequest{
		Path: "f.bin:AFP_Resource:$DATA", Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
	})
	if err != nil {
		t.Fatalf("路径内嵌流名应被接受: %v", err)
	}
	if _, err := h.WriteAt([]byte("rsrc"), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(fs.Root(), "._f.bin")); err != nil {
		t.Errorf("应落到 ._f.bin: %v", err)
	}

	// "file::$DATA" 是主数据流的显式写法，不能被当成 ADS
	h2, _, err := fs.Open(&OpenRequest{
		Path: "f.bin::$DATA", Flags: OpenRead, Disposition: OpenExisting,
	})
	if err != nil {
		t.Fatalf("主数据流的显式写法应被接受: %v", err)
	}
	defer func() { _ = h2.Close() }()
	got := make([]byte, 4)
	if _, err := h2.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Errorf("f.bin::$DATA 应读到主数据流，得到 %q", got)
	}
}

// TestStreamReadOnlyShare：只读共享上不能写任何流。
func TestStreamReadOnlyShare(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f.bin", "x")

	roFS, err := NewLocalFS(LocalConfig{Root: fs.Root(), ReadOnly: true, CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = roFS.Close() }()

	for _, stream := range []string{StreamAFPInfo, StreamAFPResource} {
		if _, _, err := roFS.Open(&OpenRequest{
			Path: "f.bin", Stream: stream, Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
		}); !errors.Is(err, ErrReadOnly) {
			t.Errorf("只读共享上创建 %s 应返回 ErrReadOnly，得到 %v", stream, err)
		}
	}
}

// TestStreamOnMissingBase：基础对象不存在时不能凭空造出流。
// TestStreamOpenOnMissingBaseStaysNotFound：**非创建语义**的打开
// （FILE_OPEN / FILE_OVERWRITE）在基础对象不存在时必须失败，
// 且不能顺手把基础文件造出来。
func TestStreamOpenOnMissingBaseStaysNotFound(t *testing.T) {
	fs := newTestFS(t, false)
	for _, disp := range []Disposition{OpenExisting, TruncateExisting} {
		for _, stream := range []string{StreamAFPInfo, StreamAFPResource, "Meta"} {
			if _, _, err := fs.Open(&OpenRequest{
				Path: "ghost.bin", Stream: stream, Flags: OpenRead | OpenWrite, Disposition: disp,
			}); !errors.Is(err, ErrNotFound) {
				t.Errorf("disposition=%d 基础对象不存在时打开 %s 应返回 ErrNotFound，得到 %v",
					disp, stream, err)
			}
		}
	}
	// 失败路径不得留下半成品基础文件。
	if _, err := os.Lstat(filepath.Join(fs.Root(), "ghost.bin")); !os.IsNotExist(err) {
		t.Errorf("失败的流打开不该创建基础文件，Lstat err=%v", err)
	}
}

// TestStreamCreateBuildsMissingBase：带创建语义的 disposition 在基础对象
// 不存在时要先把基础文件建出来再开流（bh5 F2）。
//
// Samba 对照：对流路径先以 FILE_OPEN_IF 打开基础文件
// （source3/smbd/open.c:6508 附近），注释原话
// "We may be creating the basefile as part of creating the stream"。
// 差异（有意）：Samba 对 FILE_OVERWRITE 也用 OPEN_IF 建基础文件，
// 我们对 OVERWRITE 保持 NOT_FOUND 且不建 —— 否则一次注定失败的打开
// 会留下一个空的残留文件。
func TestStreamCreateBuildsMissingBase(t *testing.T) {
	cases := []struct {
		name string
		disp Disposition
		want Action
	}{
		{"OpenIf", OpenAlways, ActionCreated},
		{"Create", CreateNew, ActionCreated},
		{"Supersede", Supersede, ActionCreated},
		{"OverwriteIf", TruncateAlways, ActionCreated},
	}
	for _, stream := range []string{"Meta", StreamAFPInfo} {
		for _, tc := range cases {
			t.Run(stream+"/"+tc.name, func(t *testing.T) {
				fs := newTestFS(t, false)
				h, action, err := fs.Open(&OpenRequest{
					Path: "new.bin", Stream: stream,
					Flags: OpenRead | OpenWrite, Disposition: tc.disp,
				})
				if err != nil {
					t.Fatalf("创建性打开 %s:%s: %v", "new.bin", stream, err)
				}
				if action != tc.want {
					t.Errorf("action = %d, 期望 %d", action, tc.want)
				}
				// AFP_AfpInfo 是定长且带签名校验的：必须写合法的 60 字节
				// blob 才算「真实写入」（全零 FinderInfo 等于删除语义，
				// 流不会进清单）。通用流写任意内容即可。
				if stream == StreamAFPInfo {
					ai := NewAfpInfo()
					copy(ai.FinderInfo[:], finderInfoPattern())
					if _, serr := h.WriteAt(ai.Marshal(), 0); serr != nil {
						t.Fatalf("写入流: %v", serr)
					}
				} else {
					if _, serr := h.WriteAt([]byte("x"), 0); serr != nil {
						t.Fatalf("写入流: %v", serr)
					}
				}
				if cerr := h.Close(); cerr != nil {
					t.Fatalf("Close: %v", cerr)
				}

				// 基础文件必须真的被建出来了。
				fi, lerr := os.Lstat(filepath.Join(fs.Root(), "new.bin"))
				if lerr != nil {
					t.Fatalf("基础文件未被创建: %v", lerr)
				}
				if fi.IsDir() {
					t.Fatal("基础对象应是普通文件")
				}
				// 流也要立即可见。
				list, serr := fs.Streams("new.bin")
				if serr != nil {
					t.Fatalf("Streams: %v", serr)
				}
				want := ":" + stream + ":$DATA"
				found := false
				for _, si := range list {
					if si.Name == want {
						found = true
					}
				}
				if !found {
					t.Errorf("流清单缺 %s：%v", want, list)
				}
			})
		}
	}
}

// TestStreamPathTraversal：流名不能成为路径穿越的新入口。
func TestStreamPathTraversal(t *testing.T) {
	fs := newTestFS(t, false)
	writeFile(t, fs, "f.bin", "x")

	for _, s := range []string{
		"../evil", `..\evil`, "/etc/passwd", "..", ".", "a/b", "a:b",
	} {
		if _, _, err := fs.Open(&OpenRequest{
			Path: "f.bin", Stream: s, Flags: OpenRead | OpenWrite, Disposition: OpenAlways,
		}); err == nil {
			t.Errorf("流名 %q 竟然被接受", s)
		}
	}
}

// TestAppleInfoCapability 覆盖 AAPL readdir_attr 的数据来源。
//
// wire 层拼 readdir_attr 时每条目录项都要这两样东西，
// 这里确认「有」和「没有」两种情况都给出正确结果。
func TestAppleInfoCapability(t *testing.T) {
	fs := newTestFS(t, false)
	requireXattr(t, fs)

	am, ok := interface{}(fs).(AppleMetadata)
	if !ok {
		t.Fatal("LocalFS 应实现 AppleMetadata")
	}

	// 没有任何 Apple 元数据的普通文件：全零 + 0，且**不报错**
	writeFile(t, fs, "plain.txt", "hello")
	fi, rsrc, err := am.AppleInfo("plain.txt")
	if err != nil {
		t.Fatalf("没有 Apple 元数据不该报错: %v", err)
	}
	if rsrc != 0 {
		t.Errorf("资源派生大小 = %d, want 0", rsrc)
	}
	if fi != ([FinderInfoSize]byte{}) {
		t.Errorf("FinderInfo 应为全零，得到 % x", fi)
	}

	// 写入 FinderInfo 与资源派生之后
	writeFile(t, fs, "rich.bin", "data")
	ai := NewAfpInfo()
	copy(ai.FinderInfo[:], finderInfoPattern())
	h, _ := openStreamH(t, fs, "rich.bin", StreamAFPInfo, OpenRead|OpenWrite, OpenAlways)
	if _, err := h.WriteAt(ai.Marshal(), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h2, _ := openStreamH(t, fs, "rich.bin", StreamAFPResource, OpenRead|OpenWrite, OpenAlways)
	if _, err := h2.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}

	fi, rsrc, err = am.AppleInfo("rich.bin")
	if err != nil {
		t.Fatal(err)
	}
	if rsrc != 4096 {
		t.Errorf("资源派生大小 = %d, want 4096", rsrc)
	}
	if !bytes.Equal(fi[:], finderInfoPattern()) {
		t.Errorf("FinderInfo 不对: % x", fi)
	}
	// wire 层要用的是前 8 字节（类型码 + 创建者码），确认它们在位
	if string(fi[0:4]) != "TEXT" || string(fi[4:8]) != "ttxt" {
		t.Errorf("类型/创建者码错位: %q %q", fi[0:4], fi[4:8])
	}

	// 不存在的对象要报 ErrNotFound，不能返回全零假装成功
	if _, _, err := am.AppleInfo("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的对象应返回 ErrNotFound，得到 %v", err)
	}
	// 穿越向量同样要被拦住
	if _, _, err := am.AppleInfo("../secret"); err == nil {
		t.Error("穿越向量应被拒绝")
	}
}
