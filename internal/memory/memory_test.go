package memory

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/replay"
)

func rec(key, primary string, rules []string, edges []string, origin string, affected ...string) Record {
	r := Record{Key: key, Primary: primary, Affected: affected, Edges: edges, Severity: "major", TriggerRule: rules[0]}
	for _, ru := range rules {
		r.Signals = append(r.Signals, Signal{Rule: ru, Service: primary})
	}
	if origin != "" {
		kind, svc, _ := strings.Cut(origin, "@")
		r.Hypotheses = []Hypothesis{{Rank: 1, Kind: kind, Service: svc}}
	}
	return r
}

func TestOverlapIsWeightedJaccard(t *testing.T) {
	a := []string{"edge:a>b", "service:a"}
	b := []string{"edge:a>b", "service:z"}
	v, shared, onlyA, onlyB := Overlap(a, b)
	// shared weight 3 of union 3+1+1 = 5
	if v != 0.6 || !reflect.DeepEqual(shared, []string{"edge:a>b"}) || len(onlyA) != 1 || len(onlyB) != 1 {
		t.Fatalf("got %v %v %v %v", v, shared, onlyA, onlyB)
	}
	if v, _, _, _ := Overlap(nil, nil); v != 0 {
		t.Fatalf("empty sets must give 0, got %v", v)
	}
	if v, _, _, _ := Overlap(a, a); v != 1 {
		t.Fatalf("identical sets must give 1, got %v", v)
	}
}

