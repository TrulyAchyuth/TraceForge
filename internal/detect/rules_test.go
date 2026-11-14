package detect

import (
	"testing"

	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

// h builds a histogram with n observations of about ms milliseconds.
func h(n int64, ms float64) []int64 {
	x := hist.New()
	x[hist.Index(int64(ms*1e6))] = n
	return x
}

func agg(req, errs int64, ms float64) Agg {
	return Agg{Requests: req, Errors: errs, Hist: h(req, ms), Seconds: 60}
}

func rules(sigs []model.Signal) map[string]model.Signal {
	m := map[string]model.Signal{}
	for _, s := range sigs {
		m[s.Rule+"/"+s.Service] = s
	}
	return m
}

func TestErrorRate(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name         string
		win, base    Agg
		want         bool
		wantSeverity string
	}{
		{"below absolute threshold", agg(100, 2, 10), agg(1000, 0, 10), false, ""},
		{"above threshold and 2x baseline", agg(100, 10, 10), agg(1000, 10, 10), true, model.SevWarning},
		{"above threshold but just baseline noise", agg(100, 10, 10), agg(1000, 80, 10), false, ""},
		{"too few requests to judge", agg(10, 10, 10), agg(1000, 0, 10), false, ""},
		{"no baseline: absolute threshold only", agg(100, 10, 10), Agg{}, true, model.SevWarning},
		{"major", agg(100, 25, 10), agg(1000, 0, 10), true, model.SevMajor},
		{"critical", agg(100, 60, 10), agg(1000, 0, 10), true, model.SevCritical},
	}
	for _, c := range cases {
		sg, ok := errorRateRule(cfg, ServiceInput{Name: "svc", Window: c.win, Baseline: c.base})
		if ok != c.want || (ok && sg.Severity != c.wantSeverity) {
			t.Errorf("%s: fired=%v severity=%q, want fired=%v severity=%q", c.name, ok, sg.Severity, c.want, c.wantSeverity)
		}
	}
}

func TestLatencyRequiresRatioAndAbsoluteDelta(t *testing.T) {
	cfg := DefaultConfig()
	base := agg(1000, 0, 100)
	if _, ok := latencyRule(cfg, ServiceInput{Name: "s", Window: agg(100, 0, 500), Baseline: base}); !ok {
		t.Error("5x slower with a 400ms increase must fire")
	}
	if _, ok := latencyRule(cfg, ServiceInput{Name: "s", Window: agg(100, 0, 150), Baseline: base}); ok {
		t.Error("1.5x slower is below the ratio floor")
	}
	// 3x but only +30 ms: ratio passes, absolute delta must veto it.
	if _, ok := latencyRule(cfg, ServiceInput{Name: "s", Window: agg(100, 0, 30), Baseline: agg(1000, 0, 10)}); ok {
		t.Error("a 20 ms increase must not fire")
	}
	if _, ok := latencyRule(cfg, ServiceInput{Name: "s", Window: agg(100, 0, 500), Baseline: agg(10, 0, 100)}); ok {
		t.Error("an under-sampled baseline must not be used for a latency decision")
	}
}

func TestLatencyMADVetoesNoisyBaseline(t *testing.T) {
	cfg := DefaultConfig()
	win, base := agg(100, 0, 250), agg(1000, 0, 100)
	steady := []float64{100, 100, 100, 100, 100, 100, 100}
	noisy := []float64{50, 400, 90, 450, 100, 380, 60}
	if _, ok := latencyRule(cfg, ServiceInput{Name: "s", Window: win, Baseline: base, BaselineBucketP95: steady}); !ok {
		t.Error("a steady baseline makes 250 ms a real outlier")
	}
	if _, ok := latencyRule(cfg, ServiceInput{Name: "s", Window: win, Baseline: base, BaselineBucketP95: noisy}); ok {
		t.Error("a baseline that swings 50-450 ms must not flag 250 ms")
	}
}

