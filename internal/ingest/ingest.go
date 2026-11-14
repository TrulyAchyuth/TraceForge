// Package ingest implements the OTLP/HTTP trace receiver and the bounded
// write pipeline between the receiver and storage.
package ingest

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/otlp"
)

// Writer persists span batches. *store.Store implements it.
type Writer interface {
	WriteSpans(ctx context.Context, spans []model.Span) (int, error)
}

// Config tunes the receiver and write pipeline.
type Config struct {
	// MaxBodyBytes caps the request body as sent on the wire.
	MaxBodyBytes int64
	// MaxDecodedBytes caps the body after gzip decompression.
	MaxDecodedBytes int64
	// MaxQueuedSpans bounds memory: requests are refused with 429 once this
	// many spans are waiting to be written.
	MaxQueuedSpans int
	// BatchSize is the largest number of spans written in one transaction.
	BatchSize int
	// FlushInterval is the longest a span waits before being written.
	FlushInterval time.Duration
	// WriteRetries is how many times a failed batch write is retried.
	WriteRetries int
	// Token, when non-empty, must be presented as a bearer token.
	Token  string
	Limits otlp.Limits
}

// DefaultConfig returns the configuration used when nothing is overridden.
func DefaultConfig() Config {
	return Config{
		MaxBodyBytes:    4 << 20,
		MaxDecodedBytes: 16 << 20,
		MaxQueuedSpans:  50000,
		BatchSize:       2000,
		FlushInterval:   250 * time.Millisecond,
		WriteRetries:    5,
		Limits:          otlp.DefaultLimits(),
	}
}

// Stats is a snapshot of ingestion counters since process start.
type Stats struct {
	Requests         int64          `json:"requests"`
	RequestsRejected int64          `json:"requests_rejected"`
	RequestsThrottle int64          `json:"requests_throttled"`
	SpansReceived    int64          `json:"spans_received"`
	SpansAccepted    int64          `json:"spans_accepted"`
	SpansRejected    int64          `json:"spans_rejected"`
	SpansWritten     int64          `json:"spans_written"`
	SpansDuplicate   int64          `json:"spans_duplicate"`
	SpansDropped     int64          `json:"spans_dropped"`
	WriteErrors      int64          `json:"write_errors"`
	QueueDepth       int64          `json:"queue_depth"`
	QueueCapacity    int            `json:"queue_capacity"`
	RejectReasons    map[string]int `json:"reject_reasons"`
	SpansPerSecond   float64        `json:"spans_per_second_1m"`
	LastIngestAt     *time.Time     `json:"last_ingest_at,omitempty"`
	LastError        string         `json:"last_error,omitempty"`
	LastErrorAt      *time.Time     `json:"last_error_at,omitempty"`
	StartedAt        time.Time      `json:"started_at"`
}

// rateWindowSeconds is the span of the spans-per-second moving average.
const rateWindowSeconds = 60

// Pipeline receives OTLP trace exports and writes them to storage.
type Pipeline struct {
	cfg    Config
	writer Writer
	log    *slog.Logger
	queue  chan []model.Span
	queued atomic.Int64

	requests, reqRejected, reqThrottled      atomic.Int64
	received, accepted, rejected             atomic.Int64
	written, duplicate, dropped, writeErrors atomic.Int64

	mu          sync.Mutex
	reasons     map[string]int
	lastIngest  time.Time
	lastErr     string
	lastErrAt   time.Time
	rateBuckets [rateWindowSeconds]int64
	rateStamps  [rateWindowSeconds]int64
	started     time.Time
	now         func() time.Time
}

// queueSlots is the channel capacity in batches. Memory is bounded by
// MaxQueuedSpans, not by this; it only needs to exceed the number of requests
// that can be admitted under that bound in practice.
const queueSlots = 4096

// New creates a pipeline. Call Run to start the writer.
func New(cfg Config, w Writer, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg: cfg, writer: w, log: log,
		queue:   make(chan []model.Span, queueSlots),
		reasons: map[string]int{},
		started: time.Now().UTC(),
		now:     time.Now,
	}
}

