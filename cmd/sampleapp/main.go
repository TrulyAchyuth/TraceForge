// Command sampleapp is the instrumented demo application bundled with
// TraceForge. One binary plays every role, selected with ROLE:
//
//	gateway       GET /api/checkout → checkout;  GET /api/products → inventory
//	checkout      POST /checkout → inventory, payment (with retries), notification
//	payment       POST /charge
//	inventory     POST /reserve, GET /stock
//	notification  POST /notify
//	loadgen       sends traffic to the gateway
//
// The services do no real work: each handler sleeps for a short, randomised
// "processing" time. What is real is the telemetry: every request produces
// genuine OpenTelemetry spans exported over OTLP. Each service also exposes
// /admin/fault so the TraceForge failure lab can inject latency, errors or
// unavailability. Those endpoints are meant for an isolated demo network only.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// fault is the fault document accepted by /admin/fault. It mirrors lab.Fault.
type fault struct {
	LatencyMs        float64  `json:"latency_ms,omitempty"`
	JitterMs         float64  `json:"jitter_ms,omitempty"`
	ErrorRate        float64  `json:"error_rate,omitempty"`
	Unavailable      bool     `json:"unavailable,omitempty"`
	RetryMaxAttempts int      `json:"retry_max_attempts,omitempty"`
	RetryBackoffMs   *float64 `json:"retry_backoff_ms,omitempty"`
	RateMultiplier   float64  `json:"rate_multiplier,omitempty"`
	TTLSeconds       int      `json:"ttl_seconds"`
	Seed             int64    `json:"seed"`
}

// maxFaultTTL bounds how long any injected fault can last.
const maxFaultTTL = 15 * time.Minute

// faultState holds the active fault. Faults always expire on their own.
type faultState struct {
	mu      sync.Mutex
	active  *fault
	expires time.Time
	rng     *rand.Rand
}

func (f *faultState) current() *fault {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != nil && time.Now().After(f.expires) {
		f.active = nil
	}
	return f.active
}

// sample returns the extra latency and whether to fail, for one request.
func (f *faultState) sample() (delay time.Duration, fail, unavailable bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == nil || time.Now().After(f.expires) {
		f.active = nil
		return 0, false, false
	}
	a := f.active
	ms := a.LatencyMs
	if a.JitterMs > 0 {
		ms += (f.rng.Float64()*2 - 1) * a.JitterMs
	}
	if ms < 0 {
		ms = 0
	}
	return time.Duration(ms * float64(time.Millisecond)), a.ErrorRate > 0 && f.rng.Float64() < a.ErrorRate, a.Unavailable
}

func (f *faultState) set(ft fault) error {
	ttl := time.Duration(ft.TTLSeconds) * time.Second
	switch {
	case ttl <= 0 || ttl > maxFaultTTL:
		return fmt.Errorf("ttl_seconds must be between 1 and %d", int(maxFaultTTL.Seconds()))
	case ft.LatencyMs < 0 || ft.LatencyMs > 10000 || ft.JitterMs < 0:
		return errors.New("latency_ms must be 0–10000 and jitter_ms non-negative")
	case ft.ErrorRate < 0 || ft.ErrorRate > 1:
		return errors.New("error_rate must be between 0 and 1")
	case ft.RetryMaxAttempts < 0 || ft.RetryMaxAttempts > 10:
		return errors.New("retry_max_attempts must be between 0 and 10")
	case ft.RateMultiplier < 0 || ft.RateMultiplier > 20:
		return errors.New("rate_multiplier must be between 0 and 20")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active, f.expires = &ft, time.Now().Add(ttl)
	f.rng = rand.New(rand.NewPCG(uint64(ft.Seed), 0x7f4a7c15))
	return nil
}

func (f *faultState) clear() {
	f.mu.Lock()
	f.active = nil
	f.mu.Unlock()
}

func (f *faultState) handler(w http.ResponseWriter, r *http.Request) {
	// ADMIN_TOKEN, when set, protects fault injection and load control.
	if tok := os.Getenv("ADMIN_TOKEN"); tok != "" &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(tok)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		var ft fault
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ft); err != nil {
			http.Error(w, "invalid fault document: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := f.set(ft); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	case http.MethodDelete:
		f.clear()
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	out := map[string]any{"active": false}
	if f.active != nil && time.Now().Before(f.expires) {
		out = map[string]any{"active": true, "fault": f.active, "expires_at": f.expires.UTC()}
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// app is one running role.
type app struct {
	role   string
	log    *slog.Logger
	fault  *faultState
	tracer trace.Tracer
	client *http.Client
	rngMu  sync.Mutex
	rng    *rand.Rand
	urls   map[string]string
}

// work sleeps for a randomised simulated processing time. The sleep is not
// cancelled when the caller gives up: like a database call already in
// flight, the work completes even if nobody is waiting for the answer.
func (a *app) work(minMs, maxMs float64) {
	a.rngMu.Lock()
	ms := minMs + a.rng.Float64()*(maxMs-minMs)
	a.rngMu.Unlock()
	time.Sleep(time.Duration(ms * float64(time.Millisecond)))
}

// instrument wraps a business handler with unavailability injection (outside
// tracing, so a "down" service emits no spans), server tracing, and
// latency/error injection (inside tracing, so spans show it).
func (a *app) instrument(operation string, h http.HandlerFunc) http.Handler {
	traced := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delay, fail, _ := a.fault.sample()
		if delay > 0 {
			time.Sleep(delay)
		}
		if fail {
			span := trace.SpanFromContext(r.Context())
			span.SetAttributes(attribute.String("error.type", "injected_fault"))
			http.Error(w, "injected fault", http.StatusInternalServerError)
			return
		}
		h(w, r)
	}), operation)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ft := a.fault.current(); ft != nil && ft.Unavailable {
			// Drop the connection without a response, as a dead process would.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		traced.ServeHTTP(w, r)
	})
}

