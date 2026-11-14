// Package memory implements Incident Memory: structured records of past
// incidents, a reproducible fingerprint and similarity method, and the data
// behind the Incident Memory Graph. See docs/INCIDENT_MEMORY_GRAPH.md.
//
// Nothing here estimates probabilities. Similarity is a weighted overlap of
// named observable features, and every match lists exactly which features are
// shared and which differ. Similarity is correlation between incidents, never
// evidence that one caused the other.
package memory

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/replay"
)

// MethodVersion identifies the fingerprint and similarity method. Change it
// whenever feature names or weights change.
const MethodVersion = "mem-1"

// Feature classes and their fixed weights. Weights express how specific a
// shared feature is; they are not fitted to data.
const (
	ClassEdge    = "edge"    // a dependency edge named by a detection signal or evidence
	ClassOrigin  = "origin"  // the top-ranked hypothesis, as kind@service
	ClassRule    = "rule"    // a detection rule that fired
	ClassPrimary = "primary" // the service the incident is attributed to
	ClassService = "service" // any affected service
)

var weights = map[string]float64{ClassEdge: 3, ClassOrigin: 3, ClassRule: 2, ClassPrimary: 2, ClassService: 1}

// Weights returns a copy of the feature weights, for documentation and the API.
func Weights() map[string]float64 {
	out := map[string]float64{}
	for k, v := range weights {
		out[k] = v
	}
	return out
}

// Signal is a detection signal as remembered.
type Signal struct {
	Rule     string  `json:"rule"`
	Service  string  `json:"service"`
	Peer     string  `json:"peer,omitempty"`
	Severity string  `json:"severity"`
	Unit     string  `json:"unit"`
	Peak     float64 `json:"peak"`
	Baseline float64 `json:"baseline"`
}

// Hypothesis is an investigation hypothesis as remembered.
type Hypothesis struct {
	Rank          int      `json:"rank"`
	Kind          string   `json:"kind"`
	Service       string   `json:"service"`
	Peer          string   `json:"peer,omitempty"`
	Title         string   `json:"title"`
	Score         int      `json:"score"`
	MaxScore      int      `json:"max_score"`
	Supporting    []string `json:"supporting"`
	Contradicting []string `json:"contradicting"`
}

// Intervention is something tried against the incident, with its outcome.
// Replay interventions are simulated telemetry results, not real changes.
type Intervention struct {
	Source  string `json:"source"` // "replay"
	Class   string `json:"class"`  // "simulated"
	Name    string `json:"name"`
	Detail  string `json:"detail"`
	Effect  string `json:"effect"` // improved, worsened, mixed, unchanged
	Summary string `json:"summary"`
}

