package investigate

import (
	"reflect"
	"testing"

	"github.com/TrulyAchyuth/TraceForge/internal/fixture"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

func TestPaymentSlowdownRanksPaymentFirst(t *testing.T) {
	a := Analyze(fixture.PaymentSlowdown(), nil)
	if len(a.Hypotheses) < 2 {
		t.Fatalf("want several hypotheses, got %d", len(a.Hypotheses))
	}
	top := a.Hypotheses[0]
	if top.Kind != "latency_origin" || top.Service != "payment" || top.Rank != 1 {
		t.Fatalf("payment slowdown must be ranked first, got %s %s", top.Kind, top.Service)
	}
	for i, h := range a.Hypotheses {
		if h.Rank != i+1 {
			t.Errorf("ranks must be 1..n in order, hypothesis %d has rank %d", i, h.Rank)
		}
		if i > 0 && h.Score > a.Hypotheses[i-1].Score {
			t.Errorf("hypotheses must be sorted by score: %d > %d", h.Score, a.Hypotheses[i-1].Score)
		}
		if h.Score > h.MaxScore {
			t.Errorf("%s score %d exceeds max %d", h.Title, h.Score, h.MaxScore)
		}
	}
	// The same slow payment calls also explain checkout's retries; that must rank below the origin.
	for _, h := range a.Hypotheses[1:] {
		if h.Service == "payment" && h.Kind == "latency_origin" {
			t.Error("payment latency must appear once")
		}
	}
	if a.IncidentRequests != 120 || a.ImpactedRequests != 60 || a.TracesIncomplete != 0 {
		t.Errorf("request accounting: %d incident, %d impacted, %d incomplete", a.IncidentRequests, a.ImpactedRequests, a.TracesIncomplete)
	}
}

func TestEveryHypothesisSeparatesEvidenceClasses(t *testing.T) {
	for _, h := range Analyze(fixture.PaymentSlowdown(), nil).Hypotheses {
		if len(h.Evidence) == 0 {
			t.Errorf("%q has no evidence", h.Title)
		}
		for _, e := range h.Evidence {
			switch e.Class {
			case "observed", "correlation", "inference", "simulated":
			default:
				t.Errorf("%q: evidence %q has unknown class %q", h.Title, e.Check, e.Class)
			}
			if e.Text == "" {
				t.Errorf("%q: evidence %q has no explanation", h.Title, e.Check)
			}
		}
	}
}

func TestKnownFaultsDoNotChangeRanking(t *testing.T) {
	ds := fixture.PaymentSlowdown()
	plain := Analyze(ds, nil)
	withFault := Analyze(ds, []KnownFault{{ExperimentID: 1, Name: "wrong target", Targets: []string{"inventory"}}})
	strip := func(a Analysis) [][3]any {
		var out [][3]any
		for _, h := range a.Hypotheses {
			out = append(out, [3]any{h.Kind + h.Service, h.Score, h.Rank})
		}
		return out
	}
	if !reflect.DeepEqual(strip(plain), strip(withFault)) {
		t.Error("an injected-fault record is shown beside hypotheses and must never influence their score or rank")
	}
}

func TestAnalyzeIsDeterministic(t *testing.T) {
	first := Analyze(fixture.PaymentSlowdown(), nil)
	for i := 0; i < 10; i++ {
		again := Analyze(fixture.PaymentSlowdown(), nil)
		if !reflect.DeepEqual(first.Hypotheses, again.Hypotheses) {
			t.Fatal("analysis of identical data must be identical")
		}
	}
}

func TestEmptyAndBaselineOnlyDatasetsDoNotInventCauses(t *testing.T) {
	for name, ds := range map[string]*model.Dataset{"empty": {}, "baseline only": func() *model.Dataset {
		d := fixture.PaymentSlowdown()
		var keep []model.DatasetSpan
		for _, s := range d.Spans {
			if s.Phase == model.PhaseBaseline {
				keep = append(keep, s)
			}
		}
		d.Spans = keep
		return d
	}()} {
		a := Analyze(ds, nil)
		if a.ImpactedRequests != 0 {
			t.Errorf("%s: no incident traffic, yet %d impacted requests", name, a.ImpactedRequests)
		}
		for _, h := range a.Hypotheses {
			if h.Score > h.MaxScore/2 {
				t.Errorf("%s: %q scored %d/%d without any incident data", name, h.Title, h.Score, h.MaxScore)
			}
		}
	}
}

func TestMethodologyIsNonEmpty(t *testing.T) {
	if len(Methodology()) == 0 {
		t.Error("methodology must be reported with every investigation")
	}
}
