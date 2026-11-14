// Package investigate captures incident datasets and derives ranked,
// evidence-backed hypotheses from them.
package investigate

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/replay"
	"github.com/TrulyAchyuth/TraceForge/internal/tree"
)

// Hypothesis kinds.
const (
	KindLatencyOrigin = "latency_origin"
	KindErrorOrigin   = "error_origin"
	KindUnavailable   = "unavailable"
	KindRetryAmplify  = "retry_amplification"
	KindTrafficSurge  = "traffic_surge"
	KindUndetermined  = "undetermined"
)

// Thresholds that decide when a profile counts as anomalous. They are
// deliberately coarse: the aim is to shortlist candidates, not to detect.
const (
	latencyRatioMin     = 1.5  // incident / baseline
	latencyDeltaMinMs   = 20.0 // and at least this much slower
	errorDeltaMin       = 0.03 // +3 percentage points
	unreachableMin      = 0.5  // share of calls with no callee span
	retryRatioMin       = 0.15
	retryBaselineFactor = 2.0
	trafficRatioMin     = 2.0
	minPhaseSamples     = 5
	onsetMinRequests    = 3
	exampleTraceLimit   = 5
)

// MaxScore is the highest attainable evidence score.
const MaxScore = 10

// ServiceProfile compares a service between the baseline and incident
// phases, using the entry spans in the captured traces.
type ServiceProfile struct {
	Service             string     `json:"service"`
	BaselineRequests    int        `json:"baseline_requests"`
	IncidentRequests    int        `json:"incident_requests"`
	BaselineP95Ms       float64    `json:"baseline_p95_ms"`
	IncidentP95Ms       float64    `json:"incident_p95_ms"`
	BaselineSelfP95Ms   float64    `json:"baseline_self_p95_ms"`
	IncidentSelfP95Ms   float64    `json:"incident_self_p95_ms"`
	BaselineErrorRate   float64    `json:"baseline_error_rate"`
	IncidentErrorRate   float64    `json:"incident_error_rate"`
	BaselineOriginRate  float64    `json:"baseline_origin_error_rate"`
	IncidentOriginRate  float64    `json:"incident_origin_error_rate"`
	BaselineRatePerSec  float64    `json:"baseline_rate_per_sec"`
	IncidentRatePerSec  float64    `json:"incident_rate_per_sec"`
	SymptomAnomalous    bool       `json:"symptom_anomalous"`
	OriginAnomalous     bool       `json:"origin_anomalous"`
	Onset               *time.Time `json:"onset,omitempty"`
	baselineSelfSamples []float64
}

// EdgeProfile compares a caller→callee edge between phases.
type EdgeProfile struct {
	Caller              string     `json:"caller"`
	Callee              string     `json:"callee"`
	BaselineCalls       int        `json:"baseline_calls"`
	IncidentCalls       int        `json:"incident_calls"`
	BaselineErrorRate   float64    `json:"baseline_error_rate"`
	IncidentErrorRate   float64    `json:"incident_error_rate"`
	BaselineRetryRatio  float64    `json:"baseline_retry_ratio"`
	IncidentRetryRatio  float64    `json:"incident_retry_ratio"`
	BaselineP95Ms       float64    `json:"baseline_p95_ms"`
	IncidentP95Ms       float64    `json:"incident_p95_ms"`
	IncidentUnreachable float64    `json:"incident_unreachable_rate"`
	BaselineUnreachable float64    `json:"baseline_unreachable_rate"`
	IncidentTimeoutRate float64    `json:"incident_timeout_rate"`
	Anomalous           bool       `json:"anomalous"`
	Onset               *time.Time `json:"onset,omitempty"`
}