// Recovery records how the incident ended.
type Recovery struct {
	Status      string     `json:"status"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	DurationSec float64    `json:"duration_seconds"`
	Cause       string     `json:"cause"` // what is known about why it ended
}

// Record is the structured memory of one incident.
type Record struct {
	ID            int64          `json:"id"`
	Key           string         `json:"key"`
	IncidentID    *int64         `json:"incident_id,omitempty"`
	Simulated     bool           `json:"simulated"`
	Method        string         `json:"method_version"`
	Title         string         `json:"title"`
	Severity      string         `json:"severity"`
	TriggerRule   string         `json:"trigger_rule"`
	StartedAt     time.Time      `json:"started_at"`
	Primary       string         `json:"primary_service"`
	Affected      []string       `json:"affected_services"`
	Signals       []Signal       `json:"signals"`
	Edges         []string       `json:"dependency_edges"` // "caller>callee"
	Sequence      []string       `json:"event_sequence"`   // "rule@service" in first-seen order
	Hypotheses    []Hypothesis   `json:"hypotheses"`
	KnownFaults   []string       `json:"known_faults"`
	Interventions []Intervention `json:"interventions"`
	Notes         []string       `json:"operator_notes"`
	Recovery      Recovery       `json:"recovery"`
	TraceIDs      []string       `json:"example_trace_ids"`
	Limitations   []string       `json:"limitations"`
	Features      []string       `json:"fingerprint"`
}

func uniqSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Fingerprint derives the sorted feature set of a record. It depends only on
// the record's stored fields, so it is reproducible.
func Fingerprint(r Record) []string {
	var f []string
	for _, s := range r.Signals {
		f = append(f, ClassRule+":"+s.Rule)
		if s.Peer != "" {
			f = append(f, edge(s.Service, s.Peer))
		}
	}
	for _, e := range r.Edges {
		f = append(f, ClassEdge+":"+e)
	}
	if r.Primary != "" {
		f = append(f, ClassPrimary+":"+r.Primary)
	}
	for _, s := range r.Affected {
		f = append(f, ClassService+":"+s)
	}
	for _, h := range r.Hypotheses {
		if h.Rank == 1 && h.Kind != investigate.KindUndetermined {
			f = append(f, ClassOrigin+":"+h.Kind+"@"+h.Service)
		}
	}
	return uniqSorted(f)
}

func edge(a, b string) string { return ClassEdge + ":" + a + ">" + b }

func class(feature string) string { c, _, _ := strings.Cut(feature, ":"); return c }

// Overlap is the weighted Jaccard overlap of two feature sets: the summed
// weight of shared features divided by the summed weight of all features in
// either set. It is 0 when nothing is shared and 1 when the sets are equal.
// It is a ranking aid for engineers, not a probability.
func Overlap(a, b []string) (value float64, shared, onlyA, onlyB []string) {
	inB := map[string]bool{}
	for _, f := range b {
		inB[f] = true
	}
	inA := map[string]bool{}
	var num, den float64
	for _, f := range a {
		inA[f] = true
		w := weights[class(f)]
		den += w
		if inB[f] {
			num += w
			shared = append(shared, f)
		} else {
			onlyA = append(onlyA, f)
		}
	}
	for _, f := range b {
		if !inA[f] {
			den += weights[class(f)]
			onlyB = append(onlyB, f)
		}
	}
	if den == 0 {
		return 0, shared, onlyA, onlyB
	}
	return math.Round(num/den*1000) / 1000, shared, onlyA, onlyB
}

// isCore reports whether a shared feature is specific enough to justify
// calling two incidents similar: sharing only a common service is not.
func isCore(feature string) bool { return class(feature) != ClassService }

// FromView builds a record from an investigation view and the incident's
// replay runs (with results). It never invents data: absent investigation
// data yields an empty hypotheses list and the limitations say so.
func FromView(v *investigate.View, runs []model.ReplayRun, now time.Time) Record {
	inc := v.Incident
	id := inc.ID
	r := Record{Key: inc.Key, IncidentID: &id, Method: MethodVersion, Title: inc.Title, Severity: inc.Severity,
		TriggerRule: inc.TriggerRule, StartedAt: inc.StartedAt, Primary: inc.PrimaryService,
		Affected: uniqSorted(inc.AffectedServices), Signals: []Signal{}, Hypotheses: []Hypothesis{},
		KnownFaults: []string{}, Interventions: []Intervention{}, Notes: []string{}, Sequence: []string{}, TraceIDs: []string{}}
	sigs := append([]model.Signal(nil), inc.Signals...)
	sort.SliceStable(sigs, func(i, j int) bool { return sigs[i].FirstSeen.Before(sigs[j].FirstSeen) })
	var edges, traces []string
	for _, s := range sigs {
		r.Signals = append(r.Signals, Signal{Rule: s.Rule, Service: s.Service, Peer: s.Peer, Severity: s.Severity,
			Unit: s.Unit, Peak: s.PeakValue, Baseline: s.Baseline})
		r.Sequence = append(r.Sequence, s.Rule+"@"+s.Service)
		if s.Peer != "" {
			edges = append(edges, s.Service+">"+s.Peer)
		}
		traces = append(traces, s.Exemplars...)
	}
	r.Sequence = dedupeKeepOrder(r.Sequence)
	for _, h := range v.Hypotheses {
		mh := Hypothesis{Rank: h.Rank, Kind: h.Kind, Service: h.Service, Peer: h.Peer, Title: h.Title, Score: h.Score,
			MaxScore: h.MaxScore, Supporting: []string{}, Contradicting: []string{}}
		for _, e := range h.Evidence {
			switch e.Polarity {
			case "supporting":
				mh.Supporting = append(mh.Supporting, e.Text)
			case "contradicting":
				mh.Contradicting = append(mh.Contradicting, e.Text)
			}
			if e.Peer != "" && e.Service != "" {
				edges = append(edges, e.Service+">"+e.Peer)
			}
		}
		r.Hypotheses = append(r.Hypotheses, mh)
	}
	r.Edges = uniqSorted(edges)
	if len(traces) > 5 {
		traces = traces[:5]
	}
	r.TraceIDs = uniqSorted(traces)
	for _, f := range v.KnownFaults {
		r.KnownFaults = append(r.KnownFaults, fmt.Sprintf("%s (targets: %s)", f.Name, strings.Join(f.Targets, ", ")))
	}
	for _, n := range inc.Notes {
		r.Notes = append(r.Notes, n.Body)
	}
	for _, run := range runs {
		if iv, ok := interventionOf(run); ok {
			r.Interventions = append(r.Interventions, iv)
		}
	}
	r.Recovery = recoveryOf(inc, v.KnownFaults, now)
	r.Limitations = []string{
		"Fingerprint features come from detection signals and the top-ranked hypothesis; both are heuristics.",
		"Replay interventions are simulated from captured telemetry, not re-executions of the application.",
	}
	if len(v.Hypotheses) == 0 {
		r.Limitations = append(r.Limitations, "No investigation dataset was captured, so this record has no hypotheses and no origin feature.")
	}
	if len(runs) == 0 {
		r.Limitations = append(r.Limitations, "No interventions were tested for this incident.")
	}
	r.Features = Fingerprint(r)
	return r
}

func dedupeKeepOrder(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func recoveryOf(inc model.Incident, faults []investigate.KnownFault, now time.Time) Recovery {
	rec := Recovery{Status: inc.Status, ResolvedAt: inc.ResolvedAt, Cause: "not recorded"}
	end := now
	if inc.ResolvedAt != nil {
		end = *inc.ResolvedAt
	}
	rec.DurationSec = math.Round(end.Sub(inc.StartedAt).Seconds())
	if inc.ResolvedAt != nil {
		for _, f := range faults {
			if f.EndedAt != nil && !f.EndedAt.After(*inc.ResolvedAt) {
				rec.Cause = fmt.Sprintf("a known injected fault ended before resolution (%s)", f.Name)
				return rec
			}
		}
		rec.Cause = "resolved with no recorded cause"
	}
	return rec
}

func interventionOf(run model.ReplayRun) (Intervention, bool) {
	var res replay.Result
	if len(run.Result) == 0 || json.Unmarshal(run.Result, &res) != nil {
		return Intervention{}, false
	}
	var ivs []replay.Intervention
	_ = json.Unmarshal(run.Interventions, &ivs)
	parts := make([]string, 0, len(ivs))
	for _, iv := range ivs {
		parts = append(parts, DescribeIntervention(iv))
	}
	o, c := res.Summary.Original, res.Summary.Counterfactual
	return Intervention{Source: "replay", Class: "simulated", Name: run.Name, Detail: strings.Join(parts, "; "),
		Effect: Effect(o.ErrorRate, c.ErrorRate, o.P95Ms, c.P95Ms),
		Summary: fmt.Sprintf("error rate %.1f%% → %.1f%%, p95 %.0f ms → %.0f ms (simulated)",
			o.ErrorRate*100, c.ErrorRate*100, o.P95Ms, c.P95Ms)}, true
}

// DescribeIntervention renders a replay intervention in words.
func DescribeIntervention(iv replay.Intervention) string {
	switch iv.Type {
	case "scale_latency":
		if iv.Mode == "baseline" {
			return fmt.Sprintf("restore %s self-time to baseline", iv.Service)
		}
		return fmt.Sprintf("scale %s latency by %.2g×", iv.Service, iv.Factor)
	case "remove_errors":
		return fmt.Sprintf("remove %s's own errors", iv.Service)
	case "retry_policy":
		return fmt.Sprintf("%s→%s: %d attempts", iv.Caller, iv.Callee, iv.MaxAttempts)
	case "set_timeout":
		return fmt.Sprintf("%s→%s: timeout %.0f ms", iv.Caller, iv.Callee, iv.TimeoutMs)
	}
	return iv.Type
}

// Effect classifies a simulated before/after pair using the same tolerances
// the replay engine uses to call a metric changed: 0.5 percentage points for
// error rate, and for p95 both 1 ms and 2%.
func Effect(origErr, cfErr, origP95, cfP95 float64) string {
	better, worse := false, false
	if d := cfErr - origErr; d < -0.005 {
		better = true
	} else if d > 0.005 {
		worse = true
	}
	d := math.Abs(cfP95 - origP95)
	if d > 1 && d > 0.02*math.Max(origP95, cfP95) {
		if cfP95 < origP95 {
			better = true
		} else {
			worse = true
		}
	}
	switch {
	case better && worse:
		return "mixed"
	case better:
		return "improved"
	case worse:
		return "worsened"
	}
	return "unchanged"
}

// Match is one historical incident compared with the current one.
type Match struct {
	Record        Record         `json:"record"`
	Overlap       float64        `json:"overlap"`
	Shared        []string       `json:"shared_features"`
	OnlyCurrent   []string       `json:"only_in_current"`
	OnlyHistoric  []string       `json:"only_in_historical"`
	Differences   []string       `json:"differences"`
	Interventions []Intervention `json:"interventions"`
}

// Pattern is a feature that recurs across the current and matched incidents.
type Pattern struct {
	Feature   string   `json:"feature"`
	Incidents []string `json:"incidents"`
}

// Similarity is the result of comparing one incident with the memory.
type Similarity struct {
	Method          string             `json:"method_version"`
	Weights         map[string]float64 `json:"weights"`
	Fingerprint     []string           `json:"fingerprint"`
	HistoricalCount int                `json:"historical_count"`
	SimulatedCount  int                `json:"simulated_count"`
	Matches         []Match            `json:"matches"`
	Recurring       []Pattern          `json:"recurring_patterns"`
	Services        []Pattern          `json:"commonly_affected_services"`
	Notes           []string           `json:"notes"`
}

// MaxMatches bounds the matches returned.
const MaxMatches = 8

// Find compares cur with the history. Records with the same incident id as cur
// are skipped. A record matches only when it shares at least one specific
// feature (anything except a bare affected service).
func Find(cur Record, history []Record) Similarity {
	cur.Features = Fingerprint(cur)
	out := Similarity{Method: MethodVersion, Weights: Weights(), Fingerprint: cur.Features, Matches: []Match{},
		Recurring: []Pattern{}, Services: []Pattern{}, Notes: []string{
			"Similarity is a weighted overlap of observable features. It is not a probability and does not show that incidents share a cause.",
			"Interventions from past incidents were tested on different data; their outcomes may not transfer.",
		}}
	for _, h := range history {
		if cur.IncidentID != nil && h.IncidentID != nil && *cur.IncidentID == *h.IncidentID {
			continue
		}
		out.HistoricalCount++
		if h.Simulated {
			out.SimulatedCount++
		}
		h.Features = Fingerprint(h)
		ov, shared, onlyCur, onlyHist := Overlap(cur.Features, h.Features)
		core := false
		for _, f := range shared {
			core = core || isCore(f)
		}
		if !core {
			continue
		}
		out.Matches = append(out.Matches, Match{Record: h, Overlap: ov, Shared: shared, OnlyCurrent: nilToEmpty(onlyCur),
			OnlyHistoric: nilToEmpty(onlyHist), Differences: differences(cur, h), Interventions: h.Interventions})
	}
	sort.SliceStable(out.Matches, func(i, j int) bool {
		a, b := out.Matches[i], out.Matches[j]
		if a.Overlap != b.Overlap {
			return a.Overlap > b.Overlap
		}
		return a.Record.Key < b.Record.Key
	})
	if len(out.Matches) > MaxMatches {
		out.Matches = out.Matches[:MaxMatches]
	}
	feat := map[string][]string{}
	svc := map[string][]string{}
	for _, m := range out.Matches {
		for _, f := range m.Shared {
			if class(f) == ClassService {
				svc[f] = append(svc[f], m.Record.Key)
			} else {
				feat[f] = append(feat[f], m.Record.Key)
			}
		}
	}
	out.Recurring = patterns(feat, 1)
	out.Services = patterns(svc, 1)
	switch {
	case out.HistoricalCount == 0:
		out.Notes = append(out.Notes, "Incident memory holds no other incidents yet; nothing can be compared.")
	case len(out.Matches) == 0:
		out.Notes = append(out.Notes, "No remembered incident shares a specific feature (rule, edge, origin or primary service) with this one.")
	}
	if len(cur.Hypotheses) == 0 {
		out.Notes = append(out.Notes, "This incident has no investigation hypotheses yet, so its fingerprint lacks an origin feature; matches rely on detection signals only.")
	}
	return out
}

func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func patterns(m map[string][]string, minCount int) []Pattern {
	out := []Pattern{}
	for f, keys := range m {
		if len(keys) >= minCount {
			sort.Strings(keys)
			out = append(out, Pattern{Feature: f, Incidents: keys})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Incidents) != len(out[j].Incidents) {
			return len(out[i].Incidents) > len(out[j].Incidents)
		}
		return out[i].Feature < out[j].Feature
	})
	return out
}

// differences lists observable contrasts between two records in words.
func differences(a, b Record) []string {
	var d []string
	if a.Severity != b.Severity {
		d = append(d, fmt.Sprintf("severity: %s now, %s then", a.Severity, b.Severity))
	}
	if a.TriggerRule != b.TriggerRule {
		d = append(d, fmt.Sprintf("first detected by %s now, %s then", a.TriggerRule, b.TriggerRule))
	}
	if a.Primary != b.Primary {
		d = append(d, fmt.Sprintf("attributed to %s now, %s then", a.Primary, b.Primary))
	}
	if ta, tb := top(a), top(b); ta != tb {
		d = append(d, fmt.Sprintf("top hypothesis: %s now, %s then", ta, tb))
	}
	if sa, sb := strings.Join(a.Sequence, " → "), strings.Join(b.Sequence, " → "); sa != sb && sa != "" && sb != "" {
		d = append(d, "signals fired in a different order or set")
	}
	if len(a.Hypotheses) > 0 && len(b.Hypotheses) > 0 {
		if cb := contradicted(b); cb > 0 {
			d = append(d, fmt.Sprintf("the historical top hypothesis had %d contradicting evidence item(s)", cb))
		}
	}
	if d == nil {
		d = []string{}
	}
	return d
}

func top(r Record) string {
	for _, h := range r.Hypotheses {
		if h.Rank == 1 {
			return h.Kind + "@" + h.Service
		}
	}
	return "none recorded"
}

func contradicted(r Record) int {
	for _, h := range r.Hypotheses {
		if h.Rank == 1 {
			return len(h.Contradicting)
		}
	}
	return 0
}

// Comparison puts two records side by side.
type Comparison struct {
	A           Record   `json:"a"`
	B           Record   `json:"b"`
	Overlap     float64  `json:"overlap"`
	Shared      []string `json:"shared_features"`
	OnlyA       []string `json:"only_in_a"`
	OnlyB       []string `json:"only_in_b"`
	Differences []string `json:"differences"`
}

// Compare puts two records side by side using the same method as Find.
func Compare(a, b Record) Comparison {
	a.Features, b.Features = Fingerprint(a), Fingerprint(b)
	ov, shared, onlyA, onlyB := Overlap(a.Features, b.Features)
	return Comparison{A: a, B: b, Overlap: ov, Shared: nilToEmpty(shared), OnlyA: nilToEmpty(onlyA),
		OnlyB: nilToEmpty(onlyB), Differences: differences(a, b)}
}
