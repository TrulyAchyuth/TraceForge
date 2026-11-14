package replay

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/tree"
)

// edgeModel is what the dataset reveals about one caller→callee edge.
type edgeModel struct {
	// timeoutNs is the median duration of attempts that ended in a timeout;
	// zero when no timeout was observed on the edge.
	timeoutNs int64
	// overheadNs is the median of (client duration − server duration) over
	// successful paired calls: network plus client-side cost.
	overheadNs int64
	// maxAttempts is the largest number of attempts seen in one retry group.
	maxAttempts int
	// backoffNs is the median gap between consecutive attempts.
	backoffNs int64
	// baselineOK holds durations of successful client calls in the baseline
	// phase, ascending. It is the pool for imputing a restored call.
	baselineOK []int64
	// failed counts retry groups whose final attempt failed; propagated
	// counts those where the calling span also failed.
	failed, propagated int
	// incidentServers are the server spans that handled calls on this edge
	// during the incident phase: the pool for synthesised attempts.
	incidentServers []*tree.Node

	timeouts, overheads, gaps []int64
}

type opKey struct{ service, name string }

// opModel is what the dataset reveals about one operation (service + span name).
type opModel struct {
	// successRemainder[callee] holds, for executions that succeeded, the time
	// between the end of the call group to callee and the end of the span.
	successRemainder map[string][]int64
	// failTail[callee] is the same interval for executions where the call
	// group failed and the span failed with it.
	failTail map[string][]int64
	// baselineOK holds durations of successful baseline-phase executions.
	baselineOK []int64
}

type simModel struct {
	edges map[edgeKey]*edgeModel
	ops   map[opKey]*opModel
	// exclusive self-time samples of entry spans per service and phase (ns).
	exclBaseline, exclIncident map[string][]float64
}

func (m *simModel) edge(k edgeKey) *edgeModel {
	e := m.edges[k]
	if e == nil {
		e = &edgeModel{}
		m.edges[k] = e
	}
	return e
}

func (m *simModel) op(k opKey) *opModel {
	o := m.ops[k]
	if o == nil {
		o = &opModel{successRemainder: map[string][]int64{}, failTail: map[string][]int64{}}
		m.ops[k] = o
	}
	return o
}

