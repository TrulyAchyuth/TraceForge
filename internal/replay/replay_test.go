package replay

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/fixture"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

func mustRun(t *testing.T, ds *model.Dataset, sc Scenario) *Result {
	t.Helper()
	res, err := Run(ds, sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func scale(service string, factor float64) Scenario {
	return Scenario{Seed: 7, Interventions: []Intervention{{Type: TypeScaleLatency, Service: service, Mode: ModeFactor, Factor: factor}}}
}

func operation(t *testing.T, res *Result, name string) ScopedComparison {
	t.Helper()
	for _, op := range res.Operations {
		if strings.HasSuffix(op.Name, name) {
			return op
		}
	}
	t.Fatalf("operation %q not in result", name)
	return ScopedComparison{}
}

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// The fixture's incident phase: 60 checkouts (20 fail after three timeouts,
// 70 retries in total) and 60 product listings that never touch payment.
func TestOriginalSideMatchesRecording(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), scale("payment", 1))
	s := res.Summary
	if s.Original.Count != 120 || s.Original.Errors != 20 {
		t.Fatalf("original count/errors = %d/%d, want 120/20", s.Original.Count, s.Original.Errors)
	}
	if s.OriginalRetries != 70 {
		t.Fatalf("original retries = %d, want 70", s.OriginalRetries)
	}
	// Factor 1 changes nothing, so both sides must agree exactly.
	if s.Counterfactual != s.Original || s.CounterfactualRetries != s.OriginalRetries {
		t.Fatalf("factor 1 changed outcomes: %+v vs %+v", s.Counterfactual, s.Original)
	}
	if res.Diagnostics.TracesChanged != 0 {
		t.Fatalf("factor 1 changed %d traces", res.Diagnostics.TracesChanged)
	}
	for _, f := range res.Findings {
		if f.Changed {
			t.Errorf("finding %s / %s reported as changed under a no-op scenario", f.Scope, f.Metric)
		}
	}
}

func TestIdentitySelfCheckReproducesRecording(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), scale("payment", 0.5))
	d := res.Diagnostics
	if d.IdentityChecked != 120 || d.IdentityMismatches != 0 {
		t.Fatalf("identity self-check: %d mismatches of %d", d.IdentityMismatches, d.IdentityChecked)
	}
}

func TestReducingPaymentLatencyRemovesTimeoutsRetriesAndFailures(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), scale("payment", 0.1))
	s := res.Summary
	if s.Counterfactual.Errors != 0 {
		t.Errorf("counterfactual errors = %d, want 0", s.Counterfactual.Errors)
	}
	if s.CounterfactualRetries != 0 {
		t.Errorf("counterfactual retries = %d, want 0", s.CounterfactualRetries)
	}
	if !(s.Counterfactual.P95Ms < s.Original.P95Ms/5) {
		t.Errorf("p95 %.1f → %.1f ms: expected a large reduction", s.Original.P95Ms, s.Counterfactual.P95Ms)
	}
	co := operation(t, res, "GET /api/checkout")
	if co.Original.ErrorRate < 0.33 || co.Counterfactual.ErrorRate != 0 {
		t.Errorf("checkout error rate %.3f → %.3f, want ≈0.333 → 0", co.Original.ErrorRate, co.Counterfactual.ErrorRate)
	}
	// A checkout whose single payment attempt took 700 ms: gateway 1 + hop 1
	// + checkout (2 + inventory 11 + 1 + payment 70+1 + 1 + notify 6 + 1) + 1.
	// Every non-payment term is unchanged, so the trace shrinks by 630 ms.
	found := false
	for _, tr := range res.Traces {
		if approx(tr.OriginalMs, 726, 0.001) {
			found = true
			if !approx(tr.CounterfactualMs, 96, 0.001) {
				t.Errorf("700 ms payment trace: counterfactual %.3f ms, want 96", tr.CounterfactualMs)
			}
		}
	}
	if !found {
		t.Error("expected a 726 ms single-attempt checkout trace in the fixture")
	}
	if res.Diagnostics.ImputedContinuance != 20 {
		t.Errorf("imputed continuations = %d, want 20 (one per failed checkout)", res.Diagnostics.ImputedContinuance)
	}
}

func TestUnrelatedOperationIsReportedUnchanged(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), scale("payment", 0.1))
	products := operation(t, res, "GET /api/products")
	if products.Original != products.Counterfactual {
		t.Fatalf("products changed: %+v → %+v", products.Original, products.Counterfactual)
	}
	var changed, unchanged int
	for _, f := range res.Findings {
		if strings.Contains(f.Scope, "products") || strings.Contains(f.Scope, "inventory") {
			if f.Changed {
				t.Errorf("%s / %s should be unchanged", f.Scope, f.Metric)
			}
			unchanged++
		}
		if f.Changed {
			changed++
		}
	}
	if changed == 0 || unchanged == 0 {
		t.Fatalf("expected both changed (%d) and unchanged (%d) findings", changed, unchanged)
	}
}

