package hist

import (
	"math"
	"testing"
	"time"
)

func TestQuantileInterpolatesAndHandlesEdges(t *testing.T) {
	if Quantile(New(), 0.95) != 0 {
		t.Error("empty histogram must report 0")
	}
	h := New()
	h[Index(10e6)] = 100
	p50, p99 := Quantile(h, 0.5), Quantile(h, 0.99)
	if p50 <= 0 || p99 < p50 || p99 > 25 {
		t.Errorf("all observations near 10ms: p50=%v p99=%v", p50, p99)
	}
	slow := New()
	slow[len(slow)-1] = 10 // overflow bucket
	if got := Quantile(slow, 0.5); got != BoundsMs[len(BoundsMs)-1] {
		t.Errorf("overflow observations are reported at the last bound, got %v", got)
	}
}

func TestAddIgnoresMalformedHistograms(t *testing.T) {
	dst := New()
	Add(dst, []int64{1, 2, 3})
	if Count(dst) != 0 {
		t.Error("a histogram of the wrong length must be ignored, not partially added")
	}
	src := New()
	src[3] = 5
	Add(dst, src)
	Add(dst, src)
	if dst[3] != 10 || Count(dst) != 10 {
		t.Errorf("Add must accumulate, got %v", dst[3])
	}
}

func TestIndexIsMonotonic(t *testing.T) {
	prev := -1
	for _, ns := range []int64{0, 1e5, 1e6, 5e6, 1e8, 1e9, 5e9, 1e12} {
		i := Index(ns)
		if i < prev || i >= NumBuckets {
			t.Fatalf("Index(%d)=%d breaks monotonicity (prev %d)", ns, i, prev)
		}
		prev = i
	}
}

func TestMedianMADAndSorted(t *testing.T) {
	if Median(nil) != 0 || MAD(nil) != 0 {
		t.Error("empty input must give 0")
	}
	if m := Median([]float64{5, 1, 3}); m != 3 {
		t.Errorf("median = %v", m)
	}
	if m := Median([]float64{1, 2, 3, 4}); math.Abs(m-2.5) > 1e-9 {
		t.Errorf("even median = %v", m)
	}
	if d := MAD([]float64{1, 1, 2, 2, 4, 6, 9}); d != 1 {
		t.Errorf("MAD = %v, want 1", d)
	}
}

func TestTruncateBucket(t *testing.T) {
	in := time.Date(2026, 1, 1, 12, 0, 17, 5, time.UTC)
	if got := TruncateBucket(in); got.Second() != 10 || got.Nanosecond() != 0 {
		t.Errorf("got %v", got)
	}
}