// KnownFault is a failure-lab experiment that overlapped the incident. It is
// an observed fact, shown beside the hypotheses but never used to rank them,
// so that the ranking can be judged against it.
type KnownFault struct {
	ExperimentID int64      `json:"experiment_id"`
	Name         string     `json:"name"`
	Scenario     string     `json:"scenario"`
	Targets      []string   `json:"targets"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
}

// Analysis is the result of analysing a dataset.
type Analysis struct {
	Services         []ServiceProfile   `json:"services"`
	Edges            []EdgeProfile      `json:"edges"`
	Hypotheses       []model.Hypothesis `json:"hypotheses"`
	IncidentRequests int                `json:"incident_requests"`
	ImpactedRequests int                `json:"impacted_requests"`
	TracesIncomplete int                `json:"traces_incomplete"`
}

type phaseAgg struct {
	durations, self []float64
	n, errs, origin int
}

type edgePhase struct {
	durations                               []float64
	calls, errs, retries, unreach, timeouts int
}

func p95(v []float64) float64 {
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return math.Round(hist.QuantileSorted(c, 0.95)*1000) / 1000
}

func rate(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func originError(n *tree.Node) bool {
	if !n.IsError() {
		return false
	}
	for _, u := range n.Units() {
		if u.Failed() {
			return false
		}
	}
	return true
}

// EstimateOnsets finds, for each service and edge, the first rollup bucket at
// or after from whose p95 latency or error rate departs from the baseline
// buckets (those before from). Resolution is one bucket.
func EstimateOnsets(svc []model.StatBucket, edges []model.EdgeBucket, from time.Time) (services map[string]time.Time, edgeOnsets map[[2]string]time.Time) {
	services, edgeOnsets = map[string]time.Time{}, map[[2]string]time.Time{}
	type base struct {
		hist     []int64
		req, err int64
	}
	sb := map[string]*base{}
	for _, b := range svc {
		if !b.Bucket.Before(from) {
			continue
		}
		x := sb[b.Service]
		if x == nil {
			x = &base{hist: hist.New()}
			sb[b.Service] = x
		}
		x.req += b.Requests
		x.err += b.Errors
		hist.Add(x.hist, b.Hist)
	}
	for _, b := range svc { // rows are ordered by bucket
		if b.Bucket.Before(from) || b.Requests < onsetMinRequests {
			continue
		}
		if _, done := services[b.Service]; done {
			continue
		}
		x := sb[b.Service]
		if x == nil || x.req == 0 {
			continue
		}
		baseP95, baseErr := hist.Quantile(x.hist, 0.95), float64(x.err)/float64(x.req)
		bp95, berr := hist.Quantile(b.Hist, 0.95), float64(b.Errors)/float64(b.Requests)
		slow := bp95 >= baseP95*latencyRatioMin && bp95-baseP95 >= latencyDeltaMinMs
		failing := b.Errors >= 2 && berr >= math.Max(errorDeltaMin, 2*baseErr)
		if slow || failing {
			services[b.Service] = b.Bucket
		}
	}
	eb := map[[2]string]*base{}
	for _, b := range edges {
		if !b.Bucket.Before(from) {
			continue
		}
		k := [2]string{b.Caller, b.Callee}
		x := eb[k]
		if x == nil {
			x = &base{hist: hist.New()}
			eb[k] = x
		}
		x.req += b.Calls
		x.err += b.Errors
		hist.Add(x.hist, b.Hist)
	}
	for _, b := range edges {
		k := [2]string{b.Caller, b.Callee}
		if b.Bucket.Before(from) || b.Calls < onsetMinRequests {
			continue
		}
		if _, done := edgeOnsets[k]; done {
			continue
		}
		baseP95, baseErr := 0.0, 0.0
		if x := eb[k]; x != nil && x.req > 0 {
			baseP95, baseErr = hist.Quantile(x.hist, 0.95), float64(x.err)/float64(x.req)
		}
		bp95, berr := hist.Quantile(b.Hist, 0.95), float64(b.Errors)/float64(b.Calls)
		slow := baseP95 > 0 && bp95 >= baseP95*latencyRatioMin && bp95-baseP95 >= latencyDeltaMinMs
		failing := b.Errors >= 2 && berr >= math.Max(errorDeltaMin, 2*baseErr)
		retrying := b.Retries >= 2 && float64(b.Retries)/float64(b.Calls) >= retryRatioMin
		if slow || failing || retrying {
			edgeOnsets[k] = b.Bucket
		}
	}
	return services, edgeOnsets
}

// EarliestOnset returns the earliest onset among services and edges, or
// fallback when none was found.
func EarliestOnset(services map[string]time.Time, edges map[[2]string]time.Time, fallback time.Time) time.Time {
	var best time.Time
	for _, t := range services {
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	for _, t := range edges {
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	if best.IsZero() {
		return fallback
	}
	return best
}

// Analyze profiles a dataset and derives ranked hypotheses. It is a pure
// function of the dataset and the known faults; scoreable evidence comes
// only from the dataset.
func Analyze(ds *model.Dataset, faults []KnownFault) Analysis {
	traces := tree.Build(ds.Spans)
	svc := map[string]*[2]phaseAgg{}
	edges := map[[2]string]*[2]edgePhase{}
	phaseIdx := func(p string) int {
		if p == model.PhaseBaseline {
			return 0
		}
		return 1
	}
	var an Analysis
	rootBase := map[string][]float64{} // baseline durations per root operation
	for _, tr := range traces {
		if !tr.Complete {
			an.TracesIncomplete++
			continue
		}
		pi := phaseIdx(tr.Phase)
		if pi == 0 {
			op := tr.Root.Span.Service + "|" + tr.Root.Span.Name
			rootBase[op] = append(rootBase[op], float64(tr.Root.DurationNs())/1e6)
		}
		for _, n := range tr.Nodes {
			if n.IsEntry() {
				a := svc[n.Span.Service]
				if a == nil {
					a = &[2]phaseAgg{}
					svc[n.Span.Service] = a
				}
				a[pi].n++
				a[pi].durations = append(a[pi].durations, float64(n.DurationNs())/1e6)
				a[pi].self = append(a[pi].self, float64(n.ExclusiveNs())/1e6)
				if n.IsError() {
					a[pi].errs++
					if originError(n) {
						a[pi].origin++
					}
				}
			}
			for _, u := range n.Units() {
				if u.Callee == "" {
					continue
				}
				k := [2]string{n.Span.Service, u.Callee}
				e := edges[k]
				if e == nil {
					e = &[2]edgePhase{}
					edges[k] = e
				}
				for i, a := range u.Attempts {
					e[pi].calls++
					e[pi].durations = append(e[pi].durations, float64(a.DurationNs())/1e6)
					if i > 0 {
						e[pi].retries++
					}
					if a.IsError() {
						e[pi].errs++
						if a.ServerChild() == nil {
							e[pi].unreach++
						}
						if a.IsTimeout() {
							e[pi].timeouts++
						}
					}
				}
			}
		}
	}

	svcOnsets, edgeOnsets := EstimateOnsets(ds.ServiceSeries, ds.EdgeSeries, ds.IncidentStart)
	baseSecs := ds.IncidentStart.Sub(ds.BaselineStart).Seconds()
	incSecs := ds.WindowEnd.Sub(ds.IncidentStart).Seconds()
	// Request rates come from the rollup series, which count every request,
	// not from the sampled traces.
	baseReq, incReq := map[string]int64{}, map[string]int64{}
	for _, b := range ds.ServiceSeries {
		if b.Bucket.Before(ds.IncidentStart) {
			baseReq[b.Service] += b.Requests
		} else {
			incReq[b.Service] += b.Requests
		}
	}

	profiles := map[string]*ServiceProfile{}
	for name, a := range svc {
		p := &ServiceProfile{Service: name,
			BaselineRequests: a[0].n, IncidentRequests: a[1].n,
			BaselineP95Ms: p95(a[0].durations), IncidentP95Ms: p95(a[1].durations),
			BaselineSelfP95Ms: p95(a[0].self), IncidentSelfP95Ms: p95(a[1].self),
			BaselineErrorRate: rate(a[0].errs, a[0].n), IncidentErrorRate: rate(a[1].errs, a[1].n),
			BaselineOriginRate: rate(a[0].origin, a[0].n), IncidentOriginRate: rate(a[1].origin, a[1].n),
			baselineSelfSamples: a[0].self,
		}
		if baseSecs > 0 {
			p.BaselineRatePerSec = math.Round(float64(baseReq[name])/baseSecs*100) / 100
		}
		if incSecs > 0 {
			p.IncidentRatePerSec = math.Round(float64(incReq[name])/incSecs*100) / 100
		}
		enough := a[0].n >= minPhaseSamples && a[1].n >= minPhaseSamples
		slower := func(base, inc float64) bool { return inc >= base*latencyRatioMin && inc-base >= latencyDeltaMinMs }
		p.SymptomAnomalous = enough && (slower(p.BaselineP95Ms, p.IncidentP95Ms) ||
			p.IncidentErrorRate-p.BaselineErrorRate >= errorDeltaMin)
		p.OriginAnomalous = enough && (slower(p.BaselineSelfP95Ms, p.IncidentSelfP95Ms) ||
			p.IncidentOriginRate-p.BaselineOriginRate >= errorDeltaMin)
		if t, ok := svcOnsets[name]; ok {
			t := t
			p.Onset = &t
		}
		profiles[name] = p
	}
	edgeProfiles := map[[2]string]*EdgeProfile{}
	for k, e := range edges {
		p := &EdgeProfile{Caller: k[0], Callee: k[1],
			BaselineCalls: e[0].calls, IncidentCalls: e[1].calls,
			BaselineErrorRate: rate(e[0].errs, e[0].calls), IncidentErrorRate: rate(e[1].errs, e[1].calls),
			BaselineRetryRatio: rate(e[0].retries, e[0].calls), IncidentRetryRatio: rate(e[1].retries, e[1].calls),
			BaselineP95Ms: p95(e[0].durations), IncidentP95Ms: p95(e[1].durations),
			BaselineUnreachable: rate(e[0].unreach, e[0].calls), IncidentUnreachable: rate(e[1].unreach, e[1].calls),
			IncidentTimeoutRate: rate(e[1].timeouts, e[1].calls),
		}
		p.Anomalous = e[1].calls >= minPhaseSamples && (p.IncidentErrorRate-p.BaselineErrorRate >= errorDeltaMin ||
			(p.IncidentP95Ms >= p.BaselineP95Ms*latencyRatioMin && p.IncidentP95Ms-p.BaselineP95Ms >= latencyDeltaMinMs && e[0].calls >= minPhaseSamples))
		if t, ok := edgeOnsets[k]; ok {
			t := t
			p.Onset = &t
		}
		edgeProfiles[k] = p
	}

	// Impacted requests: incident-phase root requests that failed or were
	// slower than the baseline p95 of the same root operation by the same
	// margin used elsewhere.
	rootBaseP95 := map[string]float64{}
	for op, v := range rootBase {
		rootBaseP95[op] = p95(v)
	}
	var incidentTraces []*tree.Trace
	impacted := map[string]bool{}
	for _, tr := range traces {
		if !tr.Complete || tr.Phase != model.PhaseIncident {
			continue
		}
		incidentTraces = append(incidentTraces, tr)
		an.IncidentRequests++
		d := float64(tr.Root.DurationNs()) / 1e6
		base, ok := rootBaseP95[tr.Root.Span.Service+"|"+tr.Root.Span.Name]
		if tr.Root.IsError() || (ok && d >= base*latencyRatioMin && d-base >= latencyDeltaMinMs) {
			impacted[tr.ID] = true
			an.ImpactedRequests++
		}
	}

	b := &builder{ds: ds, profiles: profiles, edges: edgeProfiles, traces: incidentTraces, impacted: impacted,
		faults: faults, svcOnsets: svcOnsets, edgeOnsets: edgeOnsets}
	an.Hypotheses = b.hypotheses()

	for _, p := range profiles {
		an.Services = append(an.Services, *p)
	}
	sort.Slice(an.Services, func(i, j int) bool { return an.Services[i].Service < an.Services[j].Service })
	for _, p := range edgeProfiles {
		an.Edges = append(an.Edges, *p)
	}
	sort.Slice(an.Edges, func(i, j int) bool {
		if an.Edges[i].Caller != an.Edges[j].Caller {
			return an.Edges[i].Caller < an.Edges[j].Caller
		}
		return an.Edges[i].Callee < an.Edges[j].Callee
	})
	return an
}

type builder struct {
	ds         *model.Dataset
	profiles   map[string]*ServiceProfile
	edges      map[[2]string]*EdgeProfile
	traces     []*tree.Trace
	impacted   map[string]bool
	faults     []KnownFault
	svcOnsets  map[string]time.Time
	edgeOnsets map[[2]string]time.Time
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (b *builder) sortedEdges() [][2]string {
	keys := make([][2]string, 0, len(b.edges))
	for k := range b.edges {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	return keys
}

func tier(v float64, t1, t2, t3 float64) int {
	switch {
	case v >= t3:
		return 3
	case v >= t2:
		return 2
	case v >= t1:
		return 1
	}
	return 0
}

func pct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

func (b *builder) hypotheses() []model.Hypothesis {
	var hyps []model.Hypothesis
	for _, name := range sortedKeys(b.profiles) {
		p := b.profiles[name]
		if p.BaselineRequests < minPhaseSamples || p.IncidentRequests < minPhaseSamples {
			continue
		}
		if p.IncidentSelfP95Ms >= p.BaselineSelfP95Ms*latencyRatioMin && p.IncidentSelfP95Ms-p.BaselineSelfP95Ms >= latencyDeltaMinMs {
			hyps = append(hyps, b.latencyOrigin(p))
		}
		if p.IncidentOriginRate-p.BaselineOriginRate >= errorDeltaMin {
			hyps = append(hyps, b.errorOrigin(p))
		}
	}
	// Unavailability is judged per callee across all of its callers.
	type inbound struct{ calls, unreach, baseCalls, baseUnreach float64 }
	in := map[string]*inbound{}
	for _, k := range b.sortedEdges() {
		e := b.edges[k]
		x := in[k[1]]
		if x == nil {
			x = &inbound{}
			in[k[1]] = x
		}
		x.calls += float64(e.IncidentCalls)
		x.unreach += e.IncidentUnreachable * float64(e.IncidentCalls)
		x.baseCalls += float64(e.BaselineCalls)
		x.baseUnreach += e.BaselineUnreachable * float64(e.BaselineCalls)
	}
	for _, callee := range sortedKeys(in) {
		x := in[callee]
		if x.calls < minPhaseSamples {
			continue
		}
		share, baseShare := x.unreach/x.calls, 0.0
		if x.baseCalls > 0 {
			baseShare = x.baseUnreach / x.baseCalls
		}
		if share >= unreachableMin && share >= 2*baseShare {
			hyps = append(hyps, b.unavailable(callee, share, baseShare, int(x.calls)))
		}
	}
	for _, k := range b.sortedEdges() {
		e := b.edges[k]
		if e.IncidentCalls >= minPhaseSamples && e.IncidentRetryRatio >= retryRatioMin &&
			e.IncidentRetryRatio >= retryBaselineFactor*e.BaselineRetryRatio {
			hyps = append(hyps, b.retryAmplification(e))
		}
	}
	for _, name := range sortedKeys(b.profiles) {
		p := b.profiles[name]
		if !b.isRootService(name) || p.BaselineRatePerSec <= 0 {
			continue
		}
		if ratio := p.IncidentRatePerSec / p.BaselineRatePerSec; ratio >= trafficRatioMin {
			hyps = append(hyps, b.trafficSurge(p, ratio))
		}
	}
	if len(hyps) == 0 {
		hyps = append(hyps, model.Hypothesis{
			Kind: KindUndetermined, Title: "No origin isolated",
			Statement: "The captured traces do not isolate a service whose own latency or errors changed. The triggering condition may lie outside what spans record (for example an uninstrumented dependency), or the sample may be too small.",
			MaxScore:  MaxScore,
			Evidence: []model.Evidence{{Check: "localisation", Polarity: model.PolarityNeutral, Class: model.ClassObserved,
				Text: fmt.Sprintf("No service shows self-time growth of %.1f× (and %.0f ms) or an origin-error increase of %.0f points between baseline and incident traces.",
					latencyRatioMin, latencyDeltaMinMs, errorDeltaMin*100)}},
		})
	}
	for i := range hyps {
		h := &hyps[i]
		h.MaxScore = MaxScore
		h.Score = 0
		for _, ev := range h.Evidence {
			h.Score += ev.Points
		}
		for _, f := range b.faults {
			for _, t := range f.Targets {
				if t == h.Service || (h.Peer != "" && t == h.Peer) {
					h.MatchesKnownFault = true
				}
			}
		}
	}
	sort.SliceStable(hyps, func(i, j int) bool {
		if hyps[i].Score != hyps[j].Score {
			return hyps[i].Score > hyps[j].Score
		}
		if li, lj := localisationPoints(hyps[i]), localisationPoints(hyps[j]); li != lj {
			return li > lj
		}
		if hyps[i].Service != hyps[j].Service {
			return hyps[i].Service < hyps[j].Service
		}
		return hyps[i].Kind < hyps[j].Kind
	})
	for i := range hyps {
		hyps[i].Rank = i + 1
	}
	return hyps
}

func localisationPoints(h model.Hypothesis) int {
	for _, ev := range h.Evidence {
		if ev.Check == "localisation" {
			return ev.Points
		}
	}
	return 0
}

func (b *builder) isRootService(name string) bool {
	for k := range b.edges {
		if k[1] == name {
			return false
		}
	}
	return true
}

// precedence scores when the candidate's anomaly began relative to the
// earliest anomaly seen anywhere in the incident.
func (b *builder) precedence(label string, onset *time.Time) model.Evidence {
	ev := model.Evidence{Check: "temporal_precedence", Class: model.ClassCorrelation, Polarity: model.PolarityNeutral, Metric: "onset"}
	earliest := EarliestOnset(b.svcOnsets, b.edgeOnsets, time.Time{})
	if onset == nil || earliest.IsZero() {
		ev.Text = fmt.Sprintf("No onset could be determined for %s from the %d-second rollup series, so timing neither supports nor contradicts this hypothesis.", label, hist.BucketSeconds)
		return ev
	}
	lag := onset.Sub(earliest)
	at := onset.UTC().Format("15:04:05")
	switch {
	case lag <= 0:
		ev.Polarity, ev.Points = model.PolaritySupporting, 2
		ev.Text = fmt.Sprintf("%s's anomaly begins in the %s bucket, the earliest of any service or edge in this incident. Bucket resolution is %d s, so ties are possible.", label, at, hist.BucketSeconds)
	case lag <= hist.BucketDuration:
		ev.Polarity, ev.Points = model.PolaritySupporting, 1
		ev.Text = fmt.Sprintf("%s's anomaly begins in the %s bucket, within one %d-second bucket of the earliest anomaly: too close to order reliably.", label, at, hist.BucketSeconds)
	default:
		ev.Polarity, ev.Points = model.PolarityContradicting, -1
		ev.Text = fmt.Sprintf("%s's anomaly begins at %s, %.0f s after the earliest anomaly in this incident. An origin would be expected to move first.", label, at, lag.Seconds())
	}
	return ev
}

// position scores whether something downstream of the candidate could
// explain the candidate's own symptoms instead.
func (b *builder) position(service string) model.Evidence {
	ev := model.Evidence{Check: "dependency_position", Class: model.ClassInference, Service: service}
	var culprits []string
	for _, k := range b.sortedEdges() {
		if k[0] != service {
			continue
		}
		if d := b.profiles[k[1]]; d != nil && d.OriginAnomalous {
			culprits = append(culprits, k[1])
		} else if e := b.edges[k]; d == nil && e.IncidentUnreachable >= unreachableMin {
			culprits = append(culprits, k[1])
		}
	}
	if len(culprits) == 0 {
		ev.Polarity, ev.Points = model.PolaritySupporting, 2
		ev.Text = fmt.Sprintf("None of the services %s calls shows a change in its own self time or origin errors, so nothing downstream explains %s's change.", service, service)
		return ev
	}
	ev.Polarity, ev.Points = model.PolarityContradicting, -2
	ev.Text = fmt.Sprintf("%s calls %v, which changed in their own right. Part of what is seen at %s may be inherited from them.", service, culprits, service)
	return ev
}

// coverage scores how many impacted requests contain the candidate's
// anomalous span, and specificity how rarely unaffected requests do.
func (b *builder) coverage(label string, marks func(*tree.Trace) bool) []model.Evidence {
	var hitImpacted, hitOther, other int
	var examples []string
	for _, tr := range b.traces {
		hit := marks(tr)
		if b.impacted[tr.ID] {
			if hit {
				hitImpacted++
				if len(examples) < exampleTraceLimit {
					examples = append(examples, tr.ID)
				}
			}
		} else {
			other++
			if hit {
				hitOther++
			}
		}
	}
	nImpacted := len(b.impacted)
	cov := model.Evidence{Check: "coverage", Class: model.ClassCorrelation, Polarity: model.PolarityNeutral, TraceIDs: examples}
	if nImpacted == 0 {
		cov.Text = "No sampled incident request failed or was markedly slower than baseline, so coverage cannot be assessed."
		return []model.Evidence{cov}
	}
	share := float64(hitImpacted) / float64(nImpacted)
	cov.Text = fmt.Sprintf("%d of %d impacted requests (%s) contain %s.", hitImpacted, nImpacted, pct(share), label)
	switch {
	case share >= 0.8:
		cov.Polarity, cov.Points = model.PolaritySupporting, 2
	case share >= 0.4:
		cov.Polarity, cov.Points = model.PolaritySupporting, 1
	case share < 0.2:
		cov.Polarity, cov.Points = model.PolarityContradicting, -1
		cov.Text += " Most impacted requests are not explained by it."
	}
	out := []model.Evidence{cov}
	if other > 0 {
		spec := model.Evidence{Check: "specificity", Class: model.ClassCorrelation}
		otherShare := float64(hitOther) / float64(other)
		spec.Text = fmt.Sprintf("%d of %d unaffected requests (%s) also contain %s.", hitOther, other, pct(otherShare), label)
		switch {
		case otherShare >= 0.5:
			spec.Polarity, spec.Points = model.PolarityContradicting, -1
			spec.Text += " Its presence does not distinguish impacted requests from healthy ones."
		case share >= 0.4:
			spec.Polarity, spec.Points = model.PolaritySupporting, 1
		default:
			spec.Polarity = model.PolarityNeutral
		}
		out = append(out, spec)
	}
	return out
}

func intervention(iv replay.Intervention) json.RawMessage {
	raw, err := json.Marshal(iv)
	if err != nil {
		return nil
	}
	return raw
}

func (b *builder) latencyOrigin(p *ServiceProfile) model.Hypothesis {
	ratio := p.IncidentSelfP95Ms / math.Max(p.BaselineSelfP95Ms, 0.001)
	threshold := p95(p.baselineSelfSamples)
	h := model.Hypothesis{Kind: KindLatencyOrigin, Service: p.Service,
		Title:                 fmt.Sprintf("%s: its own processing became slower", p.Service),
		Statement:             fmt.Sprintf("Hypothesis: latency originates in %s itself. Its self time (time not spent waiting on the services it calls) rose, and requests that pass through it inherit the delay.", p.Service),
		SuggestedIntervention: intervention(replay.Intervention{Type: replay.TypeScaleLatency, Service: p.Service, Mode: replay.ModeBaseline}),
	}
	h.Evidence = append(h.Evidence, model.Evidence{Check: "localisation", Polarity: model.PolaritySupporting, Class: model.ClassObserved,
		Points: tier(ratio, latencyRatioMin, 3, 6), Service: p.Service, Metric: "self_time_p95",
		Text: fmt.Sprintf("%s self-time p95 is %.0f ms in incident traces against %.0f ms in baseline traces (%.1f×; n=%d and %d).",
			p.Service, p.IncidentSelfP95Ms, p.BaselineSelfP95Ms, ratio, p.IncidentRequests, p.BaselineRequests)})
	h.Evidence = append(h.Evidence, b.precedence(p.Service, p.Onset), b.position(p.Service))
	h.Evidence = append(h.Evidence, b.coverage(fmt.Sprintf("a %s span with self time above its baseline p95 (%.0f ms)", p.Service, threshold),
		func(tr *tree.Trace) bool {
			for _, n := range tr.Nodes {
				if n.Span.Service == p.Service && n.IsEntry() && float64(n.ExclusiveNs())/1e6 > threshold {
					return true
				}
			}
			return false
		})...)
	return h
}

func (b *builder) errorOrigin(p *ServiceProfile) model.Hypothesis {
	delta := p.IncidentOriginRate - p.BaselineOriginRate
	h := model.Hypothesis{Kind: KindErrorOrigin, Service: p.Service,
		Title:                 fmt.Sprintf("%s: errors originate here", p.Service),
		Statement:             fmt.Sprintf("Hypothesis: %s is failing requests on its own account. Its spans end in error without any failed downstream call beneath them, and callers inherit those failures.", p.Service),
		SuggestedIntervention: intervention(replay.Intervention{Type: replay.TypeRemoveErrors, Service: p.Service}),
	}
	h.Evidence = append(h.Evidence, model.Evidence{Check: "localisation", Polarity: model.PolaritySupporting, Class: model.ClassObserved,
		Points: tier(delta, errorDeltaMin, 0.10, 0.30), Service: p.Service, Metric: "origin_error_rate",
		Text: fmt.Sprintf("%s of %s requests fail with no failed call beneath them during the incident, against %s in baseline (n=%d and %d).",
			pct(p.IncidentOriginRate), p.Service, pct(p.BaselineOriginRate), p.IncidentRequests, p.BaselineRequests)})
	h.Evidence = append(h.Evidence, b.precedence(p.Service, p.Onset), b.position(p.Service))
	h.Evidence = append(h.Evidence, b.coverage(fmt.Sprintf("a %s span that failed with no failed call beneath it", p.Service),
		func(tr *tree.Trace) bool {
			for _, n := range tr.Nodes {
				if n.Span.Service == p.Service && n.IsEntry() && originError(n) {
					return true
				}
			}
			return false
		})...)
	return h
}

func (b *builder) unavailable(callee string, share, baseShare float64, calls int) model.Hypothesis {
	h := model.Hypothesis{Kind: KindUnavailable, Service: callee,
		Title:                 fmt.Sprintf("%s: not reachable", callee),
		Statement:             fmt.Sprintf("Hypothesis: %s was unavailable. Calls to it fail and no span from %s exists for them, which is what a service that is down, unreachable or refusing connections looks like from its callers.", callee, callee),
		SuggestedIntervention: intervention(replay.Intervention{Type: replay.TypeRemoveErrors, Service: callee}),
	}
	h.Evidence = append(h.Evidence, model.Evidence{Check: "localisation", Polarity: model.PolaritySupporting, Class: model.ClassObserved,
		Points: tier(share, unreachableMin, 0.75, 0.9), Service: callee, Metric: "unreachable_rate",
		Text: fmt.Sprintf("%s of %d calls to %s during the incident failed with no span recorded by %s (baseline %s).",
			pct(share), calls, callee, callee, pct(baseShare))})
	var onset *time.Time
	for _, k := range b.sortedEdges() {
		if k[1] != callee {
			continue
		}
		if t, ok := b.edgeOnsets[k]; ok && (onset == nil || t.Before(*onset)) {
			t := t
			onset = &t
		}
	}
	h.Evidence = append(h.Evidence, b.precedence("Calls to "+callee, onset))
	h.Evidence = append(h.Evidence, model.Evidence{Check: "dependency_position", Polarity: model.PolaritySupporting, Class: model.ClassInference,
		Points: 2, Service: callee,
		Text: fmt.Sprintf("%s recorded nothing for the failed calls, so no downstream dependency of %s can be implicated by the traces. An absent span cannot distinguish a crashed service from a network partition or missing instrumentation.", callee, callee)})
	h.Evidence = append(h.Evidence, b.coverage(fmt.Sprintf("a failed call to %s with no %s span", callee, callee),
		func(tr *tree.Trace) bool {
			for _, n := range tr.Nodes {
				if n.IsClient() && n.IsError() && n.Callee() == callee && n.ServerChild() == nil {
					return true
				}
			}
			return false
		})...)
	return h
}

func (b *builder) retryAmplification(e *EdgeProfile) model.Hypothesis {
	h := model.Hypothesis{Kind: KindRetryAmplify, Service: e.Caller, Peer: e.Callee,
		Title:                 fmt.Sprintf("%s → %s: retries amplified the impact", e.Caller, e.Callee),
		Statement:             fmt.Sprintf("Hypothesis: retries from %s to %s made the incident worse. Each retry adds a full attempt's wait to the request and an extra request to %s. Retries respond to a failure; they are a contributing mechanism rather than an origin.", e.Caller, e.Callee, e.Callee),
		SuggestedIntervention: intervention(replay.Intervention{Type: replay.TypeRetryPolicy, Caller: e.Caller, Callee: e.Callee, MaxAttempts: 1}),
	}
	h.Evidence = append(h.Evidence, model.Evidence{Check: "localisation", Polarity: model.PolaritySupporting, Class: model.ClassObserved,
		Points: tier(e.IncidentRetryRatio, retryRatioMin, 0.3, 0.5), Service: e.Caller, Peer: e.Callee, Metric: "retry_ratio",
		Text: fmt.Sprintf("%s of %d calls from %s to %s during the incident are retries, against %s in baseline. %s of calls ended in a timeout.",
			pct(e.IncidentRetryRatio), e.IncidentCalls, e.Caller, e.Callee, pct(e.BaselineRetryRatio), pct(e.IncidentTimeoutRate))})
	h.Evidence = append(h.Evidence, b.precedence(e.Caller+" → "+e.Callee, e.Onset))
	h.Evidence = append(h.Evidence, model.Evidence{Check: "dependency_position", Polarity: model.PolarityNeutral, Class: model.ClassInference,
		Service: e.Caller, Peer: e.Callee,
		Text: fmt.Sprintf("Retries only happen after an attempt fails, so something else made calls to %s fail first. This hypothesis explains amplification, not onset.", e.Callee)})
	h.Evidence = append(h.Evidence, b.coverage(fmt.Sprintf("a retried call from %s to %s", e.Caller, e.Callee),
		func(tr *tree.Trace) bool {
			for _, n := range tr.Nodes {
				if n.Span.Service != e.Caller {
					continue
				}
				for _, u := range n.Units() {
					if u.Callee == e.Callee && len(u.Attempts) > 1 {
						return true
					}
				}
			}
			return false
		})...)
	return h
}

func (b *builder) trafficSurge(p *ServiceProfile, ratio float64) model.Hypothesis {
	h := model.Hypothesis{Kind: KindTrafficSurge, Service: p.Service,
		Title:     fmt.Sprintf("%s: request volume surged", p.Service),
		Statement: fmt.Sprintf("Hypothesis: a surge in requests entering through %s loaded the system. Replay cannot test this: the replay model has no relationship between load and latency.", p.Service),
	}
	h.Evidence = append(h.Evidence, model.Evidence{Check: "localisation", Polarity: model.PolaritySupporting, Class: model.ClassObserved,
		Points: tier(ratio, trafficRatioMin, 3, 5), Service: p.Service, Metric: "request_rate",
		Text: fmt.Sprintf("%s received %.1f req/s during the incident against %.1f req/s in baseline (%.1f×), counted from rollups of all requests.",
			p.Service, p.IncidentRatePerSec, p.BaselineRatePerSec, ratio)})
	h.Evidence = append(h.Evidence, b.precedence(p.Service, p.Onset))
	h.Evidence = append(h.Evidence, model.Evidence{Check: "dependency_position", Polarity: model.PolaritySupporting, Class: model.ClassInference,
		Points: 2, Service: p.Service,
		Text: fmt.Sprintf("%s is an entry point: no instrumented service calls it, so the extra requests arrived from outside the traced system.", p.Service)})
	h.Evidence = append(h.Evidence, model.Evidence{Check: "coverage", Polarity: model.PolarityNeutral, Class: model.ClassCorrelation,
		Text: "Coverage is not assessed: a surge affects every request alike, so no span distinguishes impacted requests."})
	return h
}

// Methodology documents how hypotheses are scored. It is returned with every
// investigation and included in reports.
func Methodology() []string {
	return []string{
		fmt.Sprintf("Each hypothesis receives an evidence score: an integer sum of points from five checks, with a maximum of %d. It is a ranking aid. It is not a probability and does not measure how likely the hypothesis is to be true.", MaxScore),
		fmt.Sprintf("Localisation (1 to 3, observed): how much the candidate's own signal changed between baseline and incident traces. For latency this is self-time p95 (≥%.1f×, ≥3×, ≥6×); for errors, the rise in origin-error rate (≥3, ≥10, ≥30 points).", latencyRatioMin),
		fmt.Sprintf("Temporal precedence (−1 to +2, correlation): +2 if the candidate's anomaly starts in the earliest anomalous %d-second bucket, +1 if within one bucket of it, −1 if later. Moving first is consistent with being the origin but does not establish it.", hist.BucketSeconds),
		"Dependency position (−2 to +2, inference): +2 if nothing the candidate calls changed in its own right, −2 if something did, because the candidate's symptoms may then be inherited.",
		"Coverage (−1 to +2, correlation): the share of impacted requests (failed, or markedly slower than the baseline p95 for the same operation) that contain the candidate's anomalous span: +2 at 80%, +1 at 40%, −1 below 20%.",
		"Specificity (−1 to +1, correlation): −1 if at least half of unaffected requests contain the same anomalous span, +1 if they do not and coverage is at least 40%.",
		"Self time is a span's duration minus the time covered by its child spans. A span 'originates' an error when it has error status and no failed call beneath it.",
		"Failure-lab experiments that overlap the incident are listed as known faults. They never contribute to a score; they let you check the ranking against ground truth.",
	}
}
