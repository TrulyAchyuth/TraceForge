package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/replay"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

// Capture sizing. Sampling keeps datasets small enough to replay
// interactively while leaving a few hundred requests per phase for
// percentile estimates.
const (
	baselineDuration   = 10 * time.Minute
	maxIncidentWindow  = 30 * time.Minute
	maxBaselineTraces  = 300
	maxIncidentTraces  = 600
	maxSpansPerTrace   = 300
	settleDelay        = 15 * time.Second // lets in-flight traces and rollups land
	relatedTracesLimit = 12
)

// Service builds and serves investigations.
type Service struct {
	store *store.Store
	log   *slog.Logger
	now   func() time.Time
}

// New creates an investigation service.
func New(st *store.Store, log *slog.Logger) *Service {
	return &Service{store: st, log: log, now: time.Now}
}

// SetClock overrides the clock. It exists for tests.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Build captures a new dataset version for the incident, derives hypotheses
// from it and stores both. It satisfies detect.Investigator.
func (s *Service) Build(ctx context.Context, incidentID int64, reason string) error {
	_, err := s.BuildDataset(ctx, incidentID, reason)
	return err
}

// BuildDataset is Build returning the captured dataset.
func (s *Service) BuildDataset(ctx context.Context, incidentID int64, reason string) (*model.Dataset, error) {
	inc, err := s.store.GetIncident(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	end := now.Add(-settleDelay)
	if inc.ResolvedAt != nil && inc.ResolvedAt.Before(end) {
		end = *inc.ResolvedAt
	}
	baselineStart := inc.StartedAt.Add(-baselineDuration)

	// Locate the onset inside the detection window so the incident phase
	// does not begin with healthy traffic.
	svc, err := s.store.ServiceStats(ctx, hist.TruncateBucket(baselineStart), end)
	if err != nil {
		return nil, err
	}
	edges, err := s.store.EdgeStats(ctx, hist.TruncateBucket(baselineStart), end)
	if err != nil {
		return nil, err
	}
	so, eo := EstimateOnsets(svc, edges, inc.StartedAt)
	incidentStart := EarliestOnset(so, eo, inc.StartedAt)
	if !end.After(incidentStart) {
		end = incidentStart.Add(hist.BucketDuration)
	}
	if end.Sub(incidentStart) > maxIncidentWindow {
		end = incidentStart.Add(maxIncidentWindow)
	}

	ds, err := s.store.CaptureDataset(ctx, store.CaptureParams{
		IncidentID: incidentID, BaselineStart: baselineStart, IncidentStart: incidentStart, WindowEnd: end,
		MaxBaselineTraces: maxBaselineTraces, MaxIncidentTraces: maxIncidentTraces, MaxSpansPerTrace: maxSpansPerTrace,
	})
	if err != nil {
		return nil, err
	}
	faults, err := s.knownFaults(ctx, incidentStart, end)
	if err != nil {
		return nil, err
	}
	an := Analyze(ds, faults)
	if err := s.store.SaveHypotheses(ctx, ds.ID, incidentID, an.Hypotheses); err != nil {
		return nil, fmt.Errorf("save hypotheses: %w", err)
	}
	top := "none"
	if len(an.Hypotheses) > 0 {
		top = an.Hypotheses[0].Title
	}
	detail, _ := json.Marshal(map[string]any{"reason": reason, "dataset_version": ds.Version, "traces": ds.TraceCount,
		"spans": ds.SpanCount, "checksum": ds.Checksum, "hypotheses": len(an.Hypotheses), "top_hypothesis": top})
	if _, err := s.store.AddEvent(ctx, model.Event{Time: now, Kind: model.EventInvestigation, IncidentID: &incidentID,
		Service: inc.PrimaryService, Detail: detail,
		Title: fmt.Sprintf("Dataset v%d captured (%d traces, %d spans) and %d hypotheses derived: %s",
			ds.Version, ds.TraceCount, ds.SpanCount, len(an.Hypotheses), reason)}); err != nil {
		s.log.Warn("record investigation event failed", "error", err)
	}
	s.log.Info("investigation built", "incident", inc.Key, "dataset_version", ds.Version, "traces", ds.TraceCount,
		"hypotheses", len(an.Hypotheses), "reason", reason)
	return ds, nil
}

func (s *Service) knownFaults(ctx context.Context, from, to time.Time) ([]KnownFault, error) {
	exps, err := s.store.ExperimentsOverlapping(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]KnownFault, 0, len(exps))
	for _, e := range exps {
		out = append(out, KnownFault{ExperimentID: e.ID, Name: e.Name, Scenario: e.Scenario, Targets: e.Targets,
			StartedAt: e.StartedAt, EndedAt: e.EndedAt})
	}
	return out, nil
}

// RelatedTrace is a trace offered as evidence in an investigation.
type RelatedTrace struct {
	TraceID    string    `json:"trace_id"`
	Start      time.Time `json:"start_time"`
	Operation  string    `json:"operation"`
	DurationMs float64   `json:"duration_ms"`
	Error      bool      `json:"error"`
	Reason     string    `json:"reason"`
}

