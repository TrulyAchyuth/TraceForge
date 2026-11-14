package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/otlp"
)

type fakeWriter struct {
	mu      sync.Mutex
	spans   []model.Span
	batches []int
	failN   int // fail this many calls first
	calls   int
}

func (f *fakeWriter) WriteSpans(_ context.Context, s []model.Span) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failN > 0 {
		f.failN--
		return 0, errors.New("db down")
	}
	f.spans = append(f.spans, s...)
	f.batches = append(f.batches, len(s))
	return len(s), nil
}

func (f *fakeWriter) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.spans) }

func newPipe(t *testing.T, cfg Config, w Writer) *Pipeline {
	t.Helper()
	return New(cfg, w, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func body(n int, bad int) []byte {
	var spans []*tracepb.Span
	now := time.Now()
	for i := 0; i < n; i++ {
		id := []byte{0, 0, 0, 0, 0, 0, byte(i >> 8), byte(i) + 1}
		tid := append(make([]byte, 8), id...)
		sp := &tracepb.Span{TraceId: tid, SpanId: id, Name: "op", StartTimeUnixNano: uint64(now.Add(-time.Second).UnixNano()), EndTimeUnixNano: uint64(now.UnixNano())}
		if i < bad {
			sp.TraceId = []byte("short")
		}
		spans = append(spans, sp)
	}
	b, _ := proto.Marshal(&collectortrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{Resource: &resourcepb.Resource{}, ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}})
	return b
}

func post(p *Pipeline, ct string, b []byte, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/traces", bytes.NewReader(b))
	r.Header.Set("Content-Type", ct)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func TestAcceptedSpansAreWrittenInBatches(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BatchSize, cfg.FlushInterval = 10, 10*time.Millisecond
	w := &fakeWriter{}
	p := newPipe(t, cfg, w)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	if rec := post(p, otlp.ContentProtobuf, body(25, 0), nil); rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	cancel()
	<-done
	if w.count() != 25 {
		t.Fatalf("shutdown must drain accepted spans: wrote %d of 25", w.count())
	}
	for _, b := range w.batches {
		if b > cfg.BatchSize {
			t.Errorf("batch of %d exceeds BatchSize %d", b, cfg.BatchSize)
		}
	}
	if s := p.Stats(); s.SpansAccepted != 25 || s.SpansWritten != 25 || s.QueueDepth != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestPartialSuccessCountsRejectedSpans(t *testing.T) {
	p := newPipe(t, DefaultConfig(), &fakeWriter{})
	rec := post(p, otlp.ContentProtobuf, body(5, 2), nil)
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	var resp collectortrace.ExportTraceServiceResponse
	if err := proto.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.GetPartialSuccess().GetRejectedSpans() != 2 {
		t.Fatalf("want partial success with 2 rejected, got %v (%v)", resp.GetPartialSuccess(), err)
	}
	if s := p.Stats(); s.SpansAccepted != 3 || s.SpansRejected != 2 || s.RejectReasons[otlp.RejectTraceID] != 2 {
		t.Errorf("stats %+v", s)
	}
}

func TestRequestValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Token = "ingest-secret"
	p := newPipe(t, cfg, &fakeWriter{})
	good := body(1, 0)
	auth := map[string]string{"Authorization": "Bearer ingest-secret"}
	cases := []struct {
		name string
		ct   string
		b    []byte
		h    map[string]string
		want int
	}{
		{"wrong content type", "text/plain", good, auth, 415},
		{"no token", otlp.ContentProtobuf, good, nil, 401},
		{"wrong token", otlp.ContentProtobuf, good, map[string]string{"Authorization": "Bearer nope"}, 401},
		{"token without scheme", otlp.ContentProtobuf, good, map[string]string{"Authorization": "ingest-secret"}, 401},
		{"garbage body", otlp.ContentProtobuf, []byte{0xff, 0xff, 0xff, 0xff}, auth, 400},
		{"unknown encoding", otlp.ContentProtobuf, good, map[string]string{"Authorization": "Bearer ingest-secret", "Content-Encoding": "br"}, 415},
		{"claims gzip, is not", otlp.ContentProtobuf, good, map[string]string{"Authorization": "Bearer ingest-secret", "Content-Encoding": "gzip"}, 400},
		{"ok", otlp.ContentProtobuf, good, auth, 200},
	}
	for _, c := range cases {
		if got := post(p, c.ct, c.b, c.h).Code; got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	r := httptest.NewRequest("GET", "/v1/traces", nil)
	r.Header.Set("Content-Type", otlp.ContentProtobuf)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Errorf("GET: got %d", w.Code)
	}
}

func TestGzipBombIsRefused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxDecodedBytes = 4 << 10
	p := newPipe(t, cfg, &fakeWriter{})
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(make([]byte, 1<<20)) // 1 MiB of zeros compresses to about 1 KiB
	_ = gz.Close()
	if buf.Len() >= 1<<16 {
		t.Skip("test payload did not compress enough")
	}
	if got := post(p, otlp.ContentProtobuf, buf.Bytes(), map[string]string{"Content-Encoding": "gzip"}).Code; got != 413 {
		t.Errorf("decompression bomb: got %d, want 413", got)
	}
	cfg = DefaultConfig()
	cfg.MaxBodyBytes = 100
	p = newPipe(t, cfg, &fakeWriter{})
	if got := post(p, otlp.ContentProtobuf, body(50, 0), nil).Code; got != 413 {
		t.Errorf("oversized body: got %d, want 413", got)
	}
}

func TestBackpressureReturns429AndAdmitsNothingPartially(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxQueuedSpans = 10
	p := newPipe(t, cfg, &fakeWriter{}) // Run is not started, so nothing drains
	if got := post(p, otlp.ContentProtobuf, body(8, 0), nil).Code; got != 200 {
		t.Fatalf("first request: %d", got)
	}
	rec := post(p, otlp.ContentProtobuf, body(8, 0), nil)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over capacity: got %d, Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if s := p.Stats(); s.QueueDepth != 8 || s.RequestsThrottle != 1 {
		t.Errorf("a refused request must not leak queue depth: %+v", s)
	}
}

func TestWriteFailureRetriesThenDrops(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FlushInterval, cfg.WriteRetries = 5*time.Millisecond, 1
	w := &fakeWriter{failN: 1}
	p := newPipe(t, cfg, w)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	post(p, otlp.ContentProtobuf, body(3, 0), nil)
	deadline := time.Now().Add(3 * time.Second)
	for w.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if w.count() != 3 {
		t.Fatalf("a transient write failure must be retried, wrote %d", w.count())
	}
	cancel()
	<-done
	if s := p.Stats(); s.WriteErrors != 1 || s.SpansDropped != 0 || s.LastError == "" {
		t.Errorf("stats %+v", s)
	}

	// A permanently failing database must drop (and count) spans rather than grow memory forever.
	cfg.WriteRetries = 0
	dead := &fakeWriter{failN: 1 << 30}
	p2 := newPipe(t, cfg, dead)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { p2.Run(ctx2); close(done2) }()
	post(p2, otlp.ContentProtobuf, body(4, 0), nil)
	deadline = time.Now().Add(3 * time.Second)
	for p2.Stats().SpansDropped < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel2()
	<-done2
	if s := p2.Stats(); s.SpansDropped != 4 || s.QueueDepth != 0 {
		t.Errorf("want 4 dropped and an empty queue, got %+v", s)
	}
}