func TestBaselineModeDerivesFactorFromData(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), Scenario{Seed: 1, Interventions: []Intervention{
		{Type: TypeScaleLatency, Service: "payment", Mode: ModeBaseline}}})
	f := res.Diagnostics.LatencyFactors["payment"]
	// Baseline payment self time is 38–42 ms; incident median is ~950 ms.
	if f <= 0 || f > 0.06 {
		t.Fatalf("baseline factor = %v, want roughly 40/950", f)
	}
	if res.Summary.Counterfactual.Errors != 0 {
		t.Errorf("errors = %d, want 0", res.Summary.Counterfactual.Errors)
	}
}

func TestRetryPolicySingleAttemptTradesRetriesForFailures(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), Scenario{Seed: 1, Interventions: []Intervention{
		{Type: TypeRetryPolicy, Caller: "checkout", Callee: "payment", MaxAttempts: 1}}})
	s := res.Summary
	if s.CounterfactualRetries != 0 {
		t.Errorf("retries = %d, want 0", s.CounterfactualRetries)
	}
	// With one attempt, the four patterns that begin with a timeout fail: 40 of 60.
	if s.Counterfactual.Errors != 40 {
		t.Errorf("errors = %d, want 40", s.Counterfactual.Errors)
	}
	if !(s.Counterfactual.P95Ms < s.Original.P95Ms) {
		t.Errorf("p95 %.1f → %.1f: failing fast should shorten the slowest requests", s.Original.P95Ms, s.Counterfactual.P95Ms)
	}
	var payment ScopedComparison
	for _, sv := range res.Services {
		if sv.Name == "payment" {
			payment = sv
		}
	}
	if payment.Original.Count != 130 || payment.Counterfactual.Count != 60 {
		t.Errorf("payment requests handled %d → %d, want 130 → 60", payment.Original.Count, payment.Counterfactual.Count)
	}
}

func TestRaisingTimeoutLetsSlowAttemptsFinish(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), Scenario{Seed: 1, Interventions: []Intervention{
		{Type: TypeSetTimeout, Caller: "checkout", Callee: "payment", TimeoutMs: 5000}}})
	s := res.Summary
	if s.Counterfactual.Errors != 0 || s.CounterfactualRetries != 0 {
		t.Fatalf("errors/retries = %d/%d, want 0/0", s.Counterfactual.Errors, s.CounterfactualRetries)
	}
}

func TestLoweringTimeoutCreatesNewFailuresAndSynthesisesRetries(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), Scenario{Seed: 3, Interventions: []Intervention{
		{Type: TypeSetTimeout, Caller: "checkout", Callee: "payment", TimeoutMs: 500}}})
	// Every recorded incident attempt takes ≥ 600 ms, so every checkout fails.
	if got := operation(t, res, "GET /api/checkout").Counterfactual.ErrorRate; got != 1 {
		t.Fatalf("checkout error rate = %v, want 1", got)
	}
	if res.Diagnostics.SynthAttempts == 0 {
		t.Error("expected synthesised attempts for requests that previously succeeded early")
	}
	foundLimit := false
	for _, l := range res.Limitations {
		if strings.Contains(l, "synthesised") {
			foundLimit = true
		}
	}
	if !foundLimit {
		t.Error("synthesised attempts must be disclosed in limitations")
	}
}

func paymentErrorsDataset() *model.Dataset {
	b := &fixture.Builder{}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		b.AddCheckout(model.PhaseBaseline, t0.Add(time.Duration(i)*time.Second), fixture.Checkout{PaymentMs: []float64{40}})
	}
	inc := t0.Add(10 * time.Minute)
	for i := 0; i < 30; i++ {
		c := fixture.Checkout{PaymentMs: []float64{5, 5, 5}, PaymentErr: []bool{true, true, true}}
		if i%3 == 0 {
			c = fixture.Checkout{PaymentMs: []float64{40}}
		}
		b.AddCheckout(model.PhaseIncident, inc.Add(time.Duration(i)*time.Second), c)
	}
	return &model.Dataset{ID: 2, Version: 1, Spans: b.Spans,
		Meta: model.DatasetMeta{IncidentTracesAvailable: 30, IncidentTracesSampled: 30}}
}

