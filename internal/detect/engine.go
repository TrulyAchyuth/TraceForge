package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

// SettingsKey is the settings-table key that stores the detection Config.
const SettingsKey = "detection"

// Service health states.
const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthCritical = "critical"
	HealthIdle     = "idle"
)

// Investigator builds (or rebuilds) the investigation for an incident.
type Investigator interface {
	Build(ctx context.Context, incidentID int64, reason string) error
}

const (
	// evalLagBuckets delays evaluation so that in-flight spans, collector
	// batching and edge resolution have settled for the evaluated window.
	evalLagBuckets = 2
	// baselineLookback is how far back the engine searches for
	// incident-free buckets to form a baseline.
	baselineLookback = time.Hour
	// incidentPad widens incident intervals excluded from baselines, covering
	// onset before detection and the tail after recovery.
	incidentPad = 60 * time.Second
	// rebuildMinInterval rate-limits investigation rebuilds on new signals.
	rebuildMinInterval = 45 * time.Second
	// exemplarLimit is the number of exemplar traces stored per signal.
	exemplarLimit = 5
)

// Engine periodically evaluates the rules and maintains incidents.
type Engine struct {
	store    *store.Store
	log      *slog.Logger
	inv      Investigator
	interval time.Duration
	now      func() time.Time

	mu        sync.Mutex
	cfg       Config
	known     map[string]struct{}
	lastBuild map[int64]time.Time
	lastTick  time.Time
	lastErr   string
	builds    sync.WaitGroup
}

// NewEngine creates a detection engine.
func NewEngine(st *store.Store, inv Investigator, interval time.Duration, log *slog.Logger) *Engine {
	return &Engine{store: st, inv: inv, interval: interval, log: log, now: time.Now, cfg: DefaultConfig(),
		known: map[string]struct{}{}, lastBuild: map[int64]time.Time{}}
}

// SetClock overrides the engine's clock. It exists for tests.
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

// LoadConfig reads the stored configuration, keeping defaults if none exists.
func (e *Engine) LoadConfig(ctx context.Context) error {
	cfg := DefaultConfig()
	found, err := e.store.GetSetting(ctx, SettingsKey, &cfg)
	if err != nil {
		return err
	}
	if found {
		if err := cfg.Validate(); err != nil {
			e.log.Warn("stored detection config is invalid; using defaults", "error", err)
			cfg = DefaultConfig()
		}
	}
	e.mu.Lock()
	e.cfg = cfg
	e.mu.Unlock()
	return nil
}

// Config returns the active configuration.
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// SetConfig validates, persists and activates a configuration.
func (e *Engine) SetConfig(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := e.store.PutSetting(ctx, SettingsKey, cfg); err != nil {
		return err
	}
	e.mu.Lock()
	e.cfg = cfg
	e.mu.Unlock()
	return nil
}

// Status reports when the engine last ran and its last error, if any.
func (e *Engine) Status() (lastTick time.Time, lastErr string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastTick, e.lastErr
}

// Run evaluates on every interval until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.builds.Wait()
			return
		case <-ticker.C:
			err := e.Tick(ctx)
			e.mu.Lock()
			e.lastTick = e.now().UTC()
			if err != nil {
				e.lastErr = err.Error()
			} else {
				e.lastErr = ""
			}
			e.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				e.log.Error("detection tick failed", "error", err)
			}
		}
	}
}

// Wait blocks until background investigation builds have finished.
func (e *Engine) Wait() { e.builds.Wait() }

// Window describes the evaluation and baseline buckets chosen for a tick.
type Window struct {
	Start, End      time.Time
	BaselineBuckets []time.Time
}

