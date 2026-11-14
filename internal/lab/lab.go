// Package lab implements the controlled failure laboratory. It injects
// faults only into an explicit allowlist of sample-application endpoints.
package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

// Fault is the fault configuration sent to one target. The same document is
// understood by every sample service; fields that do not apply are ignored.
type Fault struct {
	// LatencyMs is added to every request the service handles.
	LatencyMs float64 `json:"latency_ms,omitempty"`
	// JitterMs spreads the added latency uniformly over ±JitterMs.
	JitterMs float64 `json:"jitter_ms,omitempty"`
	// ErrorRate is the share of requests answered with HTTP 500.
	ErrorRate float64 `json:"error_rate,omitempty"`
	// Unavailable makes the service drop connections without responding.
	Unavailable bool `json:"unavailable,omitempty"`
	// RetryMaxAttempts overrides checkout's attempts per payment call.
	RetryMaxAttempts int `json:"retry_max_attempts,omitempty"`
	// RetryBackoffMs overrides checkout's pause between attempts.
	RetryBackoffMs *float64 `json:"retry_backoff_ms,omitempty"`
	// RateMultiplier scales the load generator's request rate.
	RateMultiplier float64 `json:"rate_multiplier,omitempty"`
	// TTLSeconds makes the target clear the fault by itself, so a fault
	// cannot outlive a crashed controller.
	TTLSeconds int `json:"ttl_seconds"`
	// Seed makes the target's random fault decisions reproducible.
	Seed int64 `json:"seed"`
}

// Scenario is a predefined experiment.
type Scenario struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Description     string           `json:"description"`
	Expected        string           `json:"expected"`
	Faults          map[string]Fault `json:"faults"`
	DefaultDuration int              `json:"default_duration_seconds"`
	Custom          bool             `json:"custom"`
}

// Duration bounds for an experiment, in seconds.
const (
	MinDuration = 20
	MaxDuration = 900
	// ttlMargin is how long after the planned end a target clears a fault on
	// its own if the controller never stops it.
	ttlMargin = 10
)

func backoff(ms float64) *float64 { return &ms }

// Scenarios returns the built-in experiments.
func Scenarios() []Scenario {
	return []Scenario{
		{ID: "payment-latency", Name: "Dependency slowdown: payment", DefaultDuration: 150,
			Description: "Adds 600–1200 ms to every payment request. Checkout allows 800 ms per attempt and tries three times.",
			Expected:    "Checkout latency rises, most payment attempts time out and are retried, and a share of checkouts fail. Product listing is unaffected.",
			Faults:      map[string]Fault{"payment": {LatencyMs: 900, JitterMs: 300}}},
		{ID: "payment-errors", Name: "Service errors: payment", DefaultDuration: 120,
			Description: "Payment answers 40% of requests with HTTP 500.",
			Expected:    "Payment error rate rises; checkout retries hide part of it and fail the rest.",
			Faults:      map[string]Fault{"payment": {ErrorRate: 0.4}}},
		{ID: "payment-outage", Name: "Service unavailability: payment", DefaultDuration: 120,
			Description: "Payment drops every connection without responding and emits no telemetry.",
			Expected:    "Every call to payment fails, payment reports no requests, and all checkouts fail.",
			Faults:      map[string]Fault{"payment": {Unavailable: true}}},
		{ID: "inventory-latency", Name: "Artificial latency: inventory", DefaultDuration: 120,
			Description: "Adds 350–450 ms to every inventory request.",
			Expected:    "Both checkout and product listing slow down, because both depend on inventory.",
			Faults:      map[string]Fault{"inventory": {LatencyMs: 400, JitterMs: 50}}},
		{ID: "retry-amplification", Name: "Retry amplification", DefaultDuration: 150,
			Description: "Payment fails 60% of requests while checkout is reconfigured to retry up to six times with no backoff.",
			Expected:    "The request rate into payment multiplies while few checkouts fail: retries mask errors at the cost of load.",
			Faults: map[string]Fault{"payment": {ErrorRate: 0.6},
				"checkout": {RetryMaxAttempts: 6, RetryBackoffMs: backoff(0)}}},
		{ID: "traffic-spike", Name: "Traffic spike", DefaultDuration: 120,
			Description: "The load generator sends five times its normal request rate.",
			Expected:    "Request rate rises on every service. Latency should hold, since the sample services have no capacity limit.",
			Faults:      map[string]Fault{"loadgen": {RateMultiplier: 5}}},
		{ID: "latency-recovery", Name: "Failure and recovery", DefaultDuration: 60,
			Description: "A short payment slowdown that ends on its own, to observe recovery and automatic incident resolution.",
			Expected:    "An incident opens, then auto-resolves about a minute after the fault ends.",
			Faults:      map[string]Fault{"payment": {LatencyMs: 900, JitterMs: 300}}},
		{ID: "custom", Name: "Custom fault", DefaultDuration: 120, Custom: true,
			Description: "Choose a service and the latency, error rate or unavailability to inject.",
			Expected:    "Depends on the fault chosen.",
			Faults:      map[string]Fault{}},
	}
}