func TestRemoveErrorsRestoresOriginFailures(t *testing.T) {
	ds := paymentErrorsDataset()
	res := mustRun(t, ds, Scenario{Seed: 5, Interventions: []Intervention{{Type: TypeRemoveErrors, Service: "payment"}}})
	s := res.Summary
	if s.Original.Errors != 20 || s.Counterfactual.Errors != 0 {
		t.Fatalf("errors %d → %d, want 20 → 0", s.Original.Errors, s.Counterfactual.Errors)
	}
	if s.CounterfactualRetries != 0 {
		t.Errorf("retries = %d, want 0", s.CounterfactualRetries)
	}
	if res.Diagnostics.ImputedDurations == 0 {
		t.Error("restored executions should be imputed from baseline and counted")
	}
}

func TestRemoveErrorsRestoresUnreachableCallee(t *testing.T) {
	b := &fixture.Builder{}
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		b.AddCheckout(model.PhaseBaseline, t0.Add(time.Duration(i)*time.Second), fixture.Checkout{PaymentMs: []float64{40}})
	}
	for i := 0; i < 20; i++ {
		b.AddCheckout(model.PhaseIncident, t0.Add(10*time.Minute+time.Duration(i)*time.Second),
			fixture.Checkout{PaymentMs: []float64{0, 0, 0}, PaymentDown: true})
	}
	ds := &model.Dataset{ID: 3, Version: 1, Spans: b.Spans}
	res := mustRun(t, ds, Scenario{Seed: 5, Interventions: []Intervention{{Type: TypeRemoveErrors, Service: "payment"}}})
	if res.Summary.Original.Errors != 20 || res.Summary.Counterfactual.Errors != 0 {
		t.Fatalf("errors %d → %d, want 20 → 0", res.Summary.Original.Errors, res.Summary.Counterfactual.Errors)
	}
}

func TestDeterministicForSameInputsAndSeed(t *testing.T) {
	sc := Scenario{Seed: 42, Interventions: []Intervention{
		{Type: TypeScaleLatency, Service: "payment", Mode: ModeFactor, Factor: 0.2},
		{Type: TypeSetTimeout, Caller: "checkout", Callee: "payment", TimeoutMs: 150},
		{Type: TypeRetryPolicy, Caller: "checkout", Callee: "payment", MaxAttempts: 4},
	}}
	a, err := json.Marshal(mustRun(t, fixture.PaymentSlowdown(), sc))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		// Shuffle span order: results must not depend on input ordering.
		ds := fixture.PaymentSlowdown()
		r := rand.New(rand.NewPCG(uint64(i), 99))
		r.Shuffle(len(ds.Spans), func(x, y int) { ds.Spans[x], ds.Spans[y] = ds.Spans[y], ds.Spans[x] })
		b, err := json.Marshal(mustRun(t, ds, sc))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("run %d differs from the first run with identical inputs and seed", i)
		}
	}
}

func TestSeedChangesOnlySeededDraws(t *testing.T) {
	sc := Scenario{Seed: 1, Interventions: []Intervention{
		{Type: TypeSetTimeout, Caller: "checkout", Callee: "payment", TimeoutMs: 720}}}
	a := mustRun(t, fixture.PaymentSlowdown(), sc)
	sc.Seed = 2
	b := mustRun(t, fixture.PaymentSlowdown(), sc)
	if a.Diagnostics.SynthAttempts == 0 {
		t.Skip("scenario did not synthesise attempts")
	}
	// Original side never depends on the seed.
	if a.Summary.Original != b.Summary.Original {
		t.Fatal("original statistics changed with the seed")
	}
}

func TestReplayDoesNotMutateDataset(t *testing.T) {
	ds := fixture.PaymentSlowdown()
	before := store.Checksum(ds.Spans)
	raw, _ := json.Marshal(ds.Spans)
	for _, sc := range []Scenario{
		scale("payment", 0.1),
		{Seed: 9, Interventions: []Intervention{{Type: TypeRetryPolicy, Caller: "checkout", Callee: "payment", MaxAttempts: 6}}},
		{Seed: 9, Interventions: []Intervention{{Type: TypeRemoveErrors, Service: "payment"}}},
	} {
		mustRun(t, ds, sc)
	}
	if after := store.Checksum(ds.Spans); after != before {
		t.Fatal("dataset checksum changed after replay")
	}
	if after, _ := json.Marshal(ds.Spans); string(after) != string(raw) {
		t.Fatal("dataset spans changed after replay")
	}
}

func TestIncompleteTracesAreSkippedAndReported(t *testing.T) {
	ds := fixture.PaymentSlowdown()
	// Orphan one incident trace by dropping its root span.
	var victim string
	for _, sp := range ds.Spans {
		if sp.Phase == model.PhaseIncident && sp.ParentSpanID == "" {
			victim = sp.TraceID
			break
		}
	}
	kept := ds.Spans[:0:0]
	for _, sp := range ds.Spans {
		if sp.TraceID == victim && sp.ParentSpanID == "" {
			continue
		}
		kept = append(kept, sp)
	}
	ds.Spans = kept
	res := mustRun(t, ds, scale("payment", 0.5))
	if res.Diagnostics.TracesSkipped != 1 || res.Diagnostics.TracesReplayed != 119 {
		t.Fatalf("skipped/replayed = %d/%d, want 1/119", res.Diagnostics.TracesSkipped, res.Diagnostics.TracesReplayed)
	}
}