// errStatus is returned by call for a non-2xx response.
type errStatus int

func (e errStatus) Error() string { return "unexpected HTTP status " + strconv.Itoa(int(e)) }

// call performs one traced HTTP request to another service. It records a
// CLIENT span with HTTP semantic-convention attributes, including the retry
// ordinal (http.request.resend_count) and error.type on failure.
func (a *app) call(ctx context.Context, peer, method, path string, timeout time.Duration, resend int) error {
	target := a.urls[peer] + path
	ctx, span := a.tracer.Start(ctx, method, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("http.request.method", method),
		attribute.String("url.full", target),
		attribute.String("peer.service", peer),
		attribute.String("server.address", peer),
	))
	defer span.End()
	if resend > 0 {
		span.SetAttributes(attribute.Int("http.request.resend_count", resend))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := a.client.Do(req)
	if err != nil {
		errType := "connection_error"
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			errType = "timeout"
		}
		span.SetAttributes(attribute.String("error.type", errType))
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetAttributes(attribute.String("error.type", strconv.Itoa(resp.StatusCode)))
		span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(resp.StatusCode))
		return errStatus(resp.StatusCode)
	}
	return nil
}

// Checkout's default policy for calling payment.
const (
	paymentAttempts = 3
	paymentTimeout  = 800 * time.Millisecond
	paymentBackoff  = 50 * time.Millisecond
	defaultTimeout  = 5 * time.Second
	gatewayTimeout  = 15 * time.Second
)

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *app) routes(mux *http.ServeMux) {
	switch a.role {
	case "gateway":
		mux.Handle("GET /api/checkout", a.instrument("GET /api/checkout", func(w http.ResponseWriter, r *http.Request) {
			a.work(1, 3)
			if err := a.call(r.Context(), "checkout", http.MethodPost, "/checkout", gatewayTimeout, 0); err != nil {
				respond(w, http.StatusBadGateway, map[string]string{"error": "checkout failed"})
				return
			}
			respond(w, http.StatusOK, map[string]string{"status": "order placed"})
		}))
		mux.Handle("GET /api/products", a.instrument("GET /api/products", func(w http.ResponseWriter, r *http.Request) {
			a.work(1, 3)
			if err := a.call(r.Context(), "inventory", http.MethodGet, "/stock", defaultTimeout, 0); err != nil {
				respond(w, http.StatusBadGateway, map[string]string{"error": "inventory failed"})
				return
			}
			respond(w, http.StatusOK, map[string]any{"products": []string{"anvil", "tongs", "bellows"}})
		}))
	case "checkout":
		mux.Handle("POST /checkout", a.instrument("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			a.work(2, 5)
			if err := a.call(ctx, "inventory", http.MethodPost, "/reserve", defaultTimeout, 0); err != nil {
				respond(w, http.StatusBadGateway, map[string]string{"error": "reservation failed"})
				return
			}
			attempts, backoff := paymentAttempts, paymentBackoff
			if ft := a.fault.current(); ft != nil {
				if ft.RetryMaxAttempts > 0 {
					attempts = ft.RetryMaxAttempts
				}
				if ft.RetryBackoffMs != nil {
					backoff = time.Duration(*ft.RetryBackoffMs * float64(time.Millisecond))
				}
			}
			var err error
			for i := 0; i < attempts; i++ {
				if err = a.call(ctx, "payment", http.MethodPost, "/charge", paymentTimeout, i); err == nil {
					break
				}
				if i < attempts-1 && backoff > 0 {
					time.Sleep(backoff)
				}
			}
			if err != nil {
				respond(w, http.StatusBadGateway, map[string]string{"error": "payment failed"})
				return
			}
			// A failed notification does not fail the order.
			if err := a.call(ctx, "notification", http.MethodPost, "/notify", defaultTimeout, 0); err != nil {
				trace.SpanFromContext(ctx).AddEvent("notification failed; order still placed")
			}
			a.work(1, 2)
			respond(w, http.StatusOK, map[string]string{"status": "ok"})
		}))
	case "payment":
		mux.Handle("POST /charge", a.instrument("POST /charge", func(w http.ResponseWriter, _ *http.Request) {
			a.work(30, 50)
			respond(w, http.StatusOK, map[string]string{"status": "charged"})
		}))
	case "inventory":
		mux.Handle("POST /reserve", a.instrument("POST /reserve", func(w http.ResponseWriter, _ *http.Request) {
			a.work(8, 15)
			respond(w, http.StatusOK, map[string]string{"status": "reserved"})
		}))
		mux.Handle("GET /stock", a.instrument("GET /stock", func(w http.ResponseWriter, _ *http.Request) {
			a.work(5, 10)
			respond(w, http.StatusOK, map[string]int{"anvil": 12, "tongs": 40, "bellows": 7})
		}))
	case "notification":
		mux.Handle("POST /notify", a.instrument("POST /notify", func(w http.ResponseWriter, _ *http.Request) {
			a.work(4, 8)
			respond(w, http.StatusOK, map[string]string{"status": "sent"})
		}))
	}
}