// SelectWindow picks the evaluation window ending evalLagBuckets before now
// and the most recent incident-free baseline buckets before it. excluded are
// incident intervals; dataStart is the earliest bucket holding any telemetry.
func SelectWindow(cfg Config, now, dataStart time.Time, excluded [][2]time.Time) Window {
	end := hist.TruncateBucket(now).Add(-evalLagBuckets * hist.BucketDuration)
	w := Window{Start: end.Add(-time.Duration(cfg.WindowSeconds) * time.Second), End: end}
	want := cfg.BaselineSeconds / hist.BucketSeconds
	floor := w.Start.Add(-baselineLookback)
	if dataStart.After(floor) {
		floor = dataStart
	}
	for b := w.Start.Add(-hist.BucketDuration); !b.Before(floor) && len(w.BaselineBuckets) < want; b = b.Add(-hist.BucketDuration) {
		bEnd := b.Add(hist.BucketDuration)
		clean := true
		for _, iv := range excluded {
			if bEnd.After(iv[0].Add(-incidentPad)) && b.Before(iv[1].Add(incidentPad)) {
				clean = false
				break
			}
		}
		if clean {
			w.BaselineBuckets = append(w.BaselineBuckets, b)
		}
	}
	return w
}

// BuildInput aggregates rollup rows into rule inputs for the given window.
func BuildInput(w Window, svc []model.StatBucket, edges []model.EdgeBucket) Input {
	baseline := map[int64]struct{}{}
	for _, b := range w.BaselineBuckets {
		baseline[b.Unix()] = struct{}{}
	}
	windowSeconds := w.End.Sub(w.Start).Seconds()
	baselineSeconds := float64(len(w.BaselineBuckets) * hist.BucketSeconds)

	services := map[string]*ServiceInput{}
	for _, b := range svc {
		si := services[b.Service]
		if si == nil {
			si = &ServiceInput{Name: b.Service,
				Window:   Agg{Hist: hist.New(), Seconds: windowSeconds},
				Baseline: Agg{Hist: hist.New(), Seconds: baselineSeconds}}
			services[b.Service] = si
		}
		switch {
		case !b.Bucket.Before(w.Start) && b.Bucket.Before(w.End):
			si.Window.Requests += b.Requests
			si.Window.Errors += b.Errors
			hist.Add(si.Window.Hist, b.Hist)
		default:
			if _, ok := baseline[b.Bucket.Unix()]; ok {
				si.Baseline.Requests += b.Requests
				si.Baseline.Errors += b.Errors
				hist.Add(si.Baseline.Hist, b.Hist)
				if b.Requests > 0 {
					si.BaselineBucketP95 = append(si.BaselineBucketP95, hist.Quantile(b.Hist, 0.95))
				}
			}
		}
	}
	type edgeKey struct{ caller, callee string }
	edgeMap := map[edgeKey]*EdgeInput{}
	for _, b := range edges {
		k := edgeKey{b.Caller, b.Callee}
		ei := edgeMap[k]
		if ei == nil {
			ei = &EdgeInput{Caller: b.Caller, Callee: b.Callee, Window: EdgeAgg{Hist: hist.New()}, Baseline: EdgeAgg{Hist: hist.New()}}
			edgeMap[k] = ei
		}
		var target *EdgeAgg
		if !b.Bucket.Before(w.Start) && b.Bucket.Before(w.End) {
			target = &ei.Window
		} else if _, ok := baseline[b.Bucket.Unix()]; ok {
			target = &ei.Baseline
		}
		if target != nil {
			target.Calls += b.Calls
			target.Errors += b.Errors
			target.Retries += b.Retries
			hist.Add(target.Hist, b.Hist)
		}
	}
	// A callee that is down reports nothing, so make sure every edge endpoint
	// exists as a service input (with empty aggregates).
	for k := range edgeMap {
		for _, name := range []string{k.caller, k.callee} {
			if services[name] == nil {
				services[name] = &ServiceInput{Name: name,
					Window:   Agg{Hist: hist.New(), Seconds: windowSeconds},
					Baseline: Agg{Hist: hist.New(), Seconds: baselineSeconds}}
			}
		}
	}
	in := Input{}
	for _, s := range services {
		in.Services = append(in.Services, *s)
	}
	sort.Slice(in.Services, func(i, j int) bool { return in.Services[i].Name < in.Services[j].Name })
	for _, e := range edgeMap {
		in.Edges = append(in.Edges, *e)
	}
	sort.Slice(in.Edges, func(i, j int) bool {
		if in.Edges[i].Caller != in.Edges[j].Caller {
			return in.Edges[i].Caller < in.Edges[j].Caller
		}
		return in.Edges[i].Callee < in.Edges[j].Callee
	})
	return in
}