// View is everything shown on the investigation page for one incident.
type View struct {
	Incident      model.Incident     `json:"incident"`
	Dataset       *model.Dataset     `json:"dataset"`
	Datasets      []model.Dataset    `json:"datasets"`
	Analysis      *Analysis          `json:"analysis"`
	Hypotheses    []model.Hypothesis `json:"hypotheses"`
	KnownFaults   []KnownFault       `json:"known_faults"`
	RelatedTraces []RelatedTrace     `json:"related_traces"`
	Timeline      []model.Event      `json:"timeline"`
	Replays       []model.ReplayRun  `json:"replays"`
	ReplayOptions *replay.Options    `json:"replay_options"`
	Methodology   []string           `json:"methodology"`
	IntegrityOK   bool               `json:"dataset_integrity_ok"`
}

// timelineLimit caps the events returned with an investigation.
const timelineLimit = 500

// Load assembles the investigation view from stored data. Hypotheses are
// read from storage; service and edge profiles are recomputed from the
// stored dataset, which is deterministic.
func (s *Service) Load(ctx context.Context, incidentID int64) (*View, error) {
	inc, err := s.store.GetIncident(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	v := &View{Incident: inc, Methodology: Methodology(), KnownFaults: []KnownFault{}, RelatedTraces: []RelatedTrace{},
		Hypotheses: []model.Hypothesis{}}
	if v.Datasets, err = s.store.ListDatasets(ctx, incidentID); err != nil {
		return nil, err
	}
	if v.Timeline, err = s.store.ListEvents(ctx, store.EventFilter{IncidentID: &incidentID, Limit: timelineLimit, Ascending: true}); err != nil {
		return nil, err
	}
	if v.Replays, err = s.store.ListReplays(ctx, incidentID, false); err != nil {
		return nil, err
	}
	if len(v.Datasets) == 0 {
		return v, nil
	}
	ds, err := s.store.LoadDataset(ctx, v.Datasets[0].ID)
	if err != nil {
		return nil, err
	}
	v.Dataset = ds
	v.IntegrityOK = store.Checksum(ds.Spans) == ds.Checksum
	if v.KnownFaults, err = s.knownFaults(ctx, ds.IncidentStart, ds.WindowEnd); err != nil {
		return nil, err
	}
	an := Analyze(ds, v.KnownFaults)
	v.Analysis = &an
	if v.Hypotheses, err = s.store.ListHypotheses(ctx, ds.ID); err != nil {
		return nil, err
	}
	opts := replay.OptionsFor(ds)
	v.ReplayOptions = &opts
	v.RelatedTraces = relatedTraces(ds, v.Hypotheses, inc.Signals)
	return v, nil
}

// LatestDataset returns the newest dataset of an incident, or
// store.ErrNotFound when none has been captured.
func (s *Service) LatestDataset(ctx context.Context, incidentID int64) (*model.Dataset, error) {
	list, err := s.store.ListDatasets(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, store.ErrNotFound
	}
	return s.store.LoadDataset(ctx, list[0].ID)
}

// ErrIntegrity is returned when a stored dataset no longer matches the
// checksum recorded at capture.
var ErrIntegrity = errors.New("dataset integrity check failed: stored spans do not match the capture checksum")

// relatedTraces picks traces preserved in the dataset that an operator is
// most likely to want to open: those cited by hypotheses and signals.
func relatedTraces(ds *model.Dataset, hyps []model.Hypothesis, signals []model.Signal) []RelatedTrace {
	type root struct {
		start     time.Time
		op        string
		durNs     int64
		err       bool
		preserved bool
	}
	roots := map[string]root{}
	for i := range ds.Spans {
		sp := &ds.Spans[i]
		if sp.ParentSpanID == "" {
			roots[sp.TraceID] = root{sp.Start, sp.Service + " · " + sp.Name, sp.DurationNs, sp.IsError(), true}
		}
	}
	var out []RelatedTrace
	seen := map[string]bool{}
	add := func(id, reason string) {
		if seen[id] || len(out) >= relatedTracesLimit {
			return
		}
		r, ok := roots[id]
		if !ok {
			return
		}
		seen[id] = true
		out = append(out, RelatedTrace{TraceID: id, Start: r.start, Operation: r.op,
			DurationMs: float64(r.durNs) / 1e6, Error: r.err, Reason: reason})
	}
	for _, h := range hyps {
		for _, ev := range h.Evidence {
			for _, id := range ev.TraceIDs {
				add(id, fmt.Sprintf("Cited by hypothesis #%d (%s)", h.Rank, ev.Check))
			}
		}
	}
	for _, sg := range signals {
		for _, id := range sg.Exemplars {
			add(id, "Exemplar for rule "+sg.Rule+" on "+sg.Service)
		}
	}
	if out == nil {
		out = []RelatedTrace{}
	}
	return out
}