func medianInt(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// buildModel derives the edge and operation models from every complete trace
// in the dataset (both phases). Iteration order is fixed by tree.Build.
func buildModel(traces []*tree.Trace) *simModel {
	m := &simModel{edges: map[edgeKey]*edgeModel{}, ops: map[opKey]*opModel{},
		exclBaseline: map[string][]float64{}, exclIncident: map[string][]float64{}}
	for _, tr := range traces {
		if !tr.Complete {
			continue
		}
		baseline := tr.Phase == model.PhaseBaseline
		for _, n := range tr.Nodes {
			om := m.op(opKey{n.Span.Service, n.Span.Name})
			if baseline && !n.IsError() {
				om.baselineOK = append(om.baselineOK, n.DurationNs())
			}
			if n.IsEntry() {
				if baseline {
					m.exclBaseline[n.Span.Service] = append(m.exclBaseline[n.Span.Service], float64(n.ExclusiveNs()))
				} else {
					m.exclIncident[n.Span.Service] = append(m.exclIncident[n.Span.Service], float64(n.ExclusiveNs()))
				}
			}
			for _, u := range n.Units() {
				if u.Callee == "" {
					continue
				}
				em := m.edge(edgeKey{n.Span.Service, u.Callee})
				em.maxAttempts = max(em.maxAttempts, len(u.Attempts))
				for i, a := range u.Attempts {
					if i > 0 {
						em.gaps = append(em.gaps, max(a.StartNs-u.Attempts[i-1].EndNs, 0))
					}
					s := a.ServerChild()
					switch {
					case a.IsTimeout():
						em.timeouts = append(em.timeouts, a.DurationNs())
					case s != nil && !a.IsError():
						em.overheads = append(em.overheads, max(a.DurationNs()-s.DurationNs(), 0))
					}
					if baseline && !a.IsError() {
						em.baselineOK = append(em.baselineOK, a.DurationNs())
					}
					if !baseline && s != nil {
						em.incidentServers = append(em.incidentServers, s)
					}
				}
				switch {
				case u.Failed():
					em.failed++
					if n.IsError() {
						em.propagated++
						om.failTail[u.Callee] = append(om.failTail[u.Callee], max(n.EndNs-u.EndNs(), 0))
					}
				case !n.IsError() && u.EndNs() <= n.EndNs:
					om.successRemainder[u.Callee] = append(om.successRemainder[u.Callee], n.EndNs-u.EndNs())
				}
			}
		}
	}
	for _, em := range m.edges {
		em.timeoutNs = medianInt(em.timeouts)
		em.overheadNs = medianInt(em.overheads)
		em.backoffNs = medianInt(em.gaps)
		sort.Slice(em.baselineOK, func(i, j int) bool { return em.baselineOK[i] < em.baselineOK[j] })
	}
	for _, om := range m.ops {
		sort.Slice(om.baselineOK, func(i, j int) bool { return om.baselineOK[i] < om.baselineOK[j] })
		for _, pool := range []map[string][]int64{om.successRemainder, om.failTail} {
			for k := range pool {
				v := pool[k]
				sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
			}
		}
	}
	return m
}

// outcome is the simulated result for one executed span.
type outcome struct {
	node    *tree.Node // recorded span this outcome derives from
	service string
	name    string
	entry   bool
	client  bool
	callee  string
	retry   bool // a client attempt after the first in its retry group
	durNs   int64
	err     bool
	// synthesized marks an attempt that was not in the recording.
	synthesized bool
	// imputed marks a duration drawn from a reference distribution.
	imputed  bool
	children []*outcome
}

// Diagnostics reports how much of a replay relied on each modelling device.
type Diagnostics struct {
	TracesInDataset    int `json:"traces_in_dataset"`
	IncidentTraces     int `json:"incident_traces"`
	BaselineTraces     int `json:"baseline_traces"`
	TracesReplayed     int `json:"traces_replayed"`
	TracesSkipped      int `json:"traces_skipped_incomplete"`
	TracesChanged      int `json:"traces_changed"`
	CensoredAttempts   int `json:"censored_attempts"`
	ImputedDurations   int `json:"imputed_durations"`
	ImputedContinuance int `json:"imputed_continuations"`
	SynthAttempts      int `json:"synthesized_attempts"`
	Unimputable        int `json:"unimputable"`
	AssumedPropagation int `json:"assumed_propagations"`
	// Identity* report the self-check: replaying with no intervention must
	// reproduce every recorded trace exactly.
	IdentityChecked        int                `json:"identity_traces_checked"`
	IdentityMismatches     int                `json:"identity_mismatches"`
	IdentityMaxDeviationMs float64            `json:"identity_max_deviation_ms"`
	LatencyFactors         map[string]float64 `json:"latency_factors,omitempty"`
	InferredTimeoutsMs     map[string]float64 `json:"inferred_timeouts_ms,omitempty"`
	ObservedMaxAttempts    map[string]int     `json:"observed_max_attempts,omitempty"`
}

// sim holds the state of one simulation pass over a dataset.
type sim struct {
	m            *simModel
	seed         uint64
	latency      map[string]float64
	removeErrors map[string]bool
	retryPolicy  map[edgeKey]int
	timeoutOver  map[edgeKey]int64
	memo         map[*tree.Node]*outcome
	censoredSeen map[*tree.Node]bool
	diag         *Diagnostics
	// traceSynth and traceImputed mark traces whose result relied on
	// synthesised attempts or imputed durations.
	traceSynth   map[string]bool
	traceImputed map[string]bool
}

func newSim(m *simModel, seed int64, diag *Diagnostics) *sim {
	return &sim{m: m, seed: uint64(seed), latency: map[string]float64{}, removeErrors: map[string]bool{},
		retryPolicy: map[edgeKey]int{}, timeoutOver: map[edgeKey]int64{}, memo: map[*tree.Node]*outcome{},
		censoredSeen: map[*tree.Node]bool{}, diag: diag, traceSynth: map[string]bool{}, traceImputed: map[string]bool{}}
}

// rng returns a generator that depends only on the scenario seed and the
// identity of the decision being made, never on evaluation order.
func (s *sim) rng(n *tree.Node, purpose string, index int) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(n.Span.TraceID))
	h.Write([]byte{0})
	h.Write([]byte(n.Span.SpanID))
	h.Write([]byte{0})
	h.Write([]byte(purpose))
	h.Write([]byte{byte(index), byte(index >> 8)})
	return rand.New(rand.NewPCG(s.seed, h.Sum64()))
}