// Tick runs one detection pass: evaluate rules, attach signals to incidents,
// auto-resolve quiet incidents and record service health transitions.
func (e *Engine) Tick(ctx context.Context) error {
	cfg := e.Config()
	now := e.now().UTC()
	end := hist.TruncateBucket(now).Add(-evalLagBuckets * hist.BucketDuration)
	from := end.Add(-time.Duration(cfg.WindowSeconds)*time.Second - baselineLookback)

	svc, err := e.store.ServiceStats(ctx, from, end)
	if err != nil {
		return fmt.Errorf("load service stats: %w", err)
	}
	edges, err := e.store.EdgeStats(ctx, from, end)
	if err != nil {
		return fmt.Errorf("load edge stats: %w", err)
	}
	intervals, err := e.store.IncidentIntervals(ctx, from, now)
	if err != nil {
		return fmt.Errorf("load incident intervals: %w", err)
	}
	dataStart := end
	if len(svc) > 0 {
		dataStart = svc[0].Bucket
	}
	w := SelectWindow(cfg, now, dataStart, intervals)
	in := BuildInput(w, svc, edges)
	signals := Evaluate(cfg, in)

	touched, err := e.applySignals(ctx, now, w, in, signals)
	if err != nil {
		return err
	}
	if err := e.store.DeactivateStaleSignals(ctx, now.Add(-5*e.interval/2)); err != nil {
		return fmt.Errorf("deactivate signals: %w", err)
	}
	if err := e.autoResolve(ctx, now, cfg, touched); err != nil {
		return err
	}
	return e.updateHealth(ctx, now, in, signals)
}

func signalKey(incidentID int64, sg model.Signal) string {
	return fmt.Sprintf("%d|%s|%s|%s", incidentID, sg.Rule, sg.Service, sg.Peer)
}

func signalTitle(sg model.Signal) string {
	switch sg.Rule {
	case RuleErrorRate:
		return "Elevated error rate on " + sg.Service
	case RuleLatency:
		return "Latency anomaly on " + sg.Service
	case RuleTraffic:
		if sg.Value >= sg.Baseline {
			return "Traffic spike on " + sg.Service
		}
		return "Traffic drop on " + sg.Service
	case RuleDependency:
		return fmt.Sprintf("Dependency degradation: %s → %s", sg.Service, sg.Peer)
	case RuleUnavailable:
		return sg.Service + " unavailable"
	case RuleRetries:
		return fmt.Sprintf("Abnormal retries: %s → %s", sg.Service, sg.Peer)
	}
	return "Anomaly on " + sg.Service
}

