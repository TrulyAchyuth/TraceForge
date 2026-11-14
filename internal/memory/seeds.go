package memory

import "time"

// SeedNote is shown with every seeded record.
const SeedNote = "SIMULATED EXAMPLE: written by hand to demonstrate Incident Memory. It was not derived from telemetry and does not describe a real incident."

func seedRecord(key, title, sev, trigger, primary string, affected []string, sigs []Signal, edges []string,
	top Hypothesis, ivs []Intervention, cause string, dur float64, daysAgo int) Record {
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).AddDate(0, 0, daysAgo)
	end := start.Add(time.Duration(dur) * time.Second)
	top.Rank, top.MaxScore = 1, 10
	seq := []string{}
	for _, s := range sigs {
		seq = append(seq, s.Rule+"@"+s.Service)
	}
	r := Record{Key: key, Simulated: true, Method: MethodVersion, Title: title, Severity: sev, TriggerRule: trigger,
		StartedAt: start, Primary: primary, Affected: affected, Signals: sigs, Edges: edges, Sequence: seq,
		Hypotheses: []Hypothesis{top}, KnownFaults: []string{}, Interventions: ivs, Notes: []string{SeedNote},
		Recovery: Recovery{Status: "resolved", ResolvedAt: &end, DurationSec: dur, Cause: cause}, TraceIDs: []string{},
		Limitations: []string{SeedNote}}
	r.Features = Fingerprint(r)
	return r
}

// Seeds returns the simulated demonstration records. Keys start with SIM- so
// they can never be mistaken for a real INC- key.
func Seeds() []Record {
	pay := []Signal{
		{Rule: "dependency_degradation", Service: "checkout", Peer: "payment", Severity: "critical", Unit: "ratio", Peak: 0.52, Baseline: 0},
		{Rule: "retry_anomaly", Service: "checkout", Peer: "payment", Severity: "major", Unit: "ratio", Peak: 0.4, Baseline: 0}}
	return []Record{
		seedRecord("SIM-0001", "Payment latency raised checkout timeouts", "critical", "dependency_degradation", "checkout",
			[]string{"checkout", "gateway", "payment"}, pay, []string{"checkout>payment"},
			Hypothesis{Kind: "latency_origin", Service: "payment", Title: "payment: its own processing became slower", Score: 9,
				Supporting: []string{"payment self-time p95 rose about 20× (simulated figure)"}, Contradicting: []string{}},
			[]Intervention{
				{Source: "replay", Class: "simulated", Name: "Restore payment latency to baseline", Detail: "restore payment self-time to baseline", Effect: "improved", Summary: "error rate 27.0% → 0.0%, p95 2500 ms → 80 ms (simulated)"},
				{Source: "replay", Class: "simulated", Name: "Single payment attempt", Detail: "checkout→payment: 1 attempts", Effect: "mixed", Summary: "error rate 27.0% → 31.0%, p95 2500 ms → 900 ms (simulated)"}},
			"a known injected fault ended before resolution (simulated)", 150, 0),
		seedRecord("SIM-0002", "Payment returned HTTP 500 for a share of charges", "major", "dependency_degradation", "payment",
			[]string{"checkout", "payment"}, []Signal{
				{Rule: "error_rate", Service: "payment", Severity: "major", Unit: "ratio", Peak: 0.4},
				{Rule: "dependency_degradation", Service: "checkout", Peer: "payment", Severity: "major", Unit: "ratio", Peak: 0.2}},
			[]string{"checkout>payment"},
			Hypothesis{Kind: "error_origin", Service: "payment", Title: "payment: it originates errors", Score: 8,
				Supporting: []string{"payment origin-error rate rose (simulated figure)"}, Contradicting: []string{}},
			[]Intervention{{Source: "replay", Class: "simulated", Name: "Remove payment errors", Detail: "remove payment's own errors", Effect: "improved", Summary: "error rate 18.0% → 0.0%, p95 90 ms → 88 ms (simulated)"}},
			"resolved with no recorded cause", 120, 3),
		seedRecord("SIM-0003", "Inventory slowdown on the product listing path", "major", "latency_anomaly", "inventory",
			[]string{"gateway", "inventory"}, []Signal{{Rule: "latency_anomaly", Service: "inventory", Severity: "major", Unit: "ms", Peak: 640, Baseline: 40}},
			[]string{"gateway>inventory"},
			Hypothesis{Kind: "latency_origin", Service: "inventory", Title: "inventory: its own processing became slower", Score: 8,
				Supporting: []string{"inventory self-time p95 rose (simulated figure)"}, Contradicting: []string{"checkout was unaffected (simulated)"}},
			[]Intervention{{Source: "replay", Class: "simulated", Name: "Restore inventory latency to baseline", Detail: "restore inventory self-time to baseline", Effect: "improved", Summary: "error rate 0.0% → 0.0%, p95 700 ms → 60 ms (simulated)"}},
			"resolved with no recorded cause", 200, 6),
		seedRecord("SIM-0004", "Notification service unavailable", "minor", "service_unavailable", "notification",
			[]string{"checkout", "notification"}, []Signal{{Rule: "service_unavailable", Service: "notification", Severity: "major", Unit: "ratio", Peak: 1}},
			[]string{"checkout>notification"},
			Hypothesis{Kind: "unavailable", Service: "notification", Title: "notification: unavailable", Score: 7,
				Supporting: []string{"all calls to notification failed (simulated figure)"}, Contradicting: []string{}},
			[]Intervention{}, "a known injected fault ended before resolution (simulated)", 90, 9),
	}
}