// StartRequest starts an experiment.
type StartRequest struct {
	Scenario        string       `json:"scenario"`
	DurationSeconds int          `json:"duration_seconds"`
	Seed            int64        `json:"seed"`
	Custom          *CustomFault `json:"custom,omitempty"`
}

// CustomFault parameterises the "custom" scenario.
type CustomFault struct {
	Target      string  `json:"target"`
	LatencyMs   float64 `json:"latency_ms"`
	JitterMs    float64 `json:"jitter_ms"`
	ErrorRate   float64 `json:"error_rate"`
	Unavailable bool    `json:"unavailable"`
}

// Errors returned by the manager.
var (
	ErrDisabled = errors.New("failure lab is disabled: no lab targets are configured")
	ErrBusy     = errors.New("another experiment is already running; stop it first")
	ErrInvalid  = errors.New("invalid experiment")
)

// TargetStatus is the live fault state reported by one target.
type TargetStatus struct {
	Name      string          `json:"name"`
	Reachable bool            `json:"reachable"`
	Active    bool            `json:"active"`
	Fault     json.RawMessage `json:"fault,omitempty"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Manager runs experiments against the configured targets.
type Manager struct {
	store   *store.Store
	log     *slog.Logger
	targets map[string]string
	client  *http.Client
	now     func() time.Time
	mu      sync.Mutex // serialises start/stop so they cannot interleave
}

// ParseTargets parses "name=url,name=url" into an allowlist. Only http and
// https URLs with a host are accepted.
func ParseTargets(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, raw, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("lab target %q must be name=url", part)
		}
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("lab target %q has an invalid URL", name)
		}
		out[name] = strings.TrimRight(u.String(), "/")
	}
	return out, nil
}

// NewManager creates a lab manager for the given target allowlist.
func NewManager(st *store.Store, targets map[string]string, log *slog.Logger) *Manager {
	return &Manager{store: st, log: log, targets: targets, now: time.Now,
		client: &http.Client{Timeout: 3 * time.Second}}
}

// Enabled reports whether any target is configured.
func (m *Manager) Enabled() bool { return len(m.targets) > 0 }

// TargetNames lists the configured targets.
func (m *Manager) TargetNames() []string {
	names := make([]string, 0, len(m.targets))
	for n := range m.targets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (m *Manager) endpoint(target string) string {
	if target == "loadgen" {
		return m.targets[target] + "/admin/load"
	}
	return m.targets[target] + "/admin/fault"
}

func (m *Manager) send(ctx context.Context, method, target string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.endpoint(target), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s: HTTP %d: %s", target, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (m *Manager) plan(req StartRequest) (Scenario, map[string]Fault, error) {
	var sc Scenario
	found := false
	for _, s := range Scenarios() {
		if s.ID == req.Scenario {
			sc, found = s, true
		}
	}
	if !found {
		return sc, nil, fmt.Errorf("%w: unknown scenario %q", ErrInvalid, req.Scenario)
	}
	if req.DurationSeconds < MinDuration || req.DurationSeconds > MaxDuration {
		return sc, nil, fmt.Errorf("%w: duration_seconds must be between %d and %d", ErrInvalid, MinDuration, MaxDuration)
	}
	faults := sc.Faults
	if sc.Custom {
		c := req.Custom
		switch {
		case c == nil:
			return sc, nil, fmt.Errorf("%w: the custom scenario needs a fault definition", ErrInvalid)
		case c.Target == "loadgen":
			return sc, nil, fmt.Errorf("%w: custom faults target a service, not the load generator", ErrInvalid)
		case c.LatencyMs < 0 || c.LatencyMs > 10000 || c.JitterMs < 0 || c.JitterMs > c.LatencyMs:
			return sc, nil, fmt.Errorf("%w: latency_ms must be 0–10000 and jitter_ms between 0 and latency_ms", ErrInvalid)
		case c.ErrorRate < 0 || c.ErrorRate > 1:
			return sc, nil, fmt.Errorf("%w: error_rate must be between 0 and 1", ErrInvalid)
		case c.LatencyMs == 0 && c.ErrorRate == 0 && !c.Unavailable:
			return sc, nil, fmt.Errorf("%w: the custom fault does nothing; set latency, an error rate or unavailability", ErrInvalid)
		}
		faults = map[string]Fault{c.Target: {LatencyMs: c.LatencyMs, JitterMs: c.JitterMs, ErrorRate: c.ErrorRate, Unavailable: c.Unavailable}}
	}
	for target := range faults {
		if _, ok := m.targets[target]; !ok {
			return sc, nil, fmt.Errorf("%w: %q is not a configured lab target", ErrInvalid, target)
		}
	}
	return sc, faults, nil
}

// Start validates and begins an experiment, applying its faults to every
// target. If any target cannot be configured, faults already applied are
// cleared and nothing is recorded.
func (m *Manager) Start(ctx context.Context, req StartRequest) (model.Experiment, error) {
	if !m.Enabled() {
		return model.Experiment{}, ErrDisabled
	}
	sc, faults, err := m.plan(req)
	if err != nil {
		return model.Experiment{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	running, err := m.store.RunningExperiments(ctx)
	if err != nil {
		return model.Experiment{}, err
	}
	if len(running) > 0 {
		return model.Experiment{}, ErrBusy
	}

	targets := make([]string, 0, len(faults))
	for t := range faults {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	var applied []string
	for _, t := range targets {
		f := faults[t]
		f.TTLSeconds, f.Seed = req.DurationSeconds+ttlMargin, req.Seed
		faults[t] = f
		if _, err := m.send(ctx, http.MethodPut, t, f); err != nil {
			m.clear(context.WithoutCancel(ctx), applied)
			return model.Experiment{}, fmt.Errorf("apply fault: %w", err)
		}
		applied = append(applied, t)
	}

	now := m.now().UTC()
	params, _ := json.Marshal(map[string]any{"faults": faults, "duration_seconds": req.DurationSeconds})
	exp, err := m.store.CreateExperiment(ctx, model.Experiment{Scenario: sc.ID, Name: sc.Name, Params: params,
		Targets: targets, Seed: req.Seed, Status: model.ExperimentRunning, StartedAt: now,
		PlannedEnd: now.Add(time.Duration(req.DurationSeconds) * time.Second)})
	if err != nil {
		m.clear(context.WithoutCancel(ctx), applied)
		return model.Experiment{}, fmt.Errorf("record experiment: %w", err)
	}
	m.event(ctx, model.Event{Time: now, Kind: model.EventFaultInjected, ExperimentID: &exp.ID, Service: targets[0],
		Title:  fmt.Sprintf("Fault injected: %s on %s for %ds", sc.Name, strings.Join(targets, ", "), req.DurationSeconds),
		Detail: params})
	m.log.Info("experiment started", "id", exp.ID, "scenario", sc.ID, "targets", targets, "duration_s", req.DurationSeconds, "seed", req.Seed)
	return exp, nil
}

// clear removes faults from the given targets and returns the targets that
// could not be reached. Unreachable targets still clear themselves by TTL.
func (m *Manager) clear(ctx context.Context, targets []string) []string {
	var failed []string
	for _, t := range targets {
		if _, err := m.send(ctx, http.MethodDelete, t, nil); err != nil {
			m.log.Warn("clear fault failed; the target will clear it when its TTL expires", "target", t, "error", err)
			failed = append(failed, t)
		}
	}
	return failed
}

// Stop ends a running experiment and clears its faults.
func (m *Manager) Stop(ctx context.Context, id int64, reason string) (model.Experiment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stop(ctx, id, model.ExperimentStopped, reason)
}

func (m *Manager) stop(ctx context.Context, id int64, status, reason string) (model.Experiment, error) {
	exp, err := m.store.GetExperiment(ctx, id)
	if err != nil {
		return exp, err
	}
	if exp.Status != model.ExperimentRunning {
		return exp, nil
	}
	failed := m.clear(ctx, exp.Targets)
	if len(failed) > 0 {
		reason += fmt.Sprintf(" (could not reach %s; faults there expire by TTL)", strings.Join(failed, ", "))
	}
	now := m.now().UTC()
	changed, err := m.store.FinishExperiment(ctx, id, status, reason, now)
	if err != nil {
		return exp, err
	}
	if changed {
		m.event(ctx, model.Event{Time: now, Kind: model.EventFaultStopped, ExperimentID: &exp.ID, Service: exp.Targets[0],
			Title: fmt.Sprintf("Fault stopped: %s (%s)", exp.Name, reason)})
		m.log.Info("experiment ended", "id", id, "status", status, "reason", reason)
	}
	return m.store.GetExperiment(ctx, id)
}

// Reset clears faults on every configured target and ends any running
// experiment. It is the "make everything normal again" control.
func (m *Manager) Reset(ctx context.Context) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	running, err := m.store.RunningExperiments(ctx)
	if err != nil {
		return err
	}
	for _, exp := range running {
		if _, err := m.stop(ctx, exp.ID, model.ExperimentStopped, "reset by operator"); err != nil {
			return err
		}
	}
	if failed := m.clear(ctx, m.TargetNames()); len(failed) > 0 {
		return fmt.Errorf("could not reach %s; any fault there expires by TTL", strings.Join(failed, ", "))
	}
	return nil
}

// Status asks every target for its live fault state.
func (m *Manager) Status(ctx context.Context) []TargetStatus {
	names := m.TargetNames()
	out := make([]TargetStatus, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := TargetStatus{Name: name}
			data, err := m.send(ctx, http.MethodGet, name, nil)
			if err != nil {
				st.Error = "unreachable"
				out[i] = st
				return
			}
			var body struct {
				Active    bool            `json:"active"`
				Fault     json.RawMessage `json:"fault"`
				ExpiresAt *time.Time      `json:"expires_at"`
			}
			if err := json.Unmarshal(data, &body); err != nil {
				st.Error = "unexpected response"
				out[i] = st
				return
			}
			st.Reachable, st.Active, st.Fault, st.ExpiresAt = true, body.Active, body.Fault, body.ExpiresAt
			out[i] = st
		}()
	}
	wg.Wait()
	return out
}

// watchInterval is how often running experiments are checked for expiry.
const watchInterval = 2 * time.Second

// Run completes experiments when their planned duration elapses. Experiments
// left running by a previous process are picked up the same way.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			running, err := m.store.RunningExperiments(ctx)
			if err != nil {
				if ctx.Err() == nil {
					m.log.Warn("list running experiments failed", "error", err)
				}
				continue
			}
			for _, exp := range running {
				if m.now().Before(exp.PlannedEnd) {
					continue
				}
				m.mu.Lock()
				_, err := m.stop(ctx, exp.ID, model.ExperimentCompleted, "planned duration elapsed")
				m.mu.Unlock()
				if err != nil && ctx.Err() == nil {
					m.log.Warn("complete experiment failed", "id", exp.ID, "error", err)
				}
			}
		}
	}
}

func (m *Manager) event(ctx context.Context, ev model.Event) {
	if _, err := m.store.AddEvent(ctx, ev); err != nil {
		m.log.Warn("record lab event failed", "error", err)
	}
}
