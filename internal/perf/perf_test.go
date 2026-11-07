package perf

import (
	"strings"
	"testing"
	"time"
)

// Windows and results from the live capture (fixtures/live/9.1.1, edge-* and
// perf-* request/response pairs, queried at 2026-11-06T23:38Z).
func TestBucketsMatchLiveCapture(t *testing.T) {
	now := time.Date(2026, 11, 6, 23, 38, 34, 0, time.UTC)
	cases := []struct {
		name       string
		start, end time.Time
		want       string
	}{
		{"15 minutes (every perf-* fixture)",
			time.Date(2026, 11, 6, 23, 23, 22, 340e6, time.UTC), time.Date(2026, 11, 6, 23, 38, 22, 340e6, time.UTC),
			"2026-11-06 23:20:00,2026-11-06 23:25:00,2026-11-06 23:30:00,2026-11-06 23:35:00"},
		{"30 minutes (edge win30min)",
			time.Date(2026, 11, 6, 23, 8, 22, 340e6, time.UTC), time.Date(2026, 11, 6, 23, 38, 22, 340e6, time.UTC),
			"2026-11-06 23:05:00,2026-11-06 23:10:00,2026-11-06 23:15:00,2026-11-06 23:20:00,2026-11-06 23:25:00,2026-11-06 23:30:00,2026-11-06 23:35:00"},
		{"4 minutes (edge win4min)",
			time.Date(2026, 11, 6, 23, 34, 22, 340e6, time.UTC), time.Date(2026, 11, 6, 23, 38, 22, 340e6, time.UTC),
			"2026-11-06 23:30:00,2026-11-06 23:35:00"},
		{"start after end (edge start_after_end)",
			time.Date(2026, 11, 6, 23, 38, 22, 340e6, time.UTC), time.Date(2026, 11, 6, 23, 23, 22, 340e6, time.UTC), ""},
		{"future window (edge future_window)",
			time.Date(2026, 11, 7, 23, 23, 22, 0, time.UTC), time.Date(2026, 11, 7, 23, 38, 22, 0, time.UTC), ""},
		{"one year ago (edge win1yearago)",
			time.Date(2025, 11, 6, 23, 23, 22, 0, time.UTC), time.Date(2025, 11, 6, 23, 38, 22, 0, time.UTC), ""},
	}
	for _, c := range cases {
		got := strings.Join(Format(Buckets(c.start, c.end, now)), ",")
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestBucketsNeverInFuture(t *testing.T) {
	now := time.Date(2026, 11, 6, 23, 38, 34, 0, time.UTC)
	got := Buckets(now.Add(-time.Hour), now.Add(time.Hour), now)
	if len(got) == 0 || got[len(got)-1].After(now) {
		t.Fatalf("buckets %v cross now", got)
	}
}

func TestValueDeterministicAndUnitScaled(t *testing.T) {
	b := time.Date(2026, 11, 6, 23, 35, 0, 0, time.UTC)
	if Value("s", "host-domclient:x", "iops", "number", b) != Value("s", "host-domclient:x", "iops", "number", b) {
		t.Fatal("not deterministic")
	}
	if Value("s", "host-domclient:x", "iops", "number", b) == Value("t", "host-domclient:x", "iops", "number", b) {
		t.Fatal("seed has no effect")
	}
	if Value("s", "a", "iops", "number", b) == Value("s", "b", "iops", "number", b) {
		t.Fatal("entity has no effect")
	}
	if v := Value("s", "x", "pctMemUsed", "permille", b); v < 0 || v > 1000 {
		t.Fatalf("permille out of range: %d", v)
	}
	if v := Value("s", "x", "pct", "percentage", b); v < 0 || v > 100 {
		t.Fatalf("percentage out of range: %d", v)
	}
	if Value("s", "x", "rxErrActual", "permille", b) != Value("s", "x", "rxErr", "permille", b) {
		t.Fatal("Actual twin must equal its base")
	}
	if Value("s", "x", "rxErrRaw", "permille", b.Add(Interval)) <= Value("s", "x", "rxErrRaw", "permille", b) {
		t.Fatal("Raw counter must grow")
	}
}
