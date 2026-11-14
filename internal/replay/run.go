package replay

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/tree"
)

// Stat summarises a set of requests.
type Stat struct {
	Count     int     `json:"count"`
	Errors    int     `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	MeanMs    float64 `json:"mean_ms"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
	P99Ms     float64 `json:"p99_ms"`
}

// Comparison pairs the recorded and simulated statistics for one scope.
type Comparison struct {
	Original       Stat `json:"original"`
	Counterfactual Stat `json:"counterfactual"`
}

// Summary compares end-to-end request outcomes.
type Summary struct {
	Comparison
	OriginalRetries         int     `json:"original_retries"`
	CounterfactualRetries   int     `json:"counterfactual_retries"`
	ObjectiveMs             float64 `json:"objective_ms"`
	OriginalBreachRate      float64 `json:"original_breach_rate"`
	CounterfactualBreachRt  float64 `json:"counterfactual_breach_rate"`
	OriginalBreaches        int     `json:"original_breaches"`
	CounterfactualBreaches  int     `json:"counterfactual_breaches"`
	SamplingFractionPercent float64 `json:"sampling_fraction_percent"`
}

// ScopedComparison is a Comparison for a named operation or service.
type ScopedComparison struct {
	Name string `json:"name"`
	Comparison
}

// EdgeComparison compares call volume and outcomes on one edge.
type EdgeComparison struct {
	Caller                string  `json:"caller"`
	Callee                string  `json:"callee"`
	OriginalCalls         int     `json:"original_calls"`
	CounterfactualCalls   int     `json:"counterfactual_calls"`
	OriginalRetries       int     `json:"original_retries"`
	CounterfactualRetries int     `json:"counterfactual_retries"`
	OriginalErrorRate     float64 `json:"original_error_rate"`
	CounterfactualErrRate float64 `json:"counterfactual_error_rate"`
}

// SeriesPoint is one time bucket of recorded versus simulated outcomes.
type SeriesPoint struct {
	Bucket                  time.Time `json:"bucket"`
	Requests                int       `json:"requests"`
	OriginalErrorRate       float64   `json:"original_error_rate"`
	CounterfactualErrorRate float64   `json:"counterfactual_error_rate"`
	OriginalP95Ms           float64   `json:"original_p95_ms"`
	CounterfactualP95Ms     float64   `json:"counterfactual_p95_ms"`
}

// TraceDiff is the recorded versus simulated outcome of one trace.
type TraceDiff struct {
	TraceID               string    `json:"trace_id"`
	Start                 time.Time `json:"start_time"`
	Operation             string    `json:"operation"`
	OriginalMs            float64   `json:"original_ms"`
	CounterfactualMs      float64   `json:"counterfactual_ms"`
	OriginalError         bool      `json:"original_error"`
	CounterfactualError   bool      `json:"counterfactual_error"`
	OriginalRetries       int       `json:"original_retries"`
	CounterfactualRetries int       `json:"counterfactual_retries"`
	Changed               bool      `json:"changed"`
	Imputed               bool      `json:"imputed"`
	Synthesized           bool      `json:"synthesized"`
}

// Finding states whether one observable outcome changed under the scenario.
type Finding struct {
	Scope          string  `json:"scope"`
	Metric         string  `json:"metric"`
	Unit           string  `json:"unit"`
	Original       float64 `json:"original"`
	Counterfactual float64 `json:"counterfactual"`
	Delta          float64 `json:"delta"`
	Changed        bool    `json:"changed"`
}

// DatasetInfo identifies the dataset a result was computed from.
type DatasetInfo struct {
	ID       int64  `json:"id"`
	Version  int    `json:"version"`
	Checksum string `json:"checksum"`
}

