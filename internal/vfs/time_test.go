package vfs

import (
	"testing"
	"time"
)

func TestFiletimeKnownVectors(t *testing.T) {
	// 向量来源：
	//   1. MS-DTYP §2.3.3 给出的基准 —— 1601-01-01T00:00:00Z == 0。
	//   2. Unix epoch 1970-01-01T00:00:00Z == 116444736000000000
	//      （docs/protocol-notes.md §11 的常量，可反查 Samba 的 TIME_FIXUP_CONSTANT）。
	//   3. 2001-01-01T00:00:00Z == 126227808000000000
	//      （(978307200 + 11644473600) * 1e7，可用 `date -u -d 2001-01-01 +%s` 复核）。
	cases := []struct {
		name string
		ft   uint64
		want time.Time
	}{
		{"1601 epoch", 0x0000000000000001, time.Date(1601, 1, 1, 0, 0, 0, 100, time.UTC)},
		{"unix epoch", 116444736000000000, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"2001-01-01", 126227808000000000, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FiletimeToTime(c.ft); !got.Equal(c.want) {
				t.Errorf("FiletimeToTime(%d) = %v, want %v", c.ft, got, c.want)
			}
			if got := TimeToFiletime(c.want); got != c.ft {
				t.Errorf("TimeToFiletime(%v) = %d, want %d", c.want, got, c.ft)
			}
		})
	}
}

func TestFiletimeRoundTrip(t *testing.T) {
	for _, tm := range []time.Time{
		time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 8, 12, 34, 56, 789012300, time.UTC),
		time.Date(2262, 4, 11, 23, 47, 16, 0, time.UTC), // UnixNano 的上界附近
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
	} {
		ft := TimeToFiletime(tm)
		back := FiletimeToTime(ft)
		if tm.IsZero() {
			continue
		}
		if !back.Equal(tm) && tm.Unix() != 0-filetimeEpochSecs {
			t.Errorf("往返失真: %v -> %d -> %v", tm, ft, back)
		}
	}
}

func TestFiletimeTruncatesTo100ns(t *testing.T) {
	// 199ns 应向下取整到 100ns（FILETIME 粒度）。
	tm := time.Date(2020, 1, 1, 0, 0, 0, 199, time.UTC)
	ft := TimeToFiletime(tm)
	back := FiletimeToTime(ft)
	if back.Nanosecond() != 100 {
		t.Errorf("纳秒未按 100ns 粒度截断: %d", back.Nanosecond())
	}
}

func TestFiletimeSpecialValues(t *testing.T) {
	if !FiletimeToTime(FiletimeUnspecified).IsZero() {
		t.Error("FILETIME 0 应转成零值 time.Time")
	}
	if TimeToFiletime(time.Time{}) != FiletimeUnspecified {
		t.Error("零值 time.Time 应转成 FILETIME 0")
	}
	if FiletimeIsSet(0) || FiletimeIsSet(FiletimeNoChange) {
		t.Error("0 与 -1 都表示不修改时间")
	}
	if !FiletimeIsSet(1) {
		t.Error("非 0/-1 的 FILETIME 应视为要求修改")
	}
}

func TestFiletimeClamp(t *testing.T) {
	// -1（0xFFFFFFFFFFFFFFFF）不应导致溢出，钳到 maxFiletime。
	if got := FiletimeToTime(FiletimeNoChange); got.Year() < 30000 {
		t.Errorf("超大 FILETIME 应钳制到很远的未来，得到 %v", got)
	}
	// 早于 1601 的时间无法表示，返回 0。
	if got := TimeToFiletime(time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Errorf("1601 年之前应返回 0，得到 %d", got)
	}
}
