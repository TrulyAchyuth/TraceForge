package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/fixture"
	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/replay"
)

var now = time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)

func view(t *testing.T, title string) (*investigate.View, []model.ReplayRun) {
	t.Helper()
	ds := fixture.PaymentSlowdown()
	an := investigate.Analyze(ds, nil)
	res, err := replay.Run(ds, replay.Scenario{Name: "restore payment", Seed: 7,
		Interventions: []replay.Intervention{{Type: replay.TypeScaleLatency, Service: "payment", Mode: replay.ModeBaseline}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	resolved := ds.IncidentStart.Add(5 * time.Minute)
	return &investigate.View{
		Incident: model.Incident{ID: 1, Key: "INC-0001", Title: title, Severity: "critical", Status: "resolved", PrimaryService: "checkout", StartedAt: ds.IncidentStart, ResolvedAt: &resolved},
		Dataset:  ds, Analysis: &an, Hypotheses: an.Hypotheses, Methodology: investigate.Methodology(), IntegrityOK: true,
	}, []model.ReplayRun{{ID: 1, IncidentID: 1, Name: "restore payment", Seed: 7, Result: raw}}
}

func TestReportContainsEvidenceReplayAndLimits(t *testing.T) {
	v, runs := view(t, "Checkout degraded")
	r, err := Build(v, runs, now)
	if err != nil {
		t.Fatal(err)
	}
	md, err := r.Markdown()
	if err != nil {
		t.Fatal(err)
	}
	s := string(md)
	for _, want := range []string{"INC-0001", "Checkout degraded", "payment", "restore payment", "Similar past incidents", "Limitations"} {
		if !strings.Contains(s, want) {
			t.Errorf("markdown report lacks %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(s), "simulated") {
		t.Error("counterfactual results must be labelled simulated in the report")
	}
	if len(r.Limitations) == 0 || len(r.Recommendations) == 0 || len(r.ExecutiveSummary) == 0 {
		t.Error("a report must state limitations, recommendations and a summary")
	}
	if r.DurationText != "5m0s" {
		t.Errorf("duration = %q", r.DurationText)
	}
}

func TestHTMLReportEscapesIncidentContent(t *testing.T) {
	v, _ := view(t, `<script>alert(1)</script>`)
	r, _ := Build(v, nil, now)
	html, err := r.HTML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(html), "<script>alert(1)</script>") {
		t.Fatal("incident titles are untrusted input and must be escaped in HTML reports")
	}
	if !strings.Contains(string(html), "&lt;script&gt;") {
		t.Error("expected the escaped form of the title")
	}
}

func TestJSONReportIsValidAndStable(t *testing.T) {
	v, runs := view(t, "t")
	a, _ := Build(v, runs, now)
	b, _ := Build(v, runs, now)
	ja, err := a.JSON()
	if err != nil {
		t.Fatal(err)
	}
	jb, _ := b.JSON()
	if string(ja) != string(jb) {
		t.Error("the same inputs and time must produce the same report")
	}
	var back map[string]any
	if err := json.Unmarshal(ja, &back); err != nil {
		t.Fatalf("report JSON does not parse: %v", err)
	}
}

func TestBuildFailsOnCorruptReplayResult(t *testing.T) {
	v, _ := view(t, "t")
	if _, err := Build(v, []model.ReplayRun{{ID: 9, Result: json.RawMessage(`{not json`)}}, now); err == nil {
		t.Fatal("a corrupt stored replay must be reported, not silently dropped from the report")
	}
}

func TestReportWithoutAnalysisOrReplays(t *testing.T) {
	v, _ := view(t, "t")
	v.Analysis, v.Hypotheses, v.Dataset = nil, nil, nil
	r, err := Build(v, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Markdown(); err != nil {
		t.Errorf("markdown with no data: %v", err)
	}
	if _, err := r.HTML(); err != nil {
		t.Errorf("html with no data: %v", err)
	}
}
