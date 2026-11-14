// Package detect implements TraceForge's incident detection: explainable
// threshold and robust-statistics rules evaluated over the rollup tables.
package detect

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

// Rule identifiers.
const (
	RuleErrorRate   = "error_rate"
	RuleLatency     = "latency_anomaly"
	RuleTraffic     = "traffic_change"
	RuleDependency  = "dependency_degradation"
	RuleUnavailable = "service_unavailable"
	RuleRetries     = "retry_anomaly"
)

// RuleDescriptions explains each rule in operator terms.
var RuleDescriptions = map[string]string{
	RuleErrorRate:   "Share of a service's requests that ended in error over the evaluation window exceeds the threshold and is well above its baseline.",
	RuleLatency:     "A service's p95 latency over the evaluation window is a robust outlier against its per-bucket baseline p95 (median + k·MAD) and exceeds the ratio and absolute-delta floors.",
	RuleTraffic:     "A service's request rate rose above, or fell below, a multiple of its baseline rate.",
	RuleDependency:  "Calls from one service to another are failing or slow, measured on the caller's client spans.",
	RuleUnavailable: "Nearly all calls to a service fail while the service itself reports no handled requests.",
	RuleRetries:     "The share of calls on an edge that are retries exceeds the threshold and is well above baseline.",
}

// Config holds every detection threshold. All fields are operator-editable.
type Config struct {
	// WindowSeconds is the evaluation window length.
	WindowSeconds int `json:"window_seconds"`
	// BaselineSeconds is how much incident-free history forms the baseline.
	BaselineSeconds int `json:"baseline_seconds"`
	// MinRequests is the minimum window volume for a rule to be evaluated.
	MinRequests int64 `json:"min_requests"`
	// MinBaselineRequests is the minimum baseline volume for baseline-relative rules.
	MinBaselineRequests int64 `json:"min_baseline_requests"`
	// ErrorRateThreshold is the absolute error-rate floor (0..1).
	ErrorRateThreshold float64 `json:"error_rate_threshold"`
	// ErrorRateBaselineFactor requires the rate to exceed baseline × factor.
	ErrorRateBaselineFactor float64 `json:"error_rate_baseline_factor"`
	// LatencyRatio requires window p95 ≥ baseline p95 × ratio.
	LatencyRatio float64 `json:"latency_ratio"`
	// LatencyMinDeltaMs requires window p95 − baseline p95 ≥ this many ms.
	LatencyMinDeltaMs float64 `json:"latency_min_delta_ms"`
	// LatencyMADs is k in the robust outlier test p95 > median + k·1.4826·MAD.
	LatencyMADs float64 `json:"latency_mads"`
	// TrafficSpikeFactor and TrafficDropFactor bound the rate ratio.
	TrafficSpikeFactor float64 `json:"traffic_spike_factor"`
	TrafficDropFactor  float64 `json:"traffic_drop_factor"`
	// UnavailableErrorRate is the edge error rate that, with no handled
	// requests on the callee, marks it unavailable.
	UnavailableErrorRate float64 `json:"unavailable_error_rate"`
	// RetryRatioThreshold is the share of calls that are retries.
	RetryRatioThreshold float64 `json:"retry_ratio_threshold"`
	// ResolveAfterSeconds is how long an incident must be quiet to auto-resolve.
	ResolveAfterSeconds int `json:"resolve_after_seconds"`
}

// DefaultConfig returns the default thresholds.
func DefaultConfig() Config {
	return Config{
		WindowSeconds:           60,
		BaselineSeconds:         600,
		MinRequests:             20,
		MinBaselineRequests:     50,
		ErrorRateThreshold:      0.05,
		ErrorRateBaselineFactor: 2,
		LatencyRatio:            2,
		LatencyMinDeltaMs:       50,
		LatencyMADs:             3,
		TrafficSpikeFactor:      3,
		TrafficDropFactor:       0.25,
		UnavailableErrorRate:    0.9,
		RetryRatioThreshold:     0.2,
		ResolveAfterSeconds:     60,
	}
}

