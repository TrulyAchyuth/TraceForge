// Package hist implements the fixed-bound latency histogram used by the
// rollup tables, and quantile helpers shared by detection and investigation.
package hist

import (
	"math"
	"sort"
	"time"
)

// BoundsMs are the inclusive upper bounds, in milliseconds, of every bucket
// except the last, which is unbounded. The spacing is roughly exponential so
// relative quantile error stays bounded from 1 ms to 10 s.
var BoundsMs = []float64{
	1, 2, 4, 6, 8, 10, 15, 20, 30, 40, 50, 75, 100, 150, 200, 300, 400, 500,
	750, 1000, 1500, 2000, 3000, 4000, 5000, 7500, 10000,
}

// NumBuckets is the number of histogram buckets, including the overflow bucket.
var NumBuckets = len(BoundsMs) + 1

// BucketSeconds is the width of a rollup time bucket.
const BucketSeconds = 10

// BucketDuration is BucketSeconds as a time.Duration.
const BucketDuration = BucketSeconds * time.Second

// New returns an empty histogram.
func New() []int64 { return make([]int64, NumBuckets) }

// Index returns the bucket index for a duration in nanoseconds.
func Index(durationNs int64) int {
	ms := float64(durationNs) / 1e6
	return sort.SearchFloat64s(BoundsMs, ms)
}

// Add accumulates src into dst. Histograms of unexpected length are ignored
// rather than trusted, since they originate from storage.
func Add(dst, src []int64) {
	if len(dst) != NumBuckets || len(src) != NumBuckets {
		return
	}
	for i, v := range src {
		dst[i] += v
	}
}

// Count returns the number of observations in h.
func Count(h []int64) int64 {
	var n int64
	for _, v := range h {
		n += v
	}
	return n
}

// Quantile estimates the q-quantile (0..1) of h in milliseconds using linear
// interpolation inside the containing bucket. It returns 0 for an empty
// histogram. Observations in the overflow bucket are reported at the last
// finite bound, so results above 10 s are a lower bound.
func Quantile(h []int64, q float64) float64 {
	total := Count(h)
	if total == 0 || len(h) != NumBuckets {
		return 0
	}
	rank := q * float64(total)
	var cum float64
	for i, c := range h {
		if c == 0 {
			continue
		}
		next := cum + float64(c)
		if rank <= next {
			if i >= len(BoundsMs) {
				return BoundsMs[len(BoundsMs)-1]
			}
			lower := 0.0
			if i > 0 {
				lower = BoundsMs[i-1]
			}
			frac := (rank - cum) / float64(c)
			return lower + frac*(BoundsMs[i]-lower)
		}
		cum = next
	}
	return BoundsMs[len(BoundsMs)-1]
}

// TruncateBucket floors t to the rollup bucket boundary.
func TruncateBucket(t time.Time) time.Time {
	return t.UTC().Truncate(BucketDuration)
}

// QuantileSorted returns the q-quantile of an ascending slice using linear
// interpolation between closest ranks. It returns 0 for an empty slice.
func QuantileSorted(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (pos-float64(lo))*(sorted[hi]-sorted[lo])
}

// Median returns the median of values without modifying the input.
func Median(values []float64) float64 {
	c := append([]float64(nil), values...)
	sort.Float64s(c)
	return QuantileSorted(c, 0.5)
}

// MAD returns the median absolute deviation of values around their median.
func MAD(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	m := Median(values)
	dev := make([]float64, len(values))
	for i, v := range values {
		dev[i] = math.Abs(v - m)
	}
	return Median(dev)
}