func scaleGap(gapNs int64, factor float64) int64 {
	if gapNs <= 0 || factor == 1 {
		return gapNs
	}
	return int64(math.Round(float64(gapNs) * factor))
}

func (s *sim) factor(service string) float64 {
	if f, ok := s.latency[service]; ok {
		return f
	}
	return 1
}

// node simulates a recorded span and everything beneath it. Results are
// memoised: a span has one counterfactual outcome per simulation pass.
func (s *sim) node(n *tree.Node) *outcome {
	if o, ok := s.memo[n]; ok {
		return o
	}
	var o *outcome
	if n.IsClient() {
		g := s.group(tree.Unit{Attempts: []*tree.Node{n}, Callee: n.Callee()}, n)
		o = g.attempts[0]
	} else {
		o = s.span(n)
	}
	s.memo[n] = o
	return o
}

// unitSim is one unit of a parent's work with observed and simulated timing.
type unitSim struct {
	unit           tree.Unit
	obsStart       int64
	obsEnd         int64
	cfStart, cfEnd int64 // relative to the parent's start
	err            bool
	outcomes       []*outcome
}

// span simulates a non-client span: it re-times its units, then derives the
// span's own duration and status.
func (s *sim) span(n *tree.Node) *outcome {
	o := &outcome{node: n, service: n.Span.Service, name: n.Span.Name, entry: n.IsEntry()}
	f := s.factor(n.Span.Service)
	units := n.Units()

	var done []*unitSim
	var newFailure *unitSim
	observedFailed := 0
	stillFailing := false
	var lastObservedFailure *unitSim
	for _, u := range units {
		if newFailure != nil && u.StartNs() >= newFailure.obsEnd {
			continue // never started: the span failed before reaching this step
		}
		us := &unitSim{unit: u, obsStart: u.StartNs(), obsEnd: u.EndNs()}
		var dur int64
		if u.Callee != "" || u.First().IsClient() {
			g := s.group(u, n)
			us.outcomes, us.err, dur = g.attempts, g.err, g.durNs
		} else {
			co := s.node(u.First())
			us.outcomes, us.err, dur = []*outcome{co}, co.err, co.durNs
		}
		// Sequencing: a unit follows the latest-finishing earlier unit that
		// had ended when it started, keeping the observed gap (self time).
		var pred *unitSim
		for _, p := range done {
			if p.obsEnd <= us.obsStart && (pred == nil || p.obsEnd >= pred.obsEnd) {
				pred = p
			}
		}
		if pred == nil {
			us.cfStart = scaleGap(us.obsStart-n.StartNs, f)
		} else {
			us.cfStart = pred.cfEnd + scaleGap(us.obsStart-pred.obsEnd, f)
		}
		us.cfEnd = us.cfStart + dur
		done = append(done, us)
		o.children = append(o.children, us.outcomes...)

		if u.Failed() {
			observedFailed++
			if us.err {
				stillFailing = true
			}
			if lastObservedFailure == nil || us.obsEnd >= lastObservedFailure.obsEnd {
				lastObservedFailure = us
			}
		} else if us.err && newFailure == nil && s.propagates(n, u) {
			newFailure = us
		}
	}

	// Duration from the last unit that had ended when the span ended.
	var last *unitSim
	for _, p := range done {
		if p.obsEnd <= n.EndNs && (last == nil || p.obsEnd >= last.obsEnd) {
			last = p
		}
	}
	if last == nil {
		o.durNs = scaleGap(n.DurationNs(), f)
	} else {
		o.durNs = last.cfEnd + scaleGap(n.EndNs-last.obsEnd, f)
	}

	originErr := n.IsError() && observedFailed == 0
	switch {
	case newFailure != nil:
		// A step that succeeded in the recording now fails and the failure
		// propagates: the span ends shortly after that step.
		o.err = true
		o.durNs = newFailure.cfEnd + s.failTail(n, newFailure.unit.Callee)
	case originErr:
		o.err = !s.removeErrors[n.Span.Service]
		if !o.err {
			// The failed execution is replaced by a draw from successful
			// baseline executions of the same operation, when available.
			if pool := s.m.op(opKey{n.Span.Service, n.Span.Name}).baselineOK; len(pool) > 0 {
				o.durNs = pool[s.rng(n, "restore", 0).IntN(len(pool))]
				o.imputed = true
				s.diag.ImputedDurations++
				s.traceImputed[n.Span.TraceID] = true
			} else {
				s.diag.Unimputable++
			}
		}
	case n.IsError():
		// Propagated failure: it persists only while a step that failed in
		// the recording still fails.
		o.err = stillFailing
		if !o.err && lastObservedFailure != nil && lastObservedFailure == last {
			// The span stopped at the failed step in the recording. With
			// that step succeeding it would have carried on; the remaining
			// work is imputed from successful executions.
			// When the recorded remainder is already as long as the
			// shortest successful one, nothing was skipped and the
			// recorded remainder stands.
			pool := s.m.op(opKey{n.Span.Service, n.Span.Name}).successRemainder[last.unit.Callee]
			if len(pool) > 0 && n.EndNs-last.obsEnd >= pool[0] {
				// not truncated
			} else if len(pool) > 0 {
				o.durNs = last.cfEnd + pool[s.rng(n, "continue", 0).IntN(len(pool))]
				o.imputed = true
				s.diag.ImputedContinuance++
				s.traceImputed[n.Span.TraceID] = true
			} else {
				s.diag.Unimputable++
			}
		}
	}
	if o.durNs < 0 {
		o.durNs = 0
	}
	return o
}

