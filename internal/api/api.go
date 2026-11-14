// Package api implements TraceForge's versioned REST API and serves the
// web interface.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/detect"
	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/ingest"
	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/lab"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

// Version is the application version, overridable at build time with
// -ldflags "-X github.com/TrulyAchyuth/TraceForge/internal/api.Version=...".
var Version = "0.1.0"

// Options configures the API server.
type Options struct {
	Store          *store.Store
	Pipeline       *ingest.Pipeline
	Engine         *detect.Engine
	Investigations *investigate.Service
	Lab            *lab.Manager
	Log            *slog.Logger
	// Token, when set, is required as a bearer token on every /api request.
	Token          string
	IngestToken    bool
	WebDir         string
	RetentionHours int
	DetectInterval time.Duration
}

// Server is the HTTP API.
type Server struct {
	opt     Options
	started time.Time
	now     func() time.Time
}

// New creates the API server.
func New(opt Options) *Server {
	return &Server{opt: opt, started: time.Now().UTC(), now: time.Now}
}

// maxJSONBody bounds request bodies accepted by the API.
const maxJSONBody = 1 << 20

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/system", s.handleSystem)
	api.HandleFunc("GET /api/v1/overview", s.handleOverview)
	api.HandleFunc("GET /api/v1/services", s.handleServices)
	api.HandleFunc("GET /api/v1/services/{name}", s.handleService)
	api.HandleFunc("GET /api/v1/dependencies", s.handleDependencies)
	api.HandleFunc("GET /api/v1/traces", s.handleTraces)
	api.HandleFunc("GET /api/v1/traces/{id}", s.handleTrace)
	api.HandleFunc("GET /api/v1/ingest/stats", s.handleIngestStats)
	api.HandleFunc("GET /api/v1/incidents", s.handleIncidents)
	api.HandleFunc("GET /api/v1/incidents/{id}", s.handleIncident)
	api.HandleFunc("PATCH /api/v1/incidents/{id}", s.handleIncidentUpdate)
	api.HandleFunc("POST /api/v1/incidents/{id}/notes", s.handleNote)
	api.HandleFunc("GET /api/v1/incidents/{id}/investigation", s.handleInvestigation)
	api.HandleFunc("POST /api/v1/incidents/{id}/investigation", s.handleInvestigationRebuild)
	api.HandleFunc("GET /api/v1/incidents/{id}/replays", s.handleReplays)
	api.HandleFunc("POST /api/v1/incidents/{id}/replays", s.handleReplayRun)
	api.HandleFunc("GET /api/v1/replays/{id}", s.handleReplay)
	api.HandleFunc("GET /api/v1/incidents/{id}/report", s.handleReport)
	api.HandleFunc("GET /api/v1/timeline", s.handleTimeline)
	api.HandleFunc("GET /api/v1/lab", s.handleLab)
	api.HandleFunc("POST /api/v1/lab/experiments", s.handleLabStart)
	api.HandleFunc("POST /api/v1/lab/experiments/{id}/stop", s.handleLabStop)
	api.HandleFunc("POST /api/v1/lab/reset", s.handleLabReset)
	api.HandleFunc("GET /api/v1/settings/detection", s.handleDetectionGet)
	api.HandleFunc("PUT /api/v1/settings/detection", s.handleDetectionPut)
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
	})
	mux.Handle("/api/", s.authenticate(api))
	mux.Handle("/", s.static())
	return s.recoverer(s.logRequests(securityHeaders(mux)))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opt.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.opt.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="traceforge"`)
				writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API token is required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := requestID()
		w.Header().Set("X-Request-Id", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelDebug
		if rec.status >= 500 {
			level = slog.LevelError
		} else if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet {
			level = slog.LevelInfo
		}
		s.opt.Log.Log(r.Context(), level, "http request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds(), "request_id", id)
	})
}

func requestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.opt.Log.Error("panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// static serves the built web interface with single-page-app fallback.
func (s *Server) static() http.Handler {
	dir := s.opt.WebDir
	index := filepath.Join(dir, "index.html")
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if _, err := os.Stat(index); err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "TraceForge API is running. The web interface has not been built (web/dist is missing).\n")
			return
		}
		clean := filepath.Clean("/" + r.URL.Path)
		if info, err := os.Stat(filepath.Join(dir, clean)); err == nil && !info.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, index)
	})
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The status line is already sent; an encode failure here can only be a
	// broken connection, which the client observes on its own.
	_ = json.NewEncoder(w).Encode(v)
}

// fail maps an internal error to a response. Storage errors are logged and
// reported generically so internals never leak to clients.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "the requested resource does not exist")
	case errors.Is(err, context.Canceled):
		// The client went away; there is nobody to answer.
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "the request took too long")
	default:
		s.opt.Log.Error("request failed", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the request could not be completed; storage may be unavailable")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain a single JSON document")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "identifier must be a positive integer")
		return 0, false
	}
	return id, true
}

// Supported relative time ranges.
var ranges = map[string]time.Duration{
	"5m": 5 * time.Minute, "15m": 15 * time.Minute, "30m": 30 * time.Minute, "1h": time.Hour,
	"3h": 3 * time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour, "24h": 24 * time.Hour,
}

// maxRange is the longest window a query may span.
const maxRange = 7 * 24 * time.Hour

// timeRange is a resolved query window.
type timeRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// StepSeconds is the series resolution chosen for this window.
	StepSeconds int `json:"step_seconds"`
}

// targetPoints is the number of series points aimed for in a chart.
const targetPoints = 90

// parseRange resolves ?range= or ?from=&to= (RFC 3339). The default is 15m.
func (s *Server) parseRange(r *http.Request) (timeRange, error) {
	q := r.URL.Query()
	now := s.now().UTC()
	tr := timeRange{To: now}
	if from, to := q.Get("from"), q.Get("to"); from != "" || to != "" {
		var err error
		if tr.From, err = time.Parse(time.RFC3339, from); err != nil {
			return tr, errors.New("from must be an RFC 3339 timestamp")
		}
		if to != "" {
			if tr.To, err = time.Parse(time.RFC3339, to); err != nil {
				return tr, errors.New("to must be an RFC 3339 timestamp")
			}
		}
	} else {
		name := q.Get("range")
		if name == "" {
			name = "15m"
		}
		d, ok := ranges[name]
		if !ok {
			return tr, errors.New("range must be one of 5m, 15m, 30m, 1h, 3h, 6h, 12h, 24h")
		}
		tr.From = now.Add(-d)
	}
	tr.From, tr.To = tr.From.UTC(), tr.To.UTC()
	if !tr.To.After(tr.From) {
		return tr, errors.New("to must be after from")
	}
	if tr.To.Sub(tr.From) > maxRange {
		return tr, errors.New("time range must not exceed 7 days")
	}
	step := int(tr.To.Sub(tr.From).Seconds()) / targetPoints
	step = (step + hist.BucketSeconds - 1) / hist.BucketSeconds * hist.BucketSeconds
	tr.StepSeconds = max(step, hist.BucketSeconds)
	return tr, nil
}

func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, lo, hi)
	}
	return v, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.opt.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "database unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
