package tree

import (
	"testing"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/fixture"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func sp(trace, id, parent string, startMs, durMs int64) model.DatasetSpan {
	return model.DatasetSpan{TraceID: trace, SpanID: id, ParentSpanID: parent, Start: t0.Add(time.Duration(startMs) * time.Millisecond), DurationNs: durMs * 1e6}
}

func TestBuildMarksBrokenTracesIncomplete(t *testing.T) {
	cases := map[string][]model.DatasetSpan{
		"orphan (parent missing)": {sp("t", "a", "", 0, 10), sp("t", "b", "gone", 1, 1)},
		"two roots":               {sp("t", "a", "", 0, 10), sp("t", "b", "", 1, 1)},
		"parent cycle":            {sp("t", "r", "", 0, 10), sp("t", "x", "y", 1, 1), sp("t", "y", "x", 1, 1)},
		"self parent":             {sp("t", "r", "", 0, 10), sp("t", "x", "x", 1, 1)},
		"no root":                 {sp("t", "x", "y", 1, 1), sp("t", "y", "x", 1, 1)},
	}
	for name, spans := range cases {
		got := Build(spans)
		if len(got) != 1 || got[0].Complete {
			t.Errorf("%s: must be reported incomplete", name)
		}
	}
	ok := Build([]model.DatasetSpan{sp("t", "a", "", 0, 10), sp("t", "b", "a", 2, 3), sp("t", "c", "a", 1, 1)})
	if !ok[0].Complete || len(ok[0].Root.Children) != 2 || ok[0].Root.Children[0].Span.SpanID != "c" {
		t.Error("children must be ordered by start time")
	}
	if got := Build(nil); len(got) != 0 {
		t.Error("no spans, no traces")
	}
}

func TestBuildIsOrderIndependent(t *testing.T) {
	spans := fixture.PaymentSlowdown().Spans
	rev := make([]model.DatasetSpan, len(spans))
	for i := range spans {
		rev[len(spans)-1-i] = spans[i]
	}
	a, b := Build(spans), Build(rev)
	if len(a) != len(b) {
		t.Fatal("trace count differs")
	}
	for i := range a {
		if a[i].ID != b[i].ID || len(a[i].Nodes) != len(b[i].Nodes) || a[i].Complete != b[i].Complete {
			t.Fatalf("trace %d differs by input order", i)
		}
	}
}

func TestRetryGroupsAreFormedFromResendCounts(t *testing.T) {
	var retried, clean int
	for _, tr := range Build(fixture.PaymentSlowdown().Spans) {
		if !tr.Complete {
			t.Fatal("fixture traces must be complete")
		}
		if tr.Root.Span.Name != "POST /checkout" && tr.Root.Retries() == 0 {
			clean++
			continue
		}
		if tr.Root.Retries() > 0 {
			retried++
		} else {
			clean++
		}
	}
	if retried == 0 || clean == 0 {
		t.Errorf("the slowdown fixture has both retried and unretried requests, got retried=%d clean=%d", retried, clean)
	}
	// Hand-built: two attempts with rising resend counts form one group; a repeat with the same count does not.
	a, b, c := sp("t", "a", "", 0, 100), sp("t", "b", "a", 1, 10), sp("t", "c", "a", 20, 10)
	b.Kind, c.Kind, b.Name, c.Name = model.KindClient, model.KindClient, "POST", "POST"
	b.PeerService, c.PeerService = "payment", "payment"
	c.ResendCount = 1
	if got := Build([]model.DatasetSpan{a, b, c})[0].Root.Retries(); got != 1 {
		t.Errorf("want 1 retry, got %d", got)
	}
	c.ResendCount = 0
	if got := Build([]model.DatasetSpan{a, b, c})[0].Root.Retries(); got != 0 {
		t.Errorf("two separate calls are not a retry, got %d", got)
	}
}