func TestFingerprintIsDeterministicAndOrderIndependent(t *testing.T) {
	r1 := rec("X", "checkout", []string{"error_rate", "retry_anomaly"}, []string{"checkout>payment"}, "latency_origin@payment", "payment", "checkout")
	r2 := r1
	r2.Affected = []string{"checkout", "payment"}
	r2.Signals = []Signal{r1.Signals[1], r1.Signals[0]}
	if !reflect.DeepEqual(Fingerprint(r1), Fingerprint(r2)) {
		t.Fatalf("fingerprint depends on order: %v vs %v", Fingerprint(r1), Fingerprint(r2))
	}
	want := []string{"edge:checkout>payment", "origin:latency_origin@payment", "primary:checkout", "rule:error_rate", "rule:retry_anomaly", "service:checkout", "service:payment"}
	if got := Fingerprint(r1); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestUndeterminedHypothesisAddsNoOriginFeature(t *testing.T) {
	r := rec("X", "a", []string{"error_rate"}, nil, "undetermined@a", "a")
	for _, f := range Fingerprint(r) {
		if strings.HasPrefix(f, "origin:") {
			t.Fatalf("unexpected %s", f)
		}
	}
}

func TestFindRanksExplainsAndExcludesWeakMatches(t *testing.T) {
	cur := rec("INC-9", "checkout", []string{"dependency_degradation"}, []string{"checkout>payment"}, "latency_origin@payment", "checkout", "payment")
	id := int64(9)
	cur.IncidentID = &id
	same := rec("INC-1", "checkout", []string{"dependency_degradation"}, []string{"checkout>payment"}, "latency_origin@payment", "checkout", "payment")
	partial := rec("INC-2", "payment", []string{"error_rate"}, []string{"checkout>payment"}, "error_origin@payment", "checkout", "payment")
	onlySvc := rec("INC-3", "inventory", []string{"latency_anomaly"}, nil, "latency_origin@inventory", "payment", "inventory")
	self := rec("INC-9", "checkout", []string{"dependency_degradation"}, nil, "", "checkout")
	self.IncidentID = &id
	partial.Simulated = true

	got := Find(cur, []Record{onlySvc, partial, same, self})
	if got.HistoricalCount != 3 || got.SimulatedCount != 1 {
		t.Fatalf("counts: %+v", got)
	}
	if len(got.Matches) != 2 || got.Matches[0].Record.Key != "INC-1" || got.Matches[1].Record.Key != "INC-2" {
		t.Fatalf("matches: %+v", got.Matches)
	}
	if got.Matches[0].Overlap != 1 || got.Matches[0].Overlap <= got.Matches[1].Overlap {
		t.Fatalf("overlaps: %v %v", got.Matches[0].Overlap, got.Matches[1].Overlap)
	}
	m := got.Matches[1]
	if !contains(m.Shared, "edge:checkout>payment") || !contains(m.OnlyHistoric, "origin:error_origin@payment") ||
		!contains(m.OnlyCurrent, "origin:latency_origin@payment") || len(m.Differences) == 0 {
		t.Fatalf("explanation missing: %+v", m)
	}
	// Reproducible: identical inputs, identical output.
	a, _ := json.Marshal(Find(cur, []Record{onlySvc, partial, same}))
	b, _ := json.Marshal(Find(cur, []Record{same, partial, onlySvc}))
	if string(a) != string(b) {
		t.Fatal("result depends on history order")
	}
}

func TestFindHandlesSparseHistory(t *testing.T) {
	cur := rec("INC-1", "a", []string{"error_rate"}, nil, "", "a")
	none := Find(cur, nil)
	if len(none.Matches) != 0 || !anyContains(none.Notes, "no other incidents") || !anyContains(none.Notes, "no investigation hypotheses") {
		t.Fatalf("notes: %v", none.Notes)
	}
	unrelated := Find(cur, []Record{rec("INC-2", "z", []string{"latency_anomaly"}, nil, "", "z")})
	if len(unrelated.Matches) != 0 || !anyContains(unrelated.Notes, "No remembered incident shares") {
		t.Fatalf("notes: %v", unrelated.Notes)
	}
}

func TestEffectUsesReplayTolerances(t *testing.T) {
	for _, c := range []struct {
		oe, ce, op, cp float64
		want           string
	}{
		{0.3, 0, 2500, 80, "improved"},
		{0.3, 0.301, 2500, 2490, "unchanged"},
		{0.2, 0.3, 900, 700, "mixed"},
		{0.1, 0.2, 100, 100, "worsened"},
	} {
		if got := Effect(c.oe, c.ce, c.op, c.cp); got != c.want {
			t.Errorf("%+v: got %s", c, got)
		}
	}
}

func TestSeedsAreLabelledSimulatedAndSelfConsistent(t *testing.T) {
	keys := map[string]bool{}
	for _, s := range Seeds() {
		if !s.Simulated || !strings.HasPrefix(s.Key, "SIM-") || keys[s.Key] || s.IncidentID != nil {
			t.Fatalf("bad seed %+v", s)
		}
		keys[s.Key] = true
		if !anyContains(s.Notes, "SIMULATED EXAMPLE") || len(s.Features) == 0 {
			t.Fatalf("seed %s not labelled or fingerprinted", s.Key)
		}
	}
}

func TestGraphMergesSharedNodesAndHasNoDanglingEdges(t *testing.T) {
	recs := Seeds()
	real := rec("INC-1", "checkout", []string{"dependency_degradation"}, []string{"checkout>payment"}, "latency_origin@payment", "checkout", "payment")
	real.Recovery = Recovery{Status: "resolved", Cause: "resolved with no recorded cause"}
	g := BuildGraph(append(recs, real))
	ids := map[string]Node{}
	for _, n := range g.Nodes {
		if _, dup := ids[n.ID]; dup {
			t.Fatalf("duplicate node %s", n.ID)
		}
		ids[n.ID] = n
	}
	for _, e := range g.Edges {
		if _, ok := ids[e.Source]; !ok {
			t.Fatalf("dangling source %s", e.Source)
		}
		if _, ok := ids[e.Target]; !ok {
			t.Fatalf("dangling target %s", e.Target)
		}
	}
	if ids["svc:payment"].Simulated {
		t.Error("a service shared with a real incident must not be flagged simulated")
	}
	if !ids["inc:SIM-0001"].Simulated || ids["inc:INC-1"].Simulated {
		t.Error("incident simulated flags wrong")
	}
	if n := countEdges(g, "path:checkout>payment"); n < 3 {
		t.Errorf("shared path should link several incidents, got %d", n)
	}
	if len(BuildGraph(nil).Edges) != 0 {
		t.Error("empty memory must give an empty graph")
	}
}

func TestFromViewBuildsRecordFromStoredData(t *testing.T) {
	start := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Minute)
	endFault := start.Add(2 * time.Minute)
	v := &investigate.View{
		Incident: model.Incident{ID: 7, Key: "INC-0007", Title: "t", Severity: "critical", Status: "resolved", PrimaryService: "checkout",
			AffectedServices: []string{"payment", "checkout"}, TriggerRule: "dependency_degradation", StartedAt: start, ResolvedAt: &end,
			Signals: []model.Signal{
				{Rule: "retry_anomaly", Service: "checkout", Peer: "payment", FirstSeen: start.Add(time.Minute)},
				{Rule: "dependency_degradation", Service: "checkout", Peer: "payment", FirstSeen: start}},
			Notes: []model.Note{{Body: "restarted payment"}}},
		Hypotheses: []model.Hypothesis{{Rank: 1, Kind: "latency_origin", Service: "payment", Score: 9, MaxScore: 10,
			Evidence: []model.Evidence{{Polarity: "supporting", Text: "slower"}, {Polarity: "contradicting", Text: "volume flat"}}}},
		KnownFaults: []investigate.KnownFault{{Name: "Payment slowdown", Targets: []string{"payment"}, EndedAt: &endFault}},
	}
	res, _ := json.Marshal(replay.Result{Summary: replay.Summary{Comparison: replay.Comparison{
		Original: replay.Stat{ErrorRate: 0.3, P95Ms: 2500}, Counterfactual: replay.Stat{ErrorRate: 0, P95Ms: 80}}}})
	ivs, _ := json.Marshal([]replay.Intervention{{Type: "scale_latency", Service: "payment", Mode: "baseline"}})
	r := FromView(v, []model.ReplayRun{{Name: "Restore", Interventions: ivs, Result: res}}, end)

	if r.Sequence[0] != "dependency_degradation@checkout" || r.Sequence[1] != "retry_anomaly@checkout" {
		t.Errorf("sequence not ordered by first seen: %v", r.Sequence)
	}
	if len(r.Interventions) != 1 || r.Interventions[0].Effect != "improved" || r.Interventions[0].Class != "simulated" ||
		r.Interventions[0].Detail != "restore payment self-time to baseline" {
		t.Errorf("interventions: %+v", r.Interventions)
	}
	if h := r.Hypotheses[0]; len(h.Supporting) != 1 || len(h.Contradicting) != 1 {
		t.Errorf("evidence split: %+v", h)
	}
	if r.Recovery.DurationSec != 180 || !strings.Contains(r.Recovery.Cause, "Payment slowdown") {
		t.Errorf("recovery: %+v", r.Recovery)
	}
	if !contains(r.Features, "origin:latency_origin@payment") || !contains(r.Features, "edge:checkout>payment") {
		t.Errorf("features: %v", r.Features)
	}
	// An incident with no investigation data states that, rather than inventing any.
	sparse := FromView(&investigate.View{Incident: v.Incident}, nil, end)
	if len(sparse.Hypotheses) != 0 || !anyContains(sparse.Limitations, "No investigation dataset") || !anyContains(sparse.Limitations, "No interventions") {
		t.Errorf("sparse limitations: %v", sparse.Limitations)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
func anyContains(s []string, sub string) bool {
	for _, x := range s {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}
func countEdges(g Graph, target string) int {
	n := 0
	for _, e := range g.Edges {
		if e.Target == target {
			n++
		}
	}
	return n
}