// applySignals attaches each firing signal to a related active incident or
// opens a new one. It returns the IDs of incidents that received a signal.
func (e *Engine) applySignals(ctx context.Context, now time.Time, w Window, in Input, signals []model.Signal) (map[int64]bool, error) {
	touched := map[int64]bool{}
	if len(signals) == 0 {
		return touched, nil
	}
	active, err := e.store.ActiveIncidents(ctx)
	if err != nil {
		return nil, fmt.Errorf("load active incidents: %w", err)
	}
	neighbours := map[string]map[string]bool{}
	link := func(a, b string) {
		if neighbours[a] == nil {
			neighbours[a] = map[string]bool{}
		}
		neighbours[a][b] = true
	}
	for _, ed := range in.Edges {
		link(ed.Caller, ed.Callee)
		link(ed.Callee, ed.Caller)
	}
	related := func(inc *model.Incident, sg model.Signal) bool {
		for _, a := range inc.AffectedServices {
			if a == sg.Service || (sg.Peer != "" && a == sg.Peer) || neighbours[sg.Service][a] {
				return true
			}
		}
		return false
	}

	rebuild := map[int64]string{}
	for _, sg := range signals {
		var inc *model.Incident
		for i := range active {
			if related(&active[i], sg) {
				inc = &active[i]
				break
			}
		}
		if inc == nil {
			affected := []string{sg.Service}
			created, err := e.store.CreateIncident(ctx, model.Incident{
				Title: signalTitle(sg), Severity: sg.Severity, Status: model.IncidentOpen, PrimaryService: sg.Service,
				AffectedServices: affected, TriggerRule: sg.Rule, StartedAt: w.Start, DetectedAt: now, LastSignalAt: now,
			})
			if err != nil {
				return nil, fmt.Errorf("create incident: %w", err)
			}
			active = append(active, created)
			inc = &active[len(active)-1]
			e.event(ctx, model.Event{Time: now, Kind: model.EventDetection, Severity: sg.Severity, IncidentID: &inc.ID,
				Service: sg.Service, Title: fmt.Sprintf("%s opened: %s", inc.Key, inc.Title),
				Detail: mustJSON(map[string]any{"rule": sg.Rule, "message": sg.Message, "window_start": w.Start, "window_end": w.End})})
			e.log.Info("incident opened", "incident", inc.Key, "rule", sg.Rule, "service", sg.Service, "severity", sg.Severity)
			rebuild[inc.ID] = "incident detected"
		}

		key := signalKey(inc.ID, sg)
		e.mu.Lock()
		_, seen := e.known[key]
		e.known[key] = struct{}{}
		e.mu.Unlock()
		sg.IncidentID, sg.LastSeen = inc.ID, now
		if !seen {
			sg.Exemplars = e.exemplars(ctx, sg, w)
		}
		isNew, err := e.store.UpsertSignal(ctx, sg)
		if err != nil {
			return nil, fmt.Errorf("record signal: %w", err)
		}
		if isNew {
			trace := ""
			if len(sg.Exemplars) > 0 {
				trace = sg.Exemplars[0]
			}
			e.event(ctx, model.Event{Time: now, Kind: model.EventSignal, Severity: sg.Severity, IncidentID: &inc.ID,
				Service: sg.Service, TraceID: trace, Title: sg.Message,
				Detail: mustJSON(map[string]any{"rule": sg.Rule, "peer": sg.Peer, "value": sg.Value, "baseline": sg.Baseline,
					"threshold": sg.Threshold, "unit": sg.Unit})})
			if _, queued := rebuild[inc.ID]; !queued {
				rebuild[inc.ID] = "new signal: " + sg.Rule + " on " + sg.Service
			}
		}

		severity := inc.Severity
		if model.SeverityRank(sg.Severity) > model.SeverityRank(severity) {
			severity = sg.Severity
			e.event(ctx, model.Event{Time: now, Kind: model.EventDetection, Severity: severity, IncidentID: &inc.ID,
				Service: sg.Service, Title: fmt.Sprintf("%s escalated to %s", inc.Key, severity)})
		}
		affected := inc.AffectedServices
		for _, name := range []string{sg.Service, sg.Peer} {
			if name != "" && !slices.Contains(affected, name) {
				affected = append(affected, name)
			}
		}
		if err := e.store.UpdateIncidentDetection(ctx, inc.ID, severity, affected, now); err != nil {
			return nil, fmt.Errorf("update incident: %w", err)
		}
		inc.Severity, inc.AffectedServices, inc.LastSignalAt = severity, affected, now
		touched[inc.ID] = true
	}
	for id, reason := range rebuild {
		e.mu.Lock()
		last, built := e.lastBuild[id]
		due := !built || now.Sub(last) >= rebuildMinInterval
		if due {
			e.lastBuild[id] = now
		}
		e.mu.Unlock()
		if due {
			e.buildAsync(ctx, id, reason)
		}
	}
	return touched, nil
}

func (e *Engine) exemplars(ctx context.Context, sg model.Signal, w Window) []string {
	var ids []string
	var err error
	switch sg.Rule {
	case RuleErrorRate:
		ids, err = e.store.ExemplarTraces(ctx, sg.Service, w.Start, w.End, true, exemplarLimit)
	case RuleLatency:
		ids, err = e.store.ExemplarTraces(ctx, sg.Service, w.Start, w.End, false, exemplarLimit)
	case RuleDependency, RuleRetries:
		ids, err = e.store.ExemplarEdgeTraces(ctx, sg.Service, sg.Peer, w.Start, w.End, exemplarLimit)
	case RuleUnavailable:
		ids, err = e.store.ExemplarEdgeTraces(ctx, sg.Peer, sg.Service, w.Start, w.End, exemplarLimit)
	}
	if err != nil {
		e.log.Warn("exemplar lookup failed", "rule", sg.Rule, "service", sg.Service, "error", err)
	}
	return ids
}

