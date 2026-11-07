// Package perf implements the Performance Service's sample semantics: the
// five-minute bucket rule measured on the live service and a deterministic
// value function, so a query for the same entity, label and bucket always
// returns the same integer, in any process, in any request order.
package perf

import (
	"encoding/binary"
	"hash/fnv"
	"strings"
	"time"
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

// Value is the sample for (seed, entityRefId, label) at a bucket, scaled by
// the label's advertised unit. "<x>Actual" equals "<x>" (the live service
// returned identical values for every such pair); "<x>Raw" is a cumulative
// counter that grows with the bucket index.
func Value(seed, ref, label, unit string, bucket time.Time) int64 {
	if base, ok := strings.CutSuffix(label, "Actual"); ok {
		return Value(seed, ref, base, unit, bucket)
	}
	if base, ok := strings.CutSuffix(label, "Raw"); ok {
		// cumulative since a seeded boot epoch: monotonic per entity
		idx := bucket.Unix() / int64(Interval/time.Second)
		return idx*(1+int64(hash(seed, ref, base, 0)%7)) + int64(hash(seed, ref, base, 1)%100)
	}
	h := hash(seed, ref, label, uint64(bucket.Unix()))
	switch unit {
	case "rate_bytes":
		return int64(h % 50_000_000) // bytes per second
	case "time_ms":
		return 500 + int64(h%9_500) // the service reports latencies in microseconds
	case "time_ns":
		return 1_000 + int64(h%99_000)
	case "permille":
		return int64(h % 20)
	case "percentage":
		return int64(h % 100)
	case "size_bytes":
		return 1_000_000_000_000 + int64(h%9_000_000_000_000)
	default: // number
		return int64(h % 5_000)
	}
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
