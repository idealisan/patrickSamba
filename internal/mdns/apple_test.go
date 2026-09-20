package mdns

import (
	"reflect"
	"strings"
	"testing"

	"github.com/idealisan/patrickSamba/internal/config"
)

// 本文件验证 Apple 生态三组服务的 TXT/SRV 构造。
//
// 期望值不是编造的，来自 apple.go 文件头记录的 avahi/Samba Time Machine 配方
// （社区里被反复验证、macOS Ventura 实测可用的那一份）：
//
//	_smb._tcp          port 445
//	_device-info._tcp  port 0, TXT model=TimeCapsule8,119
//	_adisk._tcp        port 0, TXT dk0=adVN=<share>,adVF=0x82
//	                            TXT sys=waMa=0,adVF=0x100
//
// 这几个字符串是 macOS「时间机器」能否发现本服务的**唯一**依据，
// 写错一个字符就是整条 Time Machine 链路不通，所以逐字节比对。

func TestSMBServiceDef(t *testing.T) {
	d := smbServiceDef(445)

	if d.Type != "_smb._tcp" {
		t.Errorf("Type = %q, 期望 %q", d.Type, "_smb._tcp")
	}
	if d.Port != 445 {
		t.Errorf("Port = %d, 期望 445", d.Port)
	}
	// SMB 不需要 TXT 内容（RFC 6763 §6.1 的空 character-string 由编码层补）。
	if len(d.TXT) != 0 {
		t.Errorf("TXT = %v, 期望空", d.TXT)
	}
	if err := d.validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
	if got, want := d.typeName(), "_smb._tcp.local."; got != want {
		t.Errorf("typeName = %q, 期望 %q", got, want)
	}
}