// Validate rejects configurations that would make rules meaningless.
func (c Config) Validate() error {
	check := func(ok bool, msg string) error {
		if !ok {
			return errors.New(msg)
		}
		return nil
	}
	return errors.Join(
		check(c.WindowSeconds >= 20 && c.WindowSeconds <= 600 && c.WindowSeconds%hist.BucketSeconds == 0,
			fmt.Sprintf("window_seconds must be a multiple of %d between 20 and 600", hist.BucketSeconds)),
		check(c.BaselineSeconds >= 120 && c.BaselineSeconds <= 3600, "baseline_seconds must be between 120 and 3600"),
		check(c.MinRequests >= 1 && c.MinRequests <= 100000, "min_requests must be between 1 and 100000"),
		check(c.MinBaselineRequests >= 1 && c.MinBaselineRequests <= 1000000, "min_baseline_requests must be between 1 and 1000000"),
		check(c.ErrorRateThreshold > 0 && c.ErrorRateThreshold <= 1, "error_rate_threshold must be in (0, 1]"),
		check(c.ErrorRateBaselineFactor >= 1 && c.ErrorRateBaselineFactor <= 100, "error_rate_baseline_factor must be between 1 and 100"),
		check(c.LatencyRatio > 1 && c.LatencyRatio <= 100, "latency_ratio must be in (1, 100]"),
		check(c.LatencyMinDeltaMs >= 0 && c.LatencyMinDeltaMs <= 60000, "latency_min_delta_ms must be between 0 and 60000"),
		check(c.LatencyMADs >= 0 && c.LatencyMADs <= 20, "latency_mads must be between 0 and 20"),
		check(c.TrafficSpikeFactor > 1 && c.TrafficSpikeFactor <= 1000, "traffic_spike_factor must be in (1, 1000]"),
		check(c.TrafficDropFactor > 0 && c.TrafficDropFactor < 1, "traffic_drop_factor must be in (0, 1)"),
		check(c.UnavailableErrorRate > 0.5 && c.UnavailableErrorRate <= 1, "unavailable_error_rate must be in (0.5, 1]"),
		check(c.RetryRatioThreshold > 0 && c.RetryRatioThreshold <= 1, "retry_ratio_threshold must be in (0, 1]"),
		check(c.ResolveAfterSeconds >= 30 && c.ResolveAfterSeconds <= 3600, "resolve_after_seconds must be between 30 and 3600"),
	)
}

// Agg is a sum of service rollup buckets.
type Agg struct {
	Requests int64
	Errors   int64
	Hist     []int64
	Seconds  float64
}

// ErrorRate returns errors/requests, or 0 with no requests.
func (a Agg) ErrorRate() float64 {
	if a.Requests == 0 {
		return 0
	}
	return float64(a.Errors) / float64(a.Requests)
}

// Rate returns requests per second.
func (a Agg) Rate() float64 {
	if a.Seconds <= 0 {
		return 0
	}
	return float64(a.Requests) / a.Seconds
}

// P95 returns the 95th percentile latency in milliseconds.
func (a Agg) P95() float64 { return hist.Quantile(a.Hist, 0.95) }

// EdgeAgg is a sum of edge rollup buckets.
type EdgeAgg struct {
	Calls   int64
	Errors  int64
	Retries int64
	Hist    []int64
}

// ErrorRate returns errors/calls, or 0 with no calls.
func (a EdgeAgg) ErrorRate() float64 {
	if a.Calls == 0 {
		return 0
	}
	return float64(a.Errors) / float64(a.Calls)
}

// RetryRatio returns retries/calls, or 0 with no calls.
func (a EdgeAgg) RetryRatio() float64 {
	if a.Calls == 0 {
		return 0
	}
	return float64(a.Retries) / float64(a.Calls)
}

// ServiceInput is one service's window and baseline for evaluation.
type ServiceInput struct {
	Name     string
	Window   Agg
	Baseline Agg
	// BaselineBucketP95 holds the p95 of each baseline bucket that had traffic.
	BaselineBucketP95 []float64
}

// EdgeInput is one caller→callee edge's window and baseline.
type EdgeInput struct {
	Caller, Callee string
	Window         EdgeAgg
	Baseline       EdgeAgg
}

// Input is everything the rules need for one evaluation.
type Input struct {
	Services []ServiceInput
	Edges    []EdgeInput
}

// minBaselineBucketsForMAD is the fewest baseline buckets for which the
// median/MAD outlier test is applied; below it only the ratio test is used.
const minBaselineBucketsForMAD = 6

// madScale converts MAD to a standard-deviation estimate for normal data.
const madScale = 1.4826

// madFloorFraction floors the MAD at this share of the median. Bucketed
// histograms quantise p95 values, so the raw MAD is frequently zero.
const madFloorFraction = 0.05

// Severity breakpoints.
const (
	errorRateMajor       = 0.20
	errorRateCritical    = 0.50
	latencyRatioMajor    = 4.0
	latencyRatioCritical = 10.0
	trafficSpikeMajor    = 6.0
)