func TestUnavailableSubsumesDependency(t *testing.T) {
	cfg := DefaultConfig()
	in := Input{
		Services: []ServiceInput{{Name: "payment", Window: agg(2, 0, 10)}},
		Edges:    []EdgeInput{{Caller: "checkout", Callee: "payment", Window: EdgeAgg{Calls: 100, Errors: 100, Hist: h(100, 10)}}},
	}
	got := Evaluate(cfg, in)
	if len(got) != 1 || got[0].Rule != RuleUnavailable || got[0].Severity != model.SevCritical {
		t.Fatalf("want a single critical %s signal, got %+v", RuleUnavailable, got)
	}
	// If the callee still handles traffic it is degraded, not unavailable.
	in.Services[0].Window = agg(60, 0, 10)
	got = Evaluate(cfg, in)
	if len(got) != 1 || got[0].Rule != RuleDependency {
		t.Fatalf("want a dependency signal when the callee is still serving, got %+v", got)
	}
}

func TestTrafficSpikeAndDrop(t *testing.T) {
	cfg := DefaultConfig()
	base := Agg{Requests: 600, Hist: h(600, 10), Seconds: 600} // 1 req/s
	if _, ok := trafficRule(cfg, ServiceInput{Name: "s", Window: Agg{Requests: 240, Hist: h(240, 10), Seconds: 60}, Baseline: base}); !ok {
		t.Error("4 req/s against a 1 req/s baseline is a spike")
	}
	if sg, ok := trafficRule(cfg, ServiceInput{Name: "s", Window: Agg{Requests: 6, Hist: h(6, 10), Seconds: 60}, Baseline: base}); !ok || sg.Severity != model.SevWarning {
		t.Error("0.1 req/s against 1 req/s is a drop")
	}
	if _, ok := trafficRule(cfg, ServiceInput{Name: "s", Window: Agg{Requests: 60, Hist: h(60, 10), Seconds: 60}, Baseline: base}); ok {
		t.Error("unchanged traffic must not fire")
	}
}

func TestRetryRule(t *testing.T) {
	cfg := DefaultConfig()
	e := EdgeInput{Caller: "a", Callee: "b", Window: EdgeAgg{Calls: 100, Retries: 30}}
	if _, ok := retryRule(cfg, e); !ok {
		t.Error("30% retries with no baseline must fire")
	}
	e.Baseline = EdgeAgg{Calls: 1000, Retries: 250} // retries were already normal
	if _, ok := retryRule(cfg, e); ok {
		t.Error("retries at baseline level must not fire")
	}
}

func TestEvaluateIsDeterministicAndOrderedBySeverity(t *testing.T) {
	cfg := DefaultConfig()
	in := Input{Services: []ServiceInput{
		{Name: "b", Window: agg(100, 10, 10)}, {Name: "a", Window: agg(100, 70, 10)}, {Name: "c", Window: agg(100, 25, 10)},
	}}
	first := Evaluate(cfg, in)
	if len(first) != 3 || first[0].Service != "a" || first[1].Service != "c" || first[2].Service != "b" {
		t.Fatalf("want severity order a(critical), c(major), b(warning); got %+v", rules(first))
	}
	for i := 0; i < 20; i++ {
		again := Evaluate(cfg, in)
		for j := range first {
			if again[j].Service != first[j].Service || again[j].Rule != first[j].Rule {
				t.Fatal("Evaluate is not deterministic")
			}
		}
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	mut := map[string]func(*Config){
		"window not multiple of bucket": func(c *Config) { c.WindowSeconds = 65 },
		"window too small":              func(c *Config) { c.WindowSeconds = 10 },
		"zero error threshold":          func(c *Config) { c.ErrorRateThreshold = 0 },
		"latency ratio of 1":            func(c *Config) { c.LatencyRatio = 1 },
		"drop factor of 1":              func(c *Config) { c.TrafficDropFactor = 1 },
		"resolve too fast":              func(c *Config) { c.ResolveAfterSeconds = 5 },
		"unavailable below 0.5":         func(c *Config) { c.UnavailableErrorRate = 0.4 },
	}
	for name, f := range mut {
		c := DefaultConfig()
		f(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