// Run writes queued spans until ctx is cancelled, then drains what is queued.
func (p *Pipeline) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()
	var pending []model.Span
	flush := func(c context.Context) {
		for len(pending) > 0 {
			n := min(len(pending), p.cfg.BatchSize)
			p.writeBatch(c, pending[:n])
			pending = pending[n:]
		}
		pending = nil
	}
	for {
		select {
		case batch := <-p.queue:
			pending = append(pending, batch...)
			if len(pending) >= p.cfg.BatchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-ctx.Done():
			// Drain on a fresh context so accepted spans are not lost on shutdown.
			drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for {
				select {
				case batch := <-p.queue:
					pending = append(pending, batch...)
				default:
					flush(drainCtx)
					return
				}
			}
		}
	}
}

func (p *Pipeline) writeBatch(ctx context.Context, batch []model.Span) {
	defer p.queued.Add(-int64(len(batch)))
	backoff := 200 * time.Millisecond
	for attempt := 0; ; attempt++ {
		n, err := p.writer.WriteSpans(ctx, batch)
		if err == nil {
			p.written.Add(int64(n))
			p.duplicate.Add(int64(len(batch) - n))
			return
		}
		p.writeErrors.Add(1)
		p.recordError("write spans: " + err.Error())
		if attempt >= p.cfg.WriteRetries || ctx.Err() != nil {
			p.dropped.Add(int64(len(batch)))
			p.log.Error("dropping span batch after failed writes", "spans", len(batch), "attempts", attempt+1, "error", err)
			return
		}
		p.log.Warn("span batch write failed; retrying", "attempt", attempt+1, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (p *Pipeline) recordError(msg string) {
	p.mu.Lock()
	p.lastErr, p.lastErrAt = msg, p.now().UTC()
	p.mu.Unlock()
}

// Enqueue admits spans to the write queue. It returns false, admitting
// nothing, when the queue is at capacity.
func (p *Pipeline) Enqueue(spans []model.Span) bool {
	if len(spans) == 0 {
		return true
	}
	n := int64(len(spans))
	if p.queued.Add(n) > int64(p.cfg.MaxQueuedSpans) {
		p.queued.Add(-n)
		return false
	}
	select {
	case p.queue <- spans:
	default:
		p.queued.Add(-n)
		return false
	}
	now := p.now()
	sec := now.Unix()
	p.mu.Lock()
	p.lastIngest = now.UTC()
	i := sec % rateWindowSeconds
	if p.rateStamps[i] != sec {
		p.rateStamps[i], p.rateBuckets[i] = sec, 0
	}
	p.rateBuckets[i] += n
	p.mu.Unlock()
	return true
}

// Stats returns a snapshot of the pipeline counters.
func (p *Pipeline) Stats() Stats {
	s := Stats{
		Requests: p.requests.Load(), RequestsRejected: p.reqRejected.Load(), RequestsThrottle: p.reqThrottled.Load(),
		SpansReceived: p.received.Load(), SpansAccepted: p.accepted.Load(), SpansRejected: p.rejected.Load(),
		SpansWritten: p.written.Load(), SpansDuplicate: p.duplicate.Load(), SpansDropped: p.dropped.Load(),
		WriteErrors: p.writeErrors.Load(), QueueDepth: p.queued.Load(), QueueCapacity: p.cfg.MaxQueuedSpans,
		RejectReasons: map[string]int{}, StartedAt: p.started,
	}
	now := p.now().Unix()
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.reasons {
		s.RejectReasons[k] = v
	}
	var sum int64
	for i, stamp := range p.rateStamps {
		if now-stamp < rateWindowSeconds {
			sum += p.rateBuckets[i]
		}
	}
	s.SpansPerSecond = float64(sum) / rateWindowSeconds
	if !p.lastIngest.IsZero() {
		t := p.lastIngest
		s.LastIngestAt = &t
	}
	if p.lastErr != "" {
		t := p.lastErrAt
		s.LastError, s.LastErrorAt = p.lastErr, &t
	}
	return s
}

// gRPC status codes used in OTLP/HTTP error bodies.
const (
	codeInvalidArgument   = 3
	codeResourceExhausted = 8
	codeUnauthenticated   = 16
)

// ServeHTTP implements the OTLP/HTTP traces endpoint (POST /v1/traces).
func (p *Pipeline) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.requests.Add(1)
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != otlp.ContentProtobuf && contentType != otlp.ContentJSON {
		p.reqRejected.Add(1)
		http.Error(w, "unsupported content type; use application/x-protobuf or application/json", http.StatusUnsupportedMediaType)
		return
	}
	if r.Method != http.MethodPost {
		p.reqRejected.Add(1)
		w.Header().Set("Allow", http.MethodPost)
		p.fail(w, contentType, http.StatusMethodNotAllowed, codeInvalidArgument, "method not allowed")
		return
	}
	if p.cfg.Token != "" && !validBearer(r.Header.Get("Authorization"), p.cfg.Token) {
		p.reqRejected.Add(1)
		p.fail(w, contentType, http.StatusUnauthorized, codeUnauthenticated, "missing or invalid ingest token")
		return
	}
	body, status, err := p.readBody(w, r)
	if err != nil {
		p.reqRejected.Add(1)
		p.recordError(err.Error())
		p.fail(w, contentType, status, codeInvalidArgument, err.Error())
		return
	}
	req, err := otlp.Unmarshal(contentType, body)
	if err != nil {
		p.reqRejected.Add(1)
		p.recordError(err.Error())
		p.fail(w, contentType, http.StatusBadRequest, codeInvalidArgument, "malformed OTLP trace request")
		return
	}
	res := otlp.Convert(req, p.cfg.Limits, p.now())
	rejected := res.RejectedCount()
	p.received.Add(int64(len(res.Spans) + rejected))

	if !p.Enqueue(res.Spans) {
		p.reqThrottled.Add(1)
		w.Header().Set("Retry-After", "1")
		p.fail(w, contentType, http.StatusTooManyRequests, codeResourceExhausted, "ingest queue is full; retry later")
		return
	}
	p.accepted.Add(int64(len(res.Spans)))
	if rejected > 0 {
		p.rejected.Add(int64(rejected))
		p.mu.Lock()
		for reason, n := range res.Rejected {
			p.reasons[reason] += n
		}
		p.mu.Unlock()
	}

	resp := &collectortrace.ExportTraceServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &collectortrace.ExportTracePartialSuccess{
			RejectedSpans: int64(rejected),
			ErrorMessage:  "some spans failed validation (invalid IDs or timestamps)",
		}
	}
	p.respond(w, contentType, http.StatusOK, resp)
}

func validBearer(header, token string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	return constantTimeEqual(header[len(prefix):], token)
}

func (p *Pipeline) readBody(w http.ResponseWriter, r *http.Request) ([]byte, int, error) {
	var reader io.Reader = http.MaxBytesReader(w, r.Body, p.cfg.MaxBodyBytes)
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, http.StatusBadRequest, errors.New("invalid gzip body")
		}
		defer gz.Close()
		reader = gz
	default:
		return nil, http.StatusUnsupportedMediaType, errors.New("unsupported content encoding")
	}
	// Read one byte past the limit to detect, rather than silently truncate,
	// an oversized (possibly decompression-bomb) payload.
	body, err := io.ReadAll(io.LimitReader(reader, p.cfg.MaxDecodedBytes+1))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge, errors.New("request body too large")
		}
		return nil, http.StatusBadRequest, errors.New("unreadable request body")
	}
	if int64(len(body)) > p.cfg.MaxDecodedBytes {
		return nil, http.StatusRequestEntityTooLarge, errors.New("decompressed request body too large")
	}
	return body, 0, nil
}

func (p *Pipeline) fail(w http.ResponseWriter, contentType string, httpStatus int, code int32, msg string) {
	p.respond(w, contentType, httpStatus, &statuspb.Status{Code: code, Message: msg})
}

func (p *Pipeline) respond(w http.ResponseWriter, contentType string, httpStatus int, msg proto.Message) {
	var body []byte
	var err error
	if contentType == otlp.ContentJSON {
		body, err = protojson.Marshal(msg)
	} else {
		body, err = proto.Marshal(msg)
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(httpStatus)
	if _, err := w.Write(body); err != nil {
		p.log.Debug("write OTLP response", "error", err)
	}
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