// propagates decides whether a newly failing unit fails its parent span,
// following what the dataset shows for that caller/callee pair. With no
// observation the failure is assumed to propagate, and that is counted.
func (s *sim) propagates(n *tree.Node, u tree.Unit) bool {
	if u.Callee == "" {
		s.diag.AssumedPropagation++
		return true
	}
	em := s.m.edge(edgeKey{n.Span.Service, u.Callee})
	if em.failed == 0 {
		s.diag.AssumedPropagation++
		return true
	}
	return em.propagated*2 >= em.failed
}

func (s *sim) failTail(n *tree.Node, callee string) int64 {
	return medianInt(s.m.op(opKey{n.Span.Service, n.Span.Name}).failTail[callee])
}

type groupResult struct {
	attempts []*outcome
	durNs    int64
	err      bool
}

// group simulates a retry group: attempts run in order until one succeeds or
// the attempt budget is exhausted.
func (s *sim) group(u tree.Unit, parent *tree.Node) groupResult {
	caller := u.First().Span.Service
	ek := edgeKey{caller, u.Callee}
	em := s.m.edge(ek)
	observed := len(u.Attempts)
	policy, overridden := s.retryPolicy[ek]
	budget := max(em.maxAttempts, observed)
	if overridden {
		budget = policy
	}

	var res groupResult
	var elapsed int64
	for i := 0; i < budget; i++ {
		var ao *outcome
		if i < observed {
			ao = s.attempt(u.Attempts[i], ek, em)
		} else {
			ao = s.synthAttempt(u.First(), ek, em, i)
			if ao == nil {
				break
			}
		}
		ao.retry = i > 0
		res.attempts = append(res.attempts, ao)
		elapsed += ao.durNs
		res.err = ao.err
		if !ao.err {
			break
		}
		next := i + 1
		if next >= budget {
			break
		}
		if next < observed {
			elapsed += max(u.Attempts[next].StartNs-u.Attempts[i].EndNs, 0)
			continue
		}
		// Beyond the recording. Retry only if the policy was raised, or if
		// the final recorded attempt used to succeed and now fails.
		flipped := i == observed-1 && !u.Attempts[i].IsError()
		if !overridden && !flipped {
			break
		}
		elapsed += em.backoffNs
	}
	res.durNs = elapsed
	return res
}