func (e *Engine) autoResolve(ctx context.Context, now time.Time, cfg Config, touched map[int64]bool) error {
	active, err := e.store.ActiveIncidents(ctx)
	if err != nil {
		return fmt.Errorf("load active incidents: %w", err)
	}
	quiet := time.Duration(cfg.ResolveAfterSeconds) * time.Second
	for _, inc := range active {
		if touched[inc.ID] || now.Sub(inc.LastSignalAt) < quiet {
			continue
		}
		if err := e.store.SetIncidentStatus(ctx, inc.ID, model.IncidentResolved, now); err != nil {
			return fmt.Errorf("resolve incident: %w", err)
		}
		id := inc.ID
		e.event(ctx, model.Event{Time: now, Kind: model.EventRecovery, IncidentID: &id, Service: inc.PrimaryService,
			Title:  fmt.Sprintf("%s auto-resolved: no rule has fired for %ds", inc.Key, cfg.ResolveAfterSeconds),
			Detail: mustJSON(map[string]any{"last_signal_at": inc.LastSignalAt, "previous_status": inc.Status})})
		e.log.Info("incident auto-resolved", "incident", inc.Key)
		e.mu.Lock()
		e.lastBuild[id] = now
		e.mu.Unlock()
		e.buildAsync(ctx, id, "incident resolved")
	}
	return nil
}

func (e *Engine) buildAsync(ctx context.Context, id int64, reason string) {
	if e.inv == nil {
		return
	}
	e.builds.Add(1)
	go func() {
		defer e.builds.Done()
		if err := e.inv.Build(ctx, id, reason); err != nil && ctx.Err() == nil {
			e.log.Error("investigation build failed", "incident_id", id, "error", err)
		}
	}()
}

// HealthFor derives each service's health from the signals firing on it.
func HealthFor(in Input, signals []model.Signal) map[string]string {
	out := map[string]string{}
	for _, s := range in.Services {
		if s.Window.Requests > 0 {
			out[s.Name] = HealthHealthy
		} else {
			out[s.Name] = HealthIdle
		}
	}
	for _, sg := range signals {
		status := HealthDegraded
		if sg.Severity == model.SevCritical {
			status = HealthCritical
		}
		if cur := out[sg.Service]; cur != HealthCritical {
			out[sg.Service] = status
		}
	}
	return out
}

func (e *Engine) updateHealth(ctx context.Context, now time.Time, in Input, signals []model.Signal) error {
	stored, err := e.store.ListServiceHealth(ctx)
	if err != nil {
		return fmt.Errorf("load service health: %w", err)
	}
	prev := map[string]store.ServiceHealth{}
	for _, h := range stored {
		prev[h.Service] = h
	}
	health := HealthFor(in, signals)
	names := make([]string, 0, len(health))
	for name := range health {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		status := health[name]
		old, known := prev[name]
		lastSeen := old.LastSeen
		if status != HealthIdle || !known {
			lastSeen = now
		}
		if known && old.Status == status && now.Sub(old.LastSeen) < time.Minute && status != HealthIdle {
			// Unchanged and recently refreshed: skip the write.
			continue
		}
		if err := e.store.SetServiceHealth(ctx, name, status, now, lastSeen); err != nil {
			return fmt.Errorf("store service health: %w", err)
		}
		if known && old.Status != status {
			sev := ""
			switch status {
			case HealthCritical:
				sev = model.SevCritical
			case HealthDegraded:
				sev = model.SevWarning
			}
			e.event(ctx, model.Event{Time: now, Kind: model.EventServiceState, Severity: sev, Service: name,
				Title:  fmt.Sprintf("%s changed from %s to %s", name, old.Status, status),
				Detail: mustJSON(map[string]any{"from": old.Status, "to": status})})
		}
	}
	return nil
}

// event records a timeline event; failures are logged, never fatal, because
// the timeline is secondary to detection itself.
func (e *Engine) event(ctx context.Context, ev model.Event) {
	if _, err := e.store.AddEvent(ctx, ev); err != nil {
		e.log.Warn("record timeline event failed", "kind", ev.Kind, "error", err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
