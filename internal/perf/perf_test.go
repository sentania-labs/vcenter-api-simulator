package perf

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
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

// Without an observed envelope the unit decides a generic range.
func TestValueGenericRanges(t *testing.T) {
	b := time.Date(2026, 11, 6, 23, 35, 0, 0, time.UTC)
	none := catalog.Stats{}
	for _, c := range []struct {
		unit   string
		lo, hi float64
	}{{"permille", 0, 1000}, {"percentage", 0, 100}, {"number", 0, 5000}, {"time_ms", 0, 10000}} {
		raw := Value("s", "x", "metric", none, false, c.unit, b)
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < c.lo || v > c.hi {
			t.Errorf("%s: %q outside [%v, %v]", c.unit, raw, c.lo, c.hi)
		}
	}
}

// Values stay inside the envelope the live service showed for every label,
// hold constant where the service did, are the same for the same
// (seed, entity, label, bucket) and differ across entities and seeds, and
// move smoothly from one bucket to the next.
func TestValuesArePlausibleAgainstTheCapture(t *testing.T) {
	c := catalog.MustLoad()
	start := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	families := 0
	constants, varying := 0, 0
	for _, fam := range c.DataFamilies() {
		f := c.Families[fam]
		if len(f.Stats) == 0 {
			t.Errorf("%s: no value statistics in the catalog", fam)
			continue
		}
		families++
		for _, label := range f.Labels {
			if strings.HasSuffix(label, "Raw") {
				// cumulative counters: inside the envelope and never decreasing
				st, ok := c.LabelStats(fam, label)
				if !ok {
					continue
				}
				var prev float64
				for i := 0; i < 288; i++ {
					b := start.Add(time.Duration(i) * Interval)
					v, err := strconv.ParseFloat(Value("seed", "entity-a", label, st, ok, c.Unit(fam, label), b), 64)
					if err != nil || v < st.Min || v > st.Max {
						t.Errorf("%s/%s: %v outside [%v, %v] (%v)", fam, label, v, st.Min, st.Max, err)
					}
					if i > 0 && v < prev {
						t.Errorf("%s/%s: counter decreased %v -> %v", fam, label, prev, v)
					}
					prev = v
				}
				continue
			}
			st, ok := c.LabelStats(fam, label)
			if !ok {
				// the live service returned "None" for every sample of this
				// label; the simulator falls back to the unit's generic range
				st = genericStats(c.Unit(fam, label))
			}
			var prev float64
			var moves, jumps int
			for i := 0; i < 288; i++ { // one day of buckets
				b := start.Add(time.Duration(i) * Interval)
				raw := Value("seed", "entity-a", label, st, ok, c.Unit(fam, label), b)
				v, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					t.Fatalf("%s/%s: %q: %v", fam, label, raw, err)
				}
				if strings.Contains(raw, ".") != st.Float {
					t.Errorf("%s/%s: decimals %v, live %v", fam, label, strings.Contains(raw, "."), st.Float)
				}
				if v < st.Min || v > st.Max {
					t.Errorf("%s/%s: %v outside [%v, %v]", fam, label, v, st.Min, st.Max)
				}
				if i > 0 && v != prev {
					moves++
					if span := st.Max - st.Min; span >= 10 && math.Abs(v-prev) > 0.5*span {
						jumps++
					}
				}
				prev = v
			}
			if st.Max == st.Min {
				constants++
				if moves != 0 {
					t.Errorf("%s/%s: live constant, simulated moved %d times", fam, label, moves)
				}
			} else {
				varying++
				// a label whose quartiles (nearly) coincide is mostly one value
				// with spikes on the live service; sitting still is plausible
				if moves == 0 && st.P75-st.P25 >= 10 {
					t.Errorf("%s/%s: never changes", fam, label)
				}
				if jumps > 288/10 {
					t.Errorf("%s/%s: %d jumps of more than half the range in a day", fam, label, jumps)
				}
			}
		}
	}
	if families < 40 || constants == 0 || varying == 0 {
		t.Fatalf("families %d constants %d varying %d", families, constants, varying)
	}
	st, _ := c.LabelStats("host-domclient", "iops")
	b := start.Add(7 * Interval)
	series := func(seed, ref string) string {
		var out []string
		for i := 0; i < 24; i++ {
			out = append(out, Value(seed, ref, "iops", st, true, "number", start.Add(time.Duration(i)*Interval)))
		}
		return strings.Join(out, ",")
	}
	if series("seed", "e") != series("seed", "e") {
		t.Error("not deterministic")
	}
	if series("seed", "e") == series("seed", "f") {
		t.Error("entities alias")
	}
	if series("seed", "e") == series("other", "e") {
		t.Error("seeds alias")
	}
	if Value("seed", "e", "iopsActual", st, true, "number", b) != Value("seed", "e", "iops", st, true, "number", b) {
		t.Error("Actual twin differs from its base")
	}
	raw0, _ := strconv.ParseInt(Value("seed", "e", "iopsRaw", st, true, "number", b), 10, 64)
	raw1, _ := strconv.ParseInt(Value("seed", "e", "iopsRaw", st, true, "number", b.Add(Interval)), 10, 64)
	if raw1 < raw0 {
		t.Error("Raw counter decreased")
	}
	far, _ := strconv.ParseInt(Value("seed", "e", "iopsRaw", st, true, "number", b.Add(24*time.Hour)), 10, 64)
	if far <= raw0 {
		t.Error("Raw counter does not climb over a day")
	}
}