// Load generator settings.
const (
	maxInFlight   = 256
	checkoutShare = 0.7
)

// runLoad sends requests to the gateway at baseRPS × the active multiplier.
func (a *app) runLoad(ctx context.Context, baseRPS float64) {
	var inFlight atomic.Int64
	gateway := a.urls["gateway"]
	for ctx.Err() == nil {
		rate := baseRPS
		if ft := a.fault.current(); ft != nil && ft.RateMultiplier > 0 {
			rate *= ft.RateMultiplier
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(float64(time.Second) / rate)):
		}
		if inFlight.Load() >= maxInFlight {
			continue // shed load rather than queue without bound
		}
		a.rngMu.Lock()
		path := "/api/products"
		if a.rng.Float64() < checkoutShare {
			path = "/api/checkout"
		}
		a.rngMu.Unlock()
		inFlight.Add(1)
		go func() {
			defer inFlight.Add(-1)
			reqCtx, cancel := context.WithTimeout(ctx, gatewayTimeout+5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, gateway+path, nil)
			if err != nil {
				return
			}
			resp, err := a.client.Do(req)
			if err != nil {
				return // the gateway's own telemetry, or its absence, tells the story
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
		}()
	}
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("sampleapp failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	role := os.Getenv("ROLE")
	roles := map[string]bool{"gateway": true, "checkout": true, "payment": true, "inventory": true, "notification": true, "loadgen": true}
	if !roles[role] {
		return fmt.Errorf("ROLE must be one of gateway, checkout, payment, inventory, notification, loadgen (got %q)", role)
	}
	log = log.With("role", role)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	seed, err := strconv.ParseInt(env("SAMPLE_SEED", "1"), 10, 64)
	if err != nil {
		return errors.New("SAMPLE_SEED must be an integer")
	}
	h := fnv.New64a()
	h.Write([]byte(role))
	a := &app{role: role, log: log, fault: &faultState{},
		rng:    rand.New(rand.NewPCG(uint64(seed), h.Sum64())),
		client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 30 * time.Second}},
		urls: map[string]string{
			"gateway":      env("GATEWAY_URL", "http://gateway:8080"),
			"checkout":     env("CHECKOUT_URL", "http://checkout:8080"),
			"payment":      env("PAYMENT_URL", "http://payment:8080"),
			"inventory":    env("INVENTORY_URL", "http://inventory:8080"),
			"notification": env("NOTIFICATION_URL", "http://notification:8080"),
		}}
	for name, raw := range a.urls {
		if u, err := url.Parse(raw); err != nil || u.Host == "" {
			return fmt.Errorf("invalid URL for %s: %q", name, raw)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	if role == "loadgen" {
		rps, err := strconv.ParseFloat(env("LOAD_RPS", "8"), 64)
		if err != nil || rps <= 0 || rps > 500 {
			return errors.New("LOAD_RPS must be a number between 0 and 500")
		}
		mux.HandleFunc("/admin/load", a.fault.handler)
		go a.runLoad(ctx, rps)
		log.Info("load generator started", "rps", rps, "target", a.urls["gateway"])
	} else {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return fmt.Errorf("create OTLP exporter: %w", err)
		}
		res, err := resource.New(ctx, resource.WithAttributes(
			attribute.String("service.name", role),
			attribute.String("service.namespace", "traceforge-sample"),
			attribute.String("deployment.environment.name", "demo"),
		))
		if err != nil {
			return fmt.Errorf("create resource: %w", err)
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res),
			sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(time.Second)))
		defer func() {
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tp.Shutdown(flushCtx); err != nil {
				log.Warn("flush spans on shutdown", "error", err)
			}
		}()
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.TraceContext{})
		a.tracer = tp.Tracer("github.com/TrulyAchyuth/TraceForge/cmd/sampleapp")
		mux.HandleFunc("/admin/fault", a.fault.handler)
		a.routes(mux)
	}

	srv := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