// attempt simulates one recorded client attempt.
func (s *sim) attempt(a *tree.Node, ek edgeKey, em *edgeModel) *outcome {
	o := &outcome{node: a, service: a.Span.Service, name: a.Span.Name, client: true, callee: ek.callee,
		entry: a.IsEntry(), durNs: a.DurationNs(), err: a.IsError()}
	override, hasOverride := s.timeoutOver[ek]
	srv := a.ServerChild()
	if srv == nil {
		// Nothing recorded on the callee side: it was unreachable or is not
		// instrumented. Only an explicit restore changes the outcome.
		if a.IsError() && s.removeErrors[ek.callee] {
			if len(em.baselineOK) > 0 {
				o.durNs = em.baselineOK[s.rng(a, "restore", 0).IntN(len(em.baselineOK))]
				o.err, o.imputed = false, true
				s.diag.ImputedDurations++
				s.traceImputed[a.Span.TraceID] = true
			} else {
				s.diag.Unimputable++
			}
		}
		if hasOverride && o.durNs >= override {
			o.durNs, o.err = override, true
		}
		return o
	}

	so := s.node(srv)
	o.children = []*outcome{so}
	if a.IsTimeout() {
		// The client gave up at a.DurationNs. The callee took at least that
		// long; if its span is shorter the recording was cut off (censored)
		// and its true duration is only bounded from below.
		lower := max(srv.DurationNs(), a.DurationNs())
		if srv.DurationNs() < a.DurationNs() && !s.censoredSeen[a] {
			s.censoredSeen[a] = true
			s.diag.CensoredAttempts++
		}
		ratio := 1.0
		if srv.DurationNs() > 0 {
			ratio = float64(so.durNs) / float64(srv.DurationNs())
		}
		d := int64(math.Round(float64(lower)*ratio)) + em.overheadNs
		limit := a.DurationNs()
		if hasOverride {
			limit = override
		}
		if d >= limit {
			o.durNs, o.err = limit, true
		} else {
			o.durNs, o.err = d, so.err
		}
		return o
	}

	// The attempt changes by exactly as much as its callee did.
	d := max(a.DurationNs()+so.durNs-srv.DurationNs(), 0)
	limit := em.timeoutNs
	if hasOverride {
		limit = override
	}
	// An inferred timeout only applies when the attempt got slower than
	// recorded, so that a no-op replay reproduces the recording exactly.
	if limit > 0 && d >= limit && (hasOverride || d > a.DurationNs()) {
		o.durNs, o.err = limit, true
		return o
	}
	o.durNs = d
	o.err = so.err || (a.IsError() && !srv.IsError())
	return o
}

// synthAttempt creates an attempt that was not in the recording by drawing a
// callee execution from those recorded on the same edge during the incident
// and simulating it under the scenario. It returns nil when the edge has no
// recorded executions to draw from.
func (s *sim) synthAttempt(first *tree.Node, ek edgeKey, em *edgeModel, index int) *outcome {
	if len(em.incidentServers) == 0 {
		s.diag.Unimputable++
		return nil
	}
	pick := em.incidentServers[s.rng(first, "synth", index).IntN(len(em.incidentServers))]
	so := s.node(pick)
	child := *so
	child.synthesized = true
	o := &outcome{node: first, service: ek.caller, name: first.Span.Name, client: true, callee: ek.callee,
		synthesized: true, children: []*outcome{&child}}
	d := so.durNs + em.overheadNs
	limit := em.timeoutNs
	if over, ok := s.timeoutOver[ek]; ok {
		limit = over
	}
	if limit > 0 && d >= limit {
		o.durNs, o.err = limit, true
	} else {
		o.durNs, o.err = d, so.err
	}
	s.diag.SynthAttempts++
	s.traceSynth[first.Span.TraceID] = true
	return o
}
