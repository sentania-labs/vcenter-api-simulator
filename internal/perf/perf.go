// Package perf implements the Performance Service's sample semantics: the
// five-minute bucket rule measured on the live service and a deterministic
// value model, so a query for the same entity, label and bucket always
// returns the same value, in any process, in any request order.
//
// Values are plausible against the live capture: every label of every
// family has an observed envelope (catalog.Stats: min, quartiles, max over
// every entity and sample captured) and the model never leaves it. Each
// entity sits at its own seeded level inside the interquartile range, the
// level follows a business-hours day (busier mid-afternoon UTC, quieter at
// night), successive buckets drift smoothly instead of jumping, and a
// small per-bucket noise term keeps buckets from repeating. Labels the
// live service held constant stay constant. Nothing is stored: a value is
// a pure function of (telemetry seed, entityRefId, label, bucket).
package perf

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/catalog"
)

// Interval is the sampling interval; metricsCollectInterval on every series.
const Interval = 5 * time.Minute

// Retention bounds how far back buckets are served. The live service's
// default retention is 90 days; windows older than that returned the empty
// echo in the capture (edge case win1yearago).
const Retention = 90 * 24 * time.Hour

// SampleLayout is the sampleInfo timestamp format: naive UTC, no zone.
const SampleLayout = "2006-01-02 15:04:05"

// Buckets returns the sample timestamps for a window, ascending.
//
// Rule measured on vCenter 9.1.1 (recon section 5.3): a bucket B on the
// UTC :00/:05/... grid is returned when B <= end and B+5m > start. A 15-minute
// window returns 4 samples, a 30-minute window 7, a 4-minute window 2. Buckets
// in the future (B > now) do not exist yet, and buckets older than Retention
// are gone; both give an empty result, as the live edge cases did.
func Buckets(start, end, now time.Time) []time.Time {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil
	}
	first := start.UTC().Truncate(Interval)
	latest := now.UTC().Truncate(Interval)
	oldest := now.UTC().Add(-Retention).Truncate(Interval)
	var out []time.Time
	for b := first; !b.After(end); b = b.Add(Interval) {
		if b.After(latest) {
			break
		}
		if b.Before(oldest) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// Format renders sampleInfo entries.
func Format(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.UTC().Format(SampleLayout)
	}
	return out
}

// Value renders the sample for (seed, entityRefId, label) at a bucket as
// the service's CSV cell. st is the label's observed envelope; without one
// (ok false) the unit decides a generic range. "<x>Actual" equals "<x>"
// (the live service returned identical values for every such pair);
// "<x>Raw" is a cumulative counter: it never decreases from one bucket to
// the next and, with an envelope, climbs through it over the retention
// period (so a counter the live service held at zero stays at zero).
func Value(seed, ref, label string, st catalog.Stats, ok bool, unit string, bucket time.Time) string {
	if base, found := strings.CutSuffix(label, "Actual"); found {
		return Value(seed, ref, base, st, ok, unit, bucket)
	}
	if base, found := strings.CutSuffix(label, "Raw"); found {
		idx := bucket.Unix() / int64(Interval/time.Second)
		if !ok {
			// cumulative since a seeded boot epoch: monotonic per entity
			return strconv.FormatInt(idx*(1+int64(hash(seed, ref, base, 0)%7))+int64(hash(seed, ref, base, 1)%100), 10)
		}
		period := int64(Retention / Interval)
		phase := (idx + int64(hash(seed, ref, base, 0)%uint64(period))) % period
		v := st.Min + (st.Max-st.Min)*float64(phase)/float64(period)
		if st.Float {
			return strconv.FormatFloat(v, 'f', 2, 64)
		}
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
	if !ok {
		st = genericStats(unit)
	}
	v := model(seed, ref, label, st, bucket)
	if st.Float {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return strconv.FormatInt(int64(math.Round(v)), 10)
}

// model is the value function. The entity's level sits inside the
// interquartile range; the value then moves around the level by at most
// the room the envelope leaves on the narrower side, driven by the day
// shape, a slow drift and a little noise. A label whose range is tiny
// next to its magnitude (free capacity) therefore wobbles inside that
// range instead of swinging with the day, and a sparse label (mostly zero
// with spikes) stays near zero.
func model(seed, ref, label string, st catalog.Stats, bucket time.Time) float64 {
	if st.Max <= st.Min {
		return st.Min // the live service held this label constant
	}
	lo, hi := st.P25, st.P75
	if hi <= lo {
		lo, hi = st.Min, st.Max
	}
	level := lo + (hi-lo)*unit(hash(seed, ref, label, 0))
	room := math.Min(level-st.Min, st.Max-level)

	// business-hours day: busiest at 14:00 UTC, quietest at 02:00
	hour := float64(bucket.UTC().Hour()) + float64(bucket.UTC().Minute())/60
	day := math.Sin(2 * math.Pi * (hour - 8) / 24)

	// slow drift: linear interpolation between hourly knots
	knot := bucket.Unix() / 3600
	frac := float64(bucket.Unix()%3600) / 3600
	k0 := unit(hash(seed, ref, label, uint64(knot)))
	k1 := unit(hash(seed, ref, label, uint64(knot+1)))
	drift := 2*(k0*(1-frac)+k1*frac) - 1

	// per-bucket noise
	noise := 2*unit(hash(seed, ref, label, uint64(bucket.Unix())|1<<62)) - 1

	v := level + room*(0.5*day+0.35*drift+0.15*noise)
	if v < st.Min {
		v = st.Min
	}
	if v > st.Max {
		v = st.Max
	}
	return v
}

// genericStats is the envelope for a label the capture did not observe,
// by its advertised unit.
func genericStats(u string) catalog.Stats {
	switch u {
	case "rate_bytes":
		return catalog.Stats{Min: 0, P25: 1_000_000, P50: 5_000_000, P75: 20_000_000, Max: 50_000_000}
	case "time_ms": // the service reports latencies in microseconds
		return catalog.Stats{Min: 200, P25: 800, P50: 2_000, P75: 5_000, Max: 10_000}
	case "time_ns":
		return catalog.Stats{Min: 1_000, P25: 10_000, P50: 30_000, P75: 60_000, Max: 100_000}
	case "permille":
		return catalog.Stats{Min: 0, P25: 0, P50: 2, P75: 8, Max: 20}
	case "percentage":
		return catalog.Stats{Min: 0, P25: 10, P50: 30, P75: 60, Max: 100}
	case "size_bytes":
		return catalog.Stats{Min: 1e12, P25: 2e12, P50: 4e12, P75: 6e12, Max: 1e13}
	default: // number
		return catalog.Stats{Min: 0, P25: 100, P50: 500, P75: 2_000, Max: 5_000}
	}
}

// unit maps a hash to [0,1).
func unit(h uint64) float64 {
	return float64(h>>11) / float64(1<<53)
}

func hash(seed, ref, label string, salt uint64) uint64 {
	h := fnv.New64a()
	h.Write([]byte(seed))
	h.Write([]byte{0})
	h.Write([]byte(ref))
	h.Write([]byte{0})
	h.Write([]byte(label))
	h.Write([]byte{0})
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], salt)
	h.Write(b[:])
	return h.Sum64()
}
