package dialect

import "testing"

func TestNegotiatePicksHighestCommon(t *testing.T) {
	tests := []struct {
		name     string
		client   []Dialect
		min, max Dialect
		want     Dialect
		wantOK   bool
	}{
		{
			// Win10 客户端典型方言列表。
			name:   "win10 全量",
			client: []Dialect{SMB202, SMB210, SMB300, SMB302, SMB311},
			want:   SMB311, wantOK: true,
		},
		{
			// smbclient -m SMB2 只发 2.0.2 / 2.1。
			name:   "仅 SMB2",
			client: []Dialect{SMB202, SMB210},
			want:   SMB210, wantOK: true,
		},
		{
			name:   "服务端上限压到 3.0.2",
			client: []Dialect{SMB202, SMB210, SMB300, SMB302, SMB311},
			max:    SMB302,
			want:   SMB302, wantOK: true,
		},
		{
			name:   "服务端下限抬到 3.0",
			client: []Dialect{SMB202, SMB210},
			min:    SMB300,
			wantOK: false,
		},
		{
			name:   "通配方言被忽略",
			client: []Dialect{SMB2Wildcard},
			wantOK: false,
		},
		{
			name:   "未知方言被忽略",
			client: []Dialect{0x0400, 0x0311},
			want:   SMB311, wantOK: true,
		},
		{
			name:   "乱序也取最高",
			client: []Dialect{SMB311, SMB202, SMB300},
			want:   SMB311, wantOK: true,
		},
		{
			name:   "空列表",
			client: nil,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Negotiate(tt.client, tt.min, tt.max)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("dialect = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	// 2.0.2 不支持 LARGE_MTU。
	if c := SMB202.ServerCapabilities(true, true); c&CapLargeMTU != 0 {
		t.Fatal("2.0.2 不应宣告 CAP_LARGE_MTU")
	}
	// 2.1 起必须宣告 LARGE_MTU。
	if c := SMB210.ServerCapabilities(false, false); c&CapLargeMTU == 0 {
		t.Fatal("2.1 必须宣告 CAP_LARGE_MTU")
	}
	// 任何方言都不得宣告 CAP_DFS（我们不实现 DFS）。
	for _, d := range All {
		if c := d.ServerCapabilities(true, true); c&CapDFS != 0 {
			t.Fatalf("%v 不应宣告 CAP_DFS", d)
		}
	}
	// 3.0 开加密时宣告 CAP_ENCRYPTION；3.1.1 改用 negotiate context，不置该位。
	if c := SMB300.ServerCapabilities(true, false); c&CapEncryption == 0 {
		t.Fatal("3.0 开启加密时应宣告 CAP_ENCRYPTION")
	}
	if c := SMB311.ServerCapabilities(true, false); c&CapEncryption != 0 {
		t.Fatal("3.1.1 不应通过 Capabilities 位宣告加密")
	}
}

func TestMaxTransactSize(t *testing.T) {
	if got := SMB202.MaxTransactSize(); got != MaxTransactSizeSmall {
		t.Fatalf("2.0.2 MaxTransactSize = 0x%X, want 0x%X", got, MaxTransactSizeSmall)
	}
	for _, d := range []Dialect{SMB210, SMB300, SMB302, SMB311} {
		if got := d.MaxTransactSize(); got != MaxTransactSizeLarge {
			t.Fatalf("%v MaxTransactSize = 0x%X, want 0x%X", d, got, MaxTransactSizeLarge)
		}
	}
}

func TestFeatureMatrix(t *testing.T) {
	cases := []struct {
		d          Dialect
		smb3       bool
		multi      bool
		encryption bool
		preauth    bool
		signing    SigningAlgorithm
	}{
		{SMB202, false, false, false, false, SigningHMACSHA256},
		{SMB210, false, true, false, false, SigningHMACSHA256},
		{SMB300, true, true, true, false, SigningAESCMAC},
		{SMB302, true, true, true, false, SigningAESCMAC},
		{SMB311, true, true, true, true, SigningAESCMAC},
	}
	for _, c := range cases {
		if c.d.IsSMB3() != c.smb3 {
			t.Errorf("%v IsSMB3 = %v", c.d, c.d.IsSMB3())
		}
		if c.d.SupportsMultiCredit() != c.multi {
			t.Errorf("%v SupportsMultiCredit = %v", c.d, c.d.SupportsMultiCredit())
		}
		if c.d.SupportsEncryption() != c.encryption {
			t.Errorf("%v SupportsEncryption = %v", c.d, c.d.SupportsEncryption())
		}
		if c.d.SupportsPreauthIntegrity() != c.preauth {
			t.Errorf("%v SupportsPreauthIntegrity = %v", c.d, c.d.SupportsPreauthIntegrity())
		}
		if c.d.Signing() != c.signing {
			t.Errorf("%v Signing = %v", c.d, c.d.Signing())
		}
	}
}

func TestParse(t *testing.T) {
	ok := map[string]Dialect{
		"2.0.2":    SMB202,
		"2.1":      SMB210,
		"3.0":      SMB300,
		"3.0.2":    SMB302,
		"3.1.1":    SMB311,
		" 3.1.1 ":  SMB311,
		"SMB3.1.1": SMB311,
		"smb2.0.2": SMB202,
	}
	for s, want := range ok {
		got, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if got != want {
			t.Fatalf("Parse(%q) = %v, want %v", s, got, want)
		}
	}
	for _, s := range []string{"", "4.0", "2.???", "abc"} {
		if _, err := Parse(s); err == nil {
			t.Fatalf("Parse(%q) 应当失败", s)
		}
	}
}

func TestRange(t *testing.T) {
	got := Range(SMB210, SMB302)
	want := []Dialect{SMB210, SMB300, SMB302}
	if len(got) != len(want) {
		t.Fatalf("Range = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Range = %v, want %v", got, want)
		}
	}
	if len(Range(0, 0)) != len(All) {
		t.Fatal("Range(0,0) 应返回全部方言")
	}
}