// Evaluate applies every rule and returns the signals that fire, in a
// deterministic order. It is a pure function of its inputs.
func Evaluate(cfg Config, in Input) []model.Signal {
	var out []model.Signal
	byName := map[string]ServiceInput{}
	for _, s := range in.Services {
		byName[s.Name] = s
	}
	for _, s := range in.Services {
		if sg, ok := errorRateRule(cfg, s); ok {
			out = append(out, sg)
		}
		if sg, ok := latencyRule(cfg, s); ok {
			out = append(out, sg)
		}
		if sg, ok := trafficRule(cfg, s); ok {
			out = append(out, sg)
		}
	}
	for _, e := range in.Edges {
		if sg, ok := unavailableRule(cfg, e, byName[e.Callee]); ok {
			out = append(out, sg)
			continue // unavailability subsumes degradation on the same edge
		}
		if sg, ok := dependencyRule(cfg, e); ok {
			out = append(out, sg)
		}
		if sg, ok := retryRule(cfg, e); ok {
			out = append(out, sg)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := model.SeverityRank(a.Severity), model.SeverityRank(b.Severity); ra != rb {
			return ra > rb
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Peer < b.Peer
	})
	return out
}

func errorRateRule(cfg Config, s ServiceInput) (model.Signal, bool) {
	if s.Window.Requests < cfg.MinRequests {
		return model.Signal{}, false
	}
	rate, base := s.Window.ErrorRate(), s.Baseline.ErrorRate()
	if rate < cfg.ErrorRateThreshold {
		return model.Signal{}, false
	}
	hasBaseline := s.Baseline.Requests >= cfg.MinBaselineRequests
	if hasBaseline && rate < base*cfg.ErrorRateBaselineFactor {
		return model.Signal{}, false
	}
	sev := model.SevWarning
	switch {
	case rate >= errorRateCritical:
		sev = model.SevCritical
	case rate >= errorRateMajor:
		sev = model.SevMajor
	}
	msg := fmt.Sprintf("%s error rate is %.1f%% over the last %ds (%d of %d requests)",
		s.Name, rate*100, cfg.WindowSeconds, s.Window.Errors, s.Window.Requests)
	if hasBaseline {
		msg += fmt.Sprintf("; baseline %.1f%%", base*100)
	} else {
		msg += "; no baseline available, absolute threshold only"
	}
	return model.Signal{Rule: RuleErrorRate, Service: s.Name, Severity: sev, Value: rate, Baseline: base,
		Threshold: cfg.ErrorRateThreshold, Unit: "ratio", Message: msg}, true
}

func latencyRule(cfg Config, s ServiceInput) (model.Signal, bool) {
	if s.Window.Requests < cfg.MinRequests || s.Baseline.Requests < cfg.MinBaselineRequests {
		return model.Signal{}, false
	}
	p95, base := s.Window.P95(), s.Baseline.P95()
	if base <= 0 || p95 < base*cfg.LatencyRatio || p95-base < cfg.LatencyMinDeltaMs {
		return model.Signal{}, false
	}
	threshold := base * cfg.LatencyRatio
	detail := ""
	if len(s.BaselineBucketP95) >= minBaselineBucketsForMAD {
		median := hist.Median(s.BaselineBucketP95)
		mad := math.Max(hist.MAD(s.BaselineBucketP95), median*madFloorFraction)
		outlier := median + cfg.LatencyMADs*madScale*mad
		if p95 <= outlier {
			return model.Signal{}, false
		}
		threshold = math.Max(threshold, outlier)
		detail = fmt.Sprintf("; robust outlier bound %.0f ms (median %.0f ms + %.0f×1.4826×MAD)", outlier, median, cfg.LatencyMADs)
	}
	ratio := p95 / base
	sev := model.SevWarning
	switch {
	case ratio >= latencyRatioCritical:
		sev = model.SevCritical
	case ratio >= latencyRatioMajor:
		sev = model.SevMajor
	}
	return model.Signal{Rule: RuleLatency, Service: s.Name, Severity: sev, Value: p95, Baseline: base,
		Threshold: threshold, Unit: "ms",
		Message: fmt.Sprintf("%s p95 latency is %.0f ms over the last %ds, %.1f× its baseline of %.0f ms%s",
			s.Name, p95, cfg.WindowSeconds, ratio, base, detail)}, true
}

func trafficRule(cfg Config, s ServiceInput) (model.Signal, bool) {
	if s.Baseline.Requests < cfg.MinBaselineRequests {
		return model.Signal{}, false
	}
	rate, base := s.Window.Rate(), s.Baseline.Rate()
	if base <= 0 {
		return model.Signal{}, false
	}
	ratio := rate / base
	switch {
	case ratio >= cfg.TrafficSpikeFactor && s.Window.Requests >= cfg.MinRequests:
		sev := model.SevWarning
		if ratio >= trafficSpikeMajor {
			sev = model.SevMajor
		}
		return model.Signal{Rule: RuleTraffic, Service: s.Name, Severity: sev, Value: rate, Baseline: base,
			Threshold: base * cfg.TrafficSpikeFactor, Unit: "req/s",
			Message: fmt.Sprintf("%s request rate rose to %.1f req/s, %.1f× its baseline of %.1f req/s", s.Name, rate, ratio, base)}, true
	case ratio <= cfg.TrafficDropFactor:
		return model.Signal{Rule: RuleTraffic, Service: s.Name, Severity: model.SevWarning, Value: rate, Baseline: base,
			Threshold: base * cfg.TrafficDropFactor, Unit: "req/s",
			Message: fmt.Sprintf("%s request rate fell to %.1f req/s, %.0f%% of its baseline of %.1f req/s", s.Name, rate, ratio*100, base)}, true
	}
	return model.Signal{}, false
}

// unavailableCalleeShare is the largest share of calls the callee may still
// report as handled for the unavailability rule to apply.
const unavailableCalleeShare = 0.1

func unavailableRule(cfg Config, e EdgeInput, callee ServiceInput) (model.Signal, bool) {
	if e.Window.Calls < cfg.MinRequests/2 || e.Window.ErrorRate() < cfg.UnavailableErrorRate {
		return model.Signal{}, false
	}
	if float64(callee.Window.Requests) > unavailableCalleeShare*float64(e.Window.Calls) {
		return model.Signal{}, false
	}
	return model.Signal{Rule: RuleUnavailable, Service: e.Callee, Peer: e.Caller, Severity: model.SevCritical,
		Value: e.Window.ErrorRate(), Baseline: e.Baseline.ErrorRate(), Threshold: cfg.UnavailableErrorRate, Unit: "ratio",
		Message: fmt.Sprintf("%s appears unavailable: %.0f%% of %d calls from %s failed and %s reported %d handled requests",
			e.Callee, e.Window.ErrorRate()*100, e.Window.Calls, e.Caller, e.Callee, callee.Window.Requests)}, true
}

func dependencyRule(cfg Config, e EdgeInput) (model.Signal, bool) {
	if e.Window.Calls < cfg.MinRequests {
		return model.Signal{}, false
	}
	hasBaseline := e.Baseline.Calls >= cfg.MinBaselineRequests
	rate, base := e.Window.ErrorRate(), e.Baseline.ErrorRate()
	if rate >= cfg.ErrorRateThreshold && (!hasBaseline || rate >= base*cfg.ErrorRateBaselineFactor) {
		sev := model.SevWarning
		switch {
		case rate >= errorRateCritical:
			sev = model.SevCritical
		case rate >= errorRateMajor:
			sev = model.SevMajor
		}
		return model.Signal{Rule: RuleDependency, Service: e.Caller, Peer: e.Callee, Severity: sev, Value: rate,
			Baseline: base, Threshold: cfg.ErrorRateThreshold, Unit: "ratio",
			Message: fmt.Sprintf("%.1f%% of calls from %s to %s failed over the last %ds (baseline %.1f%%)",
				rate*100, e.Caller, e.Callee, cfg.WindowSeconds, base*100)}, true
	}
	if !hasBaseline {
		return model.Signal{}, false
	}
	p95, baseP95 := hist.Quantile(e.Window.Hist, 0.95), hist.Quantile(e.Baseline.Hist, 0.95)
	if baseP95 > 0 && p95 >= baseP95*cfg.LatencyRatio && p95-baseP95 >= cfg.LatencyMinDeltaMs {
		ratio := p95 / baseP95
		sev := model.SevWarning
		if ratio >= latencyRatioMajor {
			sev = model.SevMajor
		}
		return model.Signal{Rule: RuleDependency, Service: e.Caller, Peer: e.Callee, Severity: sev, Value: p95,
			Baseline: baseP95, Threshold: baseP95 * cfg.LatencyRatio, Unit: "ms",
			Message: fmt.Sprintf("calls from %s to %s have p95 %.0f ms, %.1f× the baseline of %.0f ms",
				e.Caller, e.Callee, p95, ratio, baseP95)}, true
	}
	return model.Signal{}, false
}

// retryBaselineFactor requires the retry ratio to exceed baseline × factor.
const retryBaselineFactor = 2

func retryRule(cfg Config, e EdgeInput) (model.Signal, bool) {
	if e.Window.Calls < cfg.MinRequests {
		return model.Signal{}, false
	}
	ratio, base := e.Window.RetryRatio(), e.Baseline.RetryRatio()
	if ratio < cfg.RetryRatioThreshold {
		return model.Signal{}, false
	}
	if e.Baseline.Calls >= cfg.MinBaselineRequests && ratio < base*retryBaselineFactor {
		return model.Signal{}, false
	}
	return model.Signal{Rule: RuleRetries, Service: e.Caller, Peer: e.Callee, Severity: model.SevMajor, Value: ratio,
		Baseline: base, Threshold: cfg.RetryRatioThreshold, Unit: "ratio",
		Message: fmt.Sprintf("%.0f%% of calls from %s to %s are retries (%d of %d; baseline %.0f%%)",
			ratio*100, e.Caller, e.Callee, e.Window.Retries, e.Window.Calls, base*100)}, true
}