// Result is the full output of a replay.
type Result struct {
	EngineVersion string             `json:"engine_version"`
	EvidenceClass string             `json:"evidence_class"`
	Scenario      Scenario           `json:"scenario"`
	Dataset       DatasetInfo        `json:"dataset"`
	Summary       Summary            `json:"summary"`
	Operations    []ScopedComparison `json:"operations"`
	Services      []ScopedComparison `json:"services"`
	Edges         []EdgeComparison   `json:"edges"`
	Series        []SeriesPoint      `json:"series"`
	Findings      []Finding          `json:"findings"`
	Traces        []TraceDiff        `json:"traces"`
	TracesOmitted int                `json:"traces_omitted"`
	Diagnostics   Diagnostics        `json:"diagnostics"`
	Assumptions   []string           `json:"assumptions"`
	Limitations   []string           `json:"limitations"`
	Unsupported   []string           `json:"unsupported_conclusions"`
}

// maxTraceDiffs caps the per-trace rows returned with a result.
const maxTraceDiffs = 500

// Tolerances below which an outcome is reported as unchanged.
const (
	ratioTolerance    = 0.005 // half a percentage point
	latencyRelTol     = 0.02
	latencyAbsTolMs   = 1.0
	identityTolerance = 1000 // ns
)

type sample struct {
	durNs int64
	err   bool
}