func TestDeviceInfoServiceDef(t *testing.T) {
	d := deviceInfoServiceDef("TimeCapsule8,119")

	if d.Type != "_device-info._tcp" {
		t.Errorf("Type = %q, 期望 %q", d.Type, "_device-info._tcp")
	}
	// 端口 0：这个服务没有真实端口，信息全在 TXT 里。
	if d.Port != 0 {
		t.Errorf("Port = %d, 期望 0", d.Port)
	}
	want := []string{"model=TimeCapsule8,119"}
	if !reflect.DeepEqual(d.TXT, want) {
		t.Errorf("TXT = %q, 期望 %q", d.TXT, want)
	}
	if err := d.validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestADiskServiceDef(t *testing.T) {
	tests := []struct {
		name    string
		volumes []string
		wantOK  bool
		wantTXT []string
	}{
		{
			name:    "无卷则不宣告",
			volumes: nil,
			wantOK:  false,
		},
		{
			name:    "单卷",
			volumes: []string{"backup"},
			wantOK:  true,
			wantTXT: []string{
				"dk0=adVN=backup,adVF=0x82",
				"sys=waMa=0,adVF=0x100",
			},
		},
		{
			// 多个 Time Machine 共享按 dk0/dk1/dk2 顺序编号（RFC 6763 §6.3）。
			name:    "多卷按 dkN 编号",
			volumes: []string{"tm1", "tm2", "tm3"},
			wantOK:  true,
			wantTXT: []string{
				"dk0=adVN=tm1,adVF=0x82",
				"dk1=adVN=tm2,adVF=0x82",
				"dk2=adVN=tm3,adVF=0x82",
				"sys=waMa=0,adVF=0x100",
			},
		},

		{
			// 卷名里的分隔符必须转义：不转义的话 `a,b` 会被当成
			// `adVN=a` + 一个畸形字段，条目直接废掉（或更糟，被解析成
			// 一个不存在的卷名而静默丢掉这个备份卷）。
			//
			// config 层已经禁掉了这些字符，这里防的是「没走 config 校验」
			// 的调用方（例如编程式构造的配置）。
			name:    "卷名含分隔符要转义",
			volumes: []string{`a,b`, `c\d`},
			wantOK:  true,
			wantTXT: []string{
				`dk0=adVN=a\,b,adVF=0x82`,
				`dk1=adVN=c\\d,adVF=0x82`,
				"sys=waMa=0,adVF=0x100",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := adiskServiceDef(tc.volumes)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, 期望 %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if d.Type != "_adisk._tcp" {
				t.Errorf("Type = %q, 期望 %q", d.Type, "_adisk._tcp")
			}
			if d.Port != 0 {
				t.Errorf("Port = %d, 期望 0", d.Port)
			}
			if !reflect.DeepEqual(d.TXT, tc.wantTXT) {
				t.Errorf("TXT =\n  %q\n期望\n  %q", d.TXT, tc.wantTXT)
			}
			if err := d.validate(); err != nil {
				t.Errorf("validate: %v", err)
			}
		})
	}
}

// TestNewServiceComposition 验证配置开关如何组合出服务集合。
func TestNewServiceComposition(t *testing.T) {
	tmShares := []config.Share{
		{Name: "public"},
		{Name: "backup", TimeMachine: true},
	}

	tests := []struct {
		name   string
		apple  config.AppleMDNS
		shares []config.Share
		want   []string
	}{
		{
			// 零值（未设置）= 默认开启：DefaultAppleMDNS 的规则在 EnabledOn 里，
			// 不依赖 ApplyDefaults 先跑一遍。
			name:   "未设置默认开启 Apple 扩展",
			apple:  config.AppleMDNS{},
			shares: tmShares,
			want:   []string{serviceTypeSMB, serviceTypeDeviceInfo},
		},
		{
			name:   "显式关闭 Apple 扩展只广播 SMB",
			apple:  config.AppleMDNS{Enabled: boolPtr(false)},
			shares: tmShares,
			want:   []string{serviceTypeSMB},
		},
		{
			name:   "开 Apple 但不宣告 TM",
			apple:  config.AppleMDNS{Enabled: boolPtr(true), Model: "MacSamba"},
			shares: tmShares,
			want:   []string{serviceTypeSMB, serviceTypeDeviceInfo},
		},
		{
			name:   "开 TM 宣告且有 TM 共享",
			apple:  config.AppleMDNS{Enabled: boolPtr(true), AdvertiseTimeMachine: true},
			shares: tmShares,
			want:   []string{serviceTypeSMB, serviceTypeDeviceInfo, serviceTypeADisk},
		},
		{
			// 没有任何 time_machine: true 的共享时不能宣告 _adisk：
			// 宣告一个空的备份磁盘列表会让 Finder 显示一个连不上的条目。
			name:   "开 TM 宣告但没有 TM 共享",
			apple:  config.AppleMDNS{Enabled: boolPtr(true), AdvertiseTimeMachine: true},
			shares: []config.Share{{Name: "public"}},
			want:   []string{serviceTypeSMB, serviceTypeDeviceInfo},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := New(config.MDNS{
				Enabled:  config.BoolPtr(true),
				Instance: "TESTBOX",
				Apple:    tc.apple,
			}, 445, tc.shares)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got := make([]string, 0, len(r.rs.defs))
			for _, d := range r.rs.defs {
				got = append(got, d.Type)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("服务集合 = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// TestNewAppleModelDefault 验证 model= 留空时回落到 config 的默认值。
func TestNewAppleModelDefault(t *testing.T) {
	r, err := New(config.MDNS{
		Enabled:  config.BoolPtr(true),
		Instance: "TESTBOX",
		Apple:    config.AppleMDNS{Enabled: boolPtr(true)},
	}, 445, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := "model=" + config.DefaultAppleModel
	for _, d := range r.rs.defs {
		if d.Type != serviceTypeDeviceInfo {
			continue
		}
		if len(d.TXT) != 1 || d.TXT[0] != want {
			t.Fatalf("_device-info TXT = %q, 期望 [%q]", d.TXT, want)
		}
		return
	}
	t.Fatal("没有生成 _device-info._tcp 服务")
}

// TestAppleRecords 验证生成的 DNS 记录：实例名、SRV 端口、TXT 内容。
//
// 这一层才是真正发到线上的东西 —— serviceDef 对了但记录拼错同样连不上。
func TestAppleRecords(t *testing.T) {
	const instance = "TESTBOX"

	r, err := New(config.MDNS{
		Enabled:  config.BoolPtr(true),
		Instance: instance,
		Apple: config.AppleMDNS{
			Enabled:              boolPtr(true),
			Model:                "TimeCapsule8,119",
			AdvertiseTimeMachine: true,
		},
	}, 445, []config.Share{{Name: "backup", TimeMachine: true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	txt := map[string][]string{}
	srv := map[string]uint16{}
	ptr := map[string]bool{}
	for _, rec := range r.rs.serviceRecords() {
		switch d := rec.Data.(type) {
		case TXT:
			txt[strings.ToLower(rec.Name)] = d.Strings
		case SRV:
			srv[strings.ToLower(rec.Name)] = d.Port
		case PTR:
			ptr[strings.ToLower(rec.Name+"|"+d.Target)] = true
		}
	}

	adiskInstance := strings.ToLower(instance + "._adisk._tcp.local.")
	wantADisk := []string{
		"dk0=adVN=backup,adVF=0x82",
		"sys=waMa=0,adVF=0x100",
	}
	if !reflect.DeepEqual(txt[adiskInstance], wantADisk) {
		t.Errorf("_adisk TXT = %q, 期望 %q", txt[adiskInstance], wantADisk)
	}

	devInstance := strings.ToLower(instance + "._device-info._tcp.local.")
	wantDev := []string{"model=TimeCapsule8,119"}
	if !reflect.DeepEqual(txt[devInstance], wantDev) {
		t.Errorf("_device-info TXT = %q, 期望 %q", txt[devInstance], wantDev)
	}

	if got := srv[strings.ToLower(instance+"._smb._tcp.local.")]; got != 445 {
		t.Errorf("_smb SRV 端口 = %d, 期望 445", got)
	}
	// _adisk / _device-info 是「无端口」服务，SRV 里必须是 0。
	if got, ok := srv[adiskInstance]; !ok || got != 0 {
		t.Errorf("_adisk SRV 端口 = %d (存在=%v), 期望 0", got, ok)
	}
	if got, ok := srv[devInstance]; !ok || got != 0 {
		t.Errorf("_device-info SRV 端口 = %d (存在=%v), 期望 0", got, ok)
	}

	// 每个服务类型都要有 类型 PTR→实例，以及元查询 PTR→类型（RFC 6763 §4.1/§9）。
	for _, typ := range []string{serviceTypeSMB, serviceTypeDeviceInfo, serviceTypeADisk} {
		typeName := typ + "." + localDomain
		if !ptr[strings.ToLower(typeName+"|"+instance+"."+typeName)] {
			t.Errorf("缺少 %s 的服务实例 PTR", typ)
		}
		if !ptr[strings.ToLower(metaQueryName+"|"+typeName)] {
			t.Errorf("缺少 %s 的元查询 PTR", typ)
		}
	}
}

// TestAppleRecordsPackable 验证这些 TXT 能被真的编码进 DNS 报文并原样解回来。
//
// TXT 里的 "=" 与 "," 是 DNS-SD 的子分隔符，历史上很容易在编码环节被吃掉。
func TestAppleRecordsPackable(t *testing.T) {
	def, ok := adiskServiceDef([]string{"backup", "archive"})
	if !ok {
		t.Fatal("adiskServiceDef 返回 ok=false")
	}

	msg := &Message{
		Flags: FlagResponse | FlagAuthoritative,
		Answers: []Record{{
			Name: "TESTBOX." + def.typeName(), Class: ClassIN, TTL: ttlShared,
			Data: TXT{Strings: def.TXT},
		}},
	}
	raw, err := msg.Pack()
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	got, err := Unpack(raw)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if len(got.Answers) != 1 {
		t.Fatalf("解出 %d 条记录, 期望 1", len(got.Answers))
	}
	rec, ok := got.Answers[0].Data.(TXT)
	if !ok {
		t.Fatalf("记录类型 = %T, 期望 TXT", got.Answers[0].Data)
	}
	if !reflect.DeepEqual(rec.Strings, def.TXT) {
		t.Errorf("往返后 TXT = %q, 期望 %q", rec.Strings, def.TXT)
	}
}

// boolPtr 是 *bool 配置项测试用的取址助手。
func boolPtr(b bool) *bool { return &b }