func TestInvalidInterventionsAreRejected(t *testing.T) {
	ds := fixture.PaymentSlowdown()
	cases := map[string]Scenario{
		"no interventions":    {},
		"unknown type":        {Interventions: []Intervention{{Type: "delete_service", Service: "payment"}}},
		"unknown service":     {Interventions: []Intervention{{Type: TypeScaleLatency, Service: "billing", Factor: 0.5}}},
		"negative factor":     {Interventions: []Intervention{{Type: TypeScaleLatency, Service: "payment", Factor: -1}}},
		"huge factor":         {Interventions: []Intervention{{Type: TypeScaleLatency, Service: "payment", Factor: 1e9}}},
		"bad mode":            {Interventions: []Intervention{{Type: TypeScaleLatency, Service: "payment", Mode: "magic"}}},
		"unknown edge":        {Interventions: []Intervention{{Type: TypeRetryPolicy, Caller: "payment", Callee: "gateway", MaxAttempts: 2}}},
		"zero attempts":       {Interventions: []Intervention{{Type: TypeRetryPolicy, Caller: "checkout", Callee: "payment"}}},
		"too many attempts":   {Interventions: []Intervention{{Type: TypeRetryPolicy, Caller: "checkout", Callee: "payment", MaxAttempts: 99}}},
		"zero timeout":        {Interventions: []Intervention{{Type: TypeSetTimeout, Caller: "checkout", Callee: "payment"}}},
		"duplicate":           {Interventions: []Intervention{{Type: TypeRemoveErrors, Service: "payment"}, {Type: TypeRemoveErrors, Service: "payment"}}},
		"negative objective":  {LatencyObjectiveMs: -5, Interventions: []Intervention{{Type: TypeRemoveErrors, Service: "payment"}}},
		"too many":            {Interventions: make([]Intervention, MaxInterventions+1)},
		"baseline without it": {},
	}
	for name, sc := range cases {
		if _, err := Run(ds, sc); err == nil || !IsValidation(err) {
			t.Errorf("%s: want validation error, got %v", name, err)
		}
	}
	// Baseline mode needs baseline executions of the service.
	onlyIncident := &model.Dataset{}
	for _, sp := range ds.Spans {
		if sp.Phase == model.PhaseIncident {
			onlyIncident.Spans = append(onlyIncident.Spans, sp)
		}
	}
	_, err := Run(onlyIncident, Scenario{Interventions: []Intervention{{Type: TypeScaleLatency, Service: "payment", Mode: ModeBaseline}}})
	if !IsValidation(err) {
		t.Errorf("baseline mode without baseline data: want validation error, got %v", err)
	}
}

func TestEmptyDataset(t *testing.T) {
	_, err := Run(&model.Dataset{}, scale("payment", 0.5))
	if !IsValidation(err) {
		t.Fatalf("empty dataset: want validation error (unknown service), got %v", err)
	}
}

func TestEveryResultCarriesScientificCaveats(t *testing.T) {
	res := mustRun(t, fixture.PaymentSlowdown(), scale("payment", 0.1))
	if res.EvidenceClass != model.ClassSimulated {
		t.Errorf("evidence class = %q, want simulated", res.EvidenceClass)
	}
	if len(res.Assumptions) == 0 || len(res.Limitations) == 0 || len(res.Unsupported) == 0 {
		t.Fatal("result must list assumptions, limitations and unsupported conclusions")
	}
	if !strings.Contains(strings.Join(res.Unsupported, " "), "does not prove") {
		t.Error("unsupported conclusions must state that the replay is not proof of causation")
	}
}

func TestOptionsDescribeDataset(t *testing.T) {
	opts := OptionsFor(fixture.PaymentSlowdown())
	if len(opts.Services) != 5 {
		t.Fatalf("services = %v, want 5", opts.Services)
	}
	for _, e := range opts.Edges {
		if e.Caller == "checkout" && e.Callee == "payment" {
			if e.ObservedMaxAttempts != 3 || !approx(e.InferredTimeoutMs, 800, 0.001) {
				t.Fatalf("checkout→payment inferred attempts/timeout = %d/%.1f, want 3/800", e.ObservedMaxAttempts, e.InferredTimeoutMs)
			}
			return
		}
	}
	t.Fatal("checkout→payment edge missing from options")
}