func statOf(samples []sample) Stat {
	st := Stat{Count: len(samples)}
	if len(samples) == 0 {
		return st
	}
	ms := make([]float64, len(samples))
	var sum float64
	for i, s := range samples {
		ms[i] = float64(s.durNs) / 1e6
		sum += ms[i]
		if s.err {
			st.Errors++
		}
	}
	sort.Float64s(ms)
	st.ErrorRate = float64(st.Errors) / float64(len(samples))
	st.MeanMs = round3(sum / float64(len(samples)))
	st.P50Ms = round3(hist.QuantileSorted(ms, 0.50))
	st.P95Ms = round3(hist.QuantileSorted(ms, 0.95))
	st.P99Ms = round3(hist.QuantileSorted(ms, 0.99))
	return st
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

type edgeCount struct{ calls, retries, errors int }

// collector accumulates per-service and per-edge statistics.
type collector struct {
	services map[string][]sample
	edges    map[edgeKey]*edgeCount
}

func newCollector() *collector {
	return &collector{services: map[string][]sample{}, edges: map[edgeKey]*edgeCount{}}
}

func (c *collector) edge(k edgeKey) *edgeCount {
	e := c.edges[k]
	if e == nil {
		e = &edgeCount{}
		c.edges[k] = e
	}
	return e
}

// observed walks a recorded subtree.
func (c *collector) observed(n *tree.Node) {
	if n.IsEntry() {
		c.services[n.Span.Service] = append(c.services[n.Span.Service], sample{n.DurationNs(), n.IsError()})
	}
	for _, u := range n.Units() {
		if u.Callee == "" {
			continue
		}
		e := c.edge(edgeKey{n.Span.Service, u.Callee})
		for i, a := range u.Attempts {
			e.calls++
			if i > 0 {
				e.retries++
			}
			if a.IsError() {
				e.errors++
			}
		}
	}
	for _, ch := range n.Children {
		c.observed(ch)
	}
}

// simulated walks an outcome tree and returns the retries it contains.
func (c *collector) simulated(o *outcome) (retries int) {
	if o.entry {
		c.services[o.service] = append(c.services[o.service], sample{o.durNs, o.err})
	}
	if o.client && o.callee != "" {
		e := c.edge(edgeKey{o.service, o.callee})
		e.calls++
		if o.retry {
			e.retries++
		}
		if o.err {
			e.errors++
		}
	}
	if o.client && o.retry {
		retries++
	}
	for _, ch := range o.children {
		retries += c.simulated(ch)
	}
	return retries
}

// Run executes a scenario against a dataset. The dataset is only read. The
// result is a pure function of (dataset, scenario): the same inputs and seed
// always produce an identical result.
func Run(ds *model.Dataset, sc Scenario) (*Result, error) {
	traces := tree.Build(ds.Spans)
	if err := validate(sc, topologyOf(traces)); err != nil {
		return nil, err
	}
	if sc.LatencyObjectiveMs == 0 {
		sc.LatencyObjectiveMs = DefaultObjectiveMs
	}
	m := buildModel(traces)

	diag := Diagnostics{TracesInDataset: len(traces), LatencyFactors: map[string]float64{},
		InferredTimeoutsMs: map[string]float64{}, ObservedMaxAttempts: map[string]int{}}
	for k, em := range m.edges {
		label := k.caller + "→" + k.callee
		if em.timeoutNs > 0 {
			diag.InferredTimeoutsMs[label] = round3(float64(em.timeoutNs) / 1e6)
		}
		if em.maxAttempts > 1 {
			diag.ObservedMaxAttempts[label] = em.maxAttempts
		}
	}

	s := newSim(m, sc.Seed, &diag)
	var problems []string
	for _, iv := range sc.Interventions {
		switch iv.Type {
		case TypeScaleLatency:
			f := iv.Factor
			if iv.Mode == ModeBaseline {
				base, inc := m.exclBaseline[iv.Service], m.exclIncident[iv.Service]
				if len(base) == 0 || len(inc) == 0 {
					problems = append(problems, fmt.Sprintf(
						"scale_latency(baseline): no baseline or no incident executions of %q in the dataset", iv.Service))
					continue
				}
				incMedian := hist.Median(inc)
				f = 1
				if incMedian > 0 {
					f = math.Min(hist.Median(base)/incMedian, 1)
				}
			}
			s.latency[iv.Service] = f
			diag.LatencyFactors[iv.Service] = math.Round(f*10000) / 10000
		case TypeRemoveErrors:
			s.removeErrors[iv.Service] = true
		case TypeRetryPolicy:
			s.retryPolicy[edgeKey{iv.Caller, iv.Callee}] = iv.MaxAttempts
		case TypeSetTimeout:
			s.timeoutOver[edgeKey{iv.Caller, iv.Callee}] = int64(iv.TimeoutMs * 1e6)
		}
	}
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}

	// Self-check: with no intervention the model must reproduce the recording.
	var idDiag Diagnostics
	identity := newSim(m, sc.Seed, &idDiag)

	origCol, cfCol := newCollector(), newCollector()
	var origAll, cfAll []sample
	byOp := map[string]*[2][]sample{}
	type bucketAgg struct{ orig, cf []sample }
	buckets := map[int64]*bucketAgg{}
	res := &Result{EngineVersion: EngineVersion, EvidenceClass: model.ClassSimulated, Scenario: sc,
		Dataset: DatasetInfo{ID: ds.ID, Version: ds.Version, Checksum: ds.Checksum}}
	objectiveNs := int64(sc.LatencyObjectiveMs * 1e6)

	for _, tr := range traces {
		if tr.Phase == model.PhaseBaseline {
			diag.BaselineTraces++
			continue
		}
		diag.IncidentTraces++
		if !tr.Complete {
			diag.TracesSkipped++
			continue
		}
		diag.TracesReplayed++
		root := tr.Root

		io := identity.node(root)
		diag.IdentityChecked++
		if dev := abs64(io.durNs - root.DurationNs()); dev > identityTolerance || io.err != root.IsError() {
			diag.IdentityMismatches++
			diag.IdentityMaxDeviationMs = math.Max(diag.IdentityMaxDeviationMs, round3(float64(dev)/1e6))
		}

		o := s.node(root)
		origCol.observed(root)
		cfRetries := cfCol.simulated(o)
		origRetries := root.Retries()
		orig, cf := sample{root.DurationNs(), root.IsError()}, sample{o.durNs, o.err}
		origAll, cfAll = append(origAll, orig), append(cfAll, cf)
		op := root.Span.Service + " · " + root.Span.Name
		if byOp[op] == nil {
			byOp[op] = &[2][]sample{}
		}
		byOp[op][0], byOp[op][1] = append(byOp[op][0], orig), append(byOp[op][1], cf)
		bk := hist.TruncateBucket(root.Span.Start).Unix()
		if buckets[bk] == nil {
			buckets[bk] = &bucketAgg{}
		}
		buckets[bk].orig, buckets[bk].cf = append(buckets[bk].orig, orig), append(buckets[bk].cf, cf)

		res.Summary.OriginalRetries += origRetries
		res.Summary.CounterfactualRetries += cfRetries
		if orig.err || orig.durNs > objectiveNs {
			res.Summary.OriginalBreaches++
		}
		if cf.err || cf.durNs > objectiveNs {
			res.Summary.CounterfactualBreaches++
		}
		changed := abs64(cf.durNs-orig.durNs) > identityTolerance || cf.err != orig.err || cfRetries != origRetries
		if changed {
			diag.TracesChanged++
		}
		res.Traces = append(res.Traces, TraceDiff{
			TraceID: tr.ID, Start: root.Span.Start, Operation: op,
			OriginalMs: round3(float64(orig.durNs) / 1e6), CounterfactualMs: round3(float64(cf.durNs) / 1e6),
			OriginalError: orig.err, CounterfactualError: cf.err,
			OriginalRetries: origRetries, CounterfactualRetries: cfRetries, Changed: changed,
			Imputed: s.traceImputed[tr.ID], Synthesized: s.traceSynth[tr.ID],
		})
	}

	res.Summary.Comparison = Comparison{Original: statOf(origAll), Counterfactual: statOf(cfAll)}
	res.Summary.ObjectiveMs = sc.LatencyObjectiveMs
	if n := len(origAll); n > 0 {
		res.Summary.OriginalBreachRate = float64(res.Summary.OriginalBreaches) / float64(n)
		res.Summary.CounterfactualBreachRt = float64(res.Summary.CounterfactualBreaches) / float64(n)
	}
	if ds.Meta.IncidentTracesAvailable > 0 {
		res.Summary.SamplingFractionPercent = round3(100 * float64(ds.Meta.IncidentTracesSampled) / float64(ds.Meta.IncidentTracesAvailable))
	}

	for op, pair := range byOp {
		res.Operations = append(res.Operations, ScopedComparison{Name: op,
			Comparison: Comparison{Original: statOf(pair[0]), Counterfactual: statOf(pair[1])}})
	}
	sort.Slice(res.Operations, func(i, j int) bool { return res.Operations[i].Name < res.Operations[j].Name })

	svcNames := map[string]bool{}
	for name := range origCol.services {
		svcNames[name] = true
	}
	for name := range cfCol.services {
		svcNames[name] = true
	}
	for name := range svcNames {
		res.Services = append(res.Services, ScopedComparison{Name: name,
			Comparison: Comparison{Original: statOf(origCol.services[name]), Counterfactual: statOf(cfCol.services[name])}})
	}
	sort.Slice(res.Services, func(i, j int) bool { return res.Services[i].Name < res.Services[j].Name })

	edgeKeys := map[edgeKey]bool{}
	for k := range origCol.edges {
		edgeKeys[k] = true
	}
	for k := range cfCol.edges {
		edgeKeys[k] = true
	}
	for k := range edgeKeys {
		oe, ce := origCol.edge(k), cfCol.edge(k)
		ec := EdgeComparison{Caller: k.caller, Callee: k.callee, OriginalCalls: oe.calls, CounterfactualCalls: ce.calls,
			OriginalRetries: oe.retries, CounterfactualRetries: ce.retries}
		if oe.calls > 0 {
			ec.OriginalErrorRate = float64(oe.errors) / float64(oe.calls)
		}
		if ce.calls > 0 {
			ec.CounterfactualErrRate = float64(ce.errors) / float64(ce.calls)
		}
		res.Edges = append(res.Edges, ec)
	}
	sort.Slice(res.Edges, func(i, j int) bool {
		if res.Edges[i].Caller != res.Edges[j].Caller {
			return res.Edges[i].Caller < res.Edges[j].Caller
		}
		return res.Edges[i].Callee < res.Edges[j].Callee
	})

	bucketKeys := make([]int64, 0, len(buckets))
	for k := range buckets {
		bucketKeys = append(bucketKeys, k)
	}
	sort.Slice(bucketKeys, func(i, j int) bool { return bucketKeys[i] < bucketKeys[j] })
	for _, k := range bucketKeys {
		o, c := statOf(buckets[k].orig), statOf(buckets[k].cf)
		res.Series = append(res.Series, SeriesPoint{Bucket: time.Unix(k, 0).UTC(), Requests: o.Count,
			OriginalErrorRate: o.ErrorRate, CounterfactualErrorRate: c.ErrorRate,
			OriginalP95Ms: o.P95Ms, CounterfactualP95Ms: c.P95Ms})
	}

	sort.SliceStable(res.Traces, func(i, j int) bool {
		di := math.Abs(res.Traces[i].CounterfactualMs - res.Traces[i].OriginalMs)
		dj := math.Abs(res.Traces[j].CounterfactualMs - res.Traces[j].OriginalMs)
		if di != dj {
			return di > dj
		}
		return res.Traces[i].TraceID < res.Traces[j].TraceID
	})
	if len(res.Traces) > maxTraceDiffs {
		res.TracesOmitted = len(res.Traces) - maxTraceDiffs
		res.Traces = res.Traces[:maxTraceDiffs]
	}

	res.Findings = findings(res)
	res.Diagnostics = diag
	res.Assumptions = assumptions()
	res.Limitations = limitations(ds, res)
	res.Unsupported = unsupported(sc)
	return res, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func ratioFinding(scope, metric string, orig, cf float64) Finding {
	return Finding{Scope: scope, Metric: metric, Unit: "ratio", Original: orig, Counterfactual: cf, Delta: cf - orig,
		Changed: math.Abs(cf-orig) > ratioTolerance}
}

func latencyFinding(scope, metric string, orig, cf float64) Finding {
	d := math.Abs(cf - orig)
	return Finding{Scope: scope, Metric: metric, Unit: "ms", Original: orig, Counterfactual: cf, Delta: round3(cf - orig),
		Changed: d > latencyAbsTolMs && d > latencyRelTol*math.Max(orig, cf)}
}

func countFinding(scope, metric string, orig, cf int) Finding {
	return Finding{Scope: scope, Metric: metric, Unit: "count", Original: float64(orig), Counterfactual: float64(cf),
		Delta: float64(cf - orig), Changed: orig != cf}
}

// findings flattens the comparisons into explicit changed / unchanged
// statements about each observable outcome.
func findings(r *Result) []Finding {
	s := r.Summary
	out := []Finding{
		ratioFinding("All requests", "Error rate", s.Original.ErrorRate, s.Counterfactual.ErrorRate),
		latencyFinding("All requests", "p50 latency", s.Original.P50Ms, s.Counterfactual.P50Ms),
		latencyFinding("All requests", "p95 latency", s.Original.P95Ms, s.Counterfactual.P95Ms),
		latencyFinding("All requests", "p99 latency", s.Original.P99Ms, s.Counterfactual.P99Ms),
		countFinding("All requests", "Retries", s.OriginalRetries, s.CounterfactualRetries),
		ratioFinding("All requests", fmt.Sprintf("Objective breaches (error or > %.0f ms)", s.ObjectiveMs),
			s.OriginalBreachRate, s.CounterfactualBreachRt),
	}
	for _, op := range r.Operations {
		out = append(out,
			ratioFinding("Operation "+op.Name, "Error rate", op.Original.ErrorRate, op.Counterfactual.ErrorRate),
			latencyFinding("Operation "+op.Name, "p95 latency", op.Original.P95Ms, op.Counterfactual.P95Ms))
	}
	for _, sv := range r.Services {
		out = append(out,
			ratioFinding("Service "+sv.Name, "Error rate", sv.Original.ErrorRate, sv.Counterfactual.ErrorRate),
			latencyFinding("Service "+sv.Name, "p95 latency", sv.Original.P95Ms, sv.Counterfactual.P95Ms),
			countFinding("Service "+sv.Name, "Requests handled", sv.Original.Count, sv.Counterfactual.Count))
	}
	return out
}

func assumptions() []string {
	return []string{
		"Recorded span timings and statuses are taken as ground truth. The replay recomputes them arithmetically; it does not re-execute any application code.",
		"Sequencing is inferred from the recording: each step is assumed to follow the latest-finishing earlier step that had ended when it started, keeping the observed gap between them.",
		"scale_latency multiplies only the target service's self time (time not spent waiting on its own downstream calls). Other services change only through the calls they make.",
		"A client timeout is inferred per attempt from the recording: an attempt recorded as timed out keeps its recorded deadline; other attempts use the median deadline observed on that edge, and only if they became slower than recorded.",
		"Retries stop at the first successful attempt. Attempts that the scenario makes unnecessary are removed; attempts beyond the recording are synthesised by drawing a recorded callee execution from the same edge with the seeded generator.",
		"Whether a newly failing call fails its caller follows the share of recorded failures on that caller/callee pair that propagated (at least half ⇒ propagates). With no recorded failures it is assumed to propagate.",
		"When a span that stopped at a failed call would now continue, its remaining duration is drawn (seeded) from successful recorded executions of the same operation. The spans of that remaining work are not individually simulated.",
		"remove_errors replaces a failed execution with a seeded draw from successful baseline executions of the same operation or edge.",
		"There is no load feedback: a service's latency does not depend on how many requests it receives in the replay.",
	}
}

func limitations(ds *model.Dataset, r *Result) []string {
	d := r.Diagnostics
	out := []string{
		fmt.Sprintf("The replay covers %d sampled incident traces (%.1f%% of the %d eligible traces in the incident window). Percentiles from a sample of this size carry sampling error.",
			d.TracesReplayed, r.Summary.SamplingFractionPercent, ds.Meta.IncidentTracesAvailable),
		"Only behaviour visible in trace spans is modelled. Saturation, queueing, connection pools, caches, autoscaling and anything else not recorded as span timing are absent.",
	}
	if d.TracesSkipped > 0 {
		out = append(out, fmt.Sprintf("%d incident traces were incomplete (missing root or parent spans) and were excluded.", d.TracesSkipped))
	}
	if d.CensoredAttempts > 0 {
		out = append(out, fmt.Sprintf("%d timed-out attempts have a callee span shorter than the client's deadline: the callee's true duration is censored and treated as a lower bound, which makes latency reductions look more effective than they may be.", d.CensoredAttempts))
	}
	if d.ImputedContinuance+d.ImputedDurations > 0 {
		out = append(out, fmt.Sprintf("%d durations were imputed from reference distributions rather than recorded for the request in question.", d.ImputedContinuance+d.ImputedDurations))
	}
	if d.SynthAttempts > 0 {
		out = append(out, fmt.Sprintf("%d call attempts were synthesised; they did not occur in the recording.", d.SynthAttempts))
	}
	if d.Unimputable > 0 {
		out = append(out, fmt.Sprintf("%d outcomes could not be adjusted because the dataset holds no reference executions to draw from; they were left as recorded.", d.Unimputable))
	}
	if d.AssumedPropagation > 0 {
		out = append(out, fmt.Sprintf("%d new failures were assumed to propagate to their caller without recorded evidence for that caller/callee pair.", d.AssumedPropagation))
	}
	if d.IdentityMismatches > 0 {
		out = append(out, fmt.Sprintf("Self-check: the model failed to reproduce %d of %d traces with no intervention (largest deviation %.3f ms), typically because of overlapping retries or clock skew. Treat small differences with caution.",
			d.IdentityMismatches, d.IdentityChecked, d.IdentityMaxDeviationMs))
	}
	return out
}

func unsupported(sc Scenario) []string {
	out := []string{
		"This result does not prove what caused the incident. It shows how the recorded traces change under the stated model and interventions.",
		"It does not predict how the real system would behave with this change deployed.",
		"It says nothing about requests, endpoints or time periods outside the sampled incident traces.",
	}
	for _, iv := range sc.Interventions {
		switch iv.Type {
		case TypeRetryPolicy:
			out = append(out, fmt.Sprintf("Changing the retry policy on %s→%s would also change the load on %s; any resulting change in its latency is not modelled.", iv.Caller, iv.Callee, iv.Callee))
		case TypeScaleLatency:
			out = append(out, fmt.Sprintf("That %s could actually have been this fast during the incident is assumed, not shown.", iv.Service))
		}
	}
	return out
}
