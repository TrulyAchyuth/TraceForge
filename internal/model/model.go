// Package model holds the domain types shared across TraceForge packages.
package model

import (
	"encoding/json"
	"time"
)

// Span kinds mirror the OTLP SpanKind enum.
const (
	KindUnspecified = 0
	KindInternal    = 1
	KindServer      = 2
	KindClient      = 3
	KindProducer    = 4
	KindConsumer    = 5
)

// Status codes mirror the OTLP Status.StatusCode enum.
const (
	StatusUnset = 0
	StatusOK    = 1
	StatusError = 2
)

// Span is a single ingested span in TraceForge's normalised form.
type Span struct {
	TraceID       string         `json:"trace_id"`
	SpanID        string         `json:"span_id"`
	ParentSpanID  string         `json:"parent_span_id"`
	Service       string         `json:"service"`
	Name          string         `json:"name"`
	Kind          int            `json:"kind"`
	Start         time.Time      `json:"start_time"`
	DurationNs    int64          `json:"duration_ns"`
	StatusCode    int            `json:"status_code"`
	StatusMessage string         `json:"status_message,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	Resource      map[string]any `json:"resource,omitempty"`
	Events        []SpanEvent    `json:"events,omitempty"`
	// PeerService is the callee named by the span's own attributes
	// (peer.service, falling back to server.address). Empty when absent.
	PeerService string `json:"peer_service,omitempty"`
	// ResendCount is http.request.resend_count: 0 for a first attempt.
	ResendCount int `json:"resend_count,omitempty"`
	// ErrorType is the error.type attribute, when present.
	ErrorType string `json:"error_type,omitempty"`
}

// End returns the span end time.
func (s *Span) End() time.Time { return s.Start.Add(time.Duration(s.DurationNs)) }

// IsError reports whether the span carries an error status.
func (s *Span) IsError() bool { return s.StatusCode == StatusError }

// IsEntry reports whether the span counts as a request handled by its service:
// SERVER and CONSUMER spans, plus root spans of any kind.
func (s *Span) IsEntry() bool {
	return s.Kind == KindServer || s.Kind == KindConsumer || s.ParentSpanID == ""
}

// SpanEvent is a timestamped event attached to a span.
type SpanEvent struct {
	Time       time.Time      `json:"time"`
	Name       string         `json:"name"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// TraceSummary is the per-trace row used by trace search.
type TraceSummary struct {
	TraceID     string    `json:"trace_id"`
	Start       time.Time `json:"start_time"`
	DurationNs  int64     `json:"duration_ns"`
	RootService string    `json:"root_service"`
	RootName    string    `json:"root_name"`
	SpanCount   int       `json:"span_count"`
	ErrorCount  int       `json:"error_count"`
	Services    []string  `json:"services"`
}

// StatBucket is one rollup bucket of entry-span statistics for a service.
type StatBucket struct {
	Bucket        time.Time `json:"bucket"`
	Service       string    `json:"service"`
	Requests      int64     `json:"requests"`
	Errors        int64     `json:"errors"`
	DurationSumNs int64     `json:"duration_sum_ns"`
	Hist          []int64   `json:"hist"`
}

// EdgeBucket is one rollup bucket of client-call statistics for a caller→callee edge.
type EdgeBucket struct {
	Bucket        time.Time `json:"bucket"`
	Caller        string    `json:"caller"`
	Callee        string    `json:"callee"`
	Calls         int64     `json:"calls"`
	Errors        int64     `json:"errors"`
	Retries       int64     `json:"retries"`
	DurationSumNs int64     `json:"duration_sum_ns"`
	Hist          []int64   `json:"hist"`
}

// Incident lifecycle states.
const (
	IncidentOpen          = "open"
	IncidentAcknowledged  = "acknowledged"
	IncidentInvestigating = "investigating"
	IncidentResolved      = "resolved"
	IncidentClosed        = "closed"
)

// Severities, ordered from least to most severe.
const (
	SevWarning  = "warning"
	SevMajor    = "major"
	SevCritical = "critical"
)

// SeverityRank orders severities for comparison.
func SeverityRank(s string) int {
	switch s {
	case SevCritical:
		return 3
	case SevMajor:
		return 2
	case SevWarning:
		return 1
	}
	return 0
}

// IncidentActive reports whether a status still accepts new detection signals.
func IncidentActive(status string) bool {
	return status == IncidentOpen || status == IncidentAcknowledged || status == IncidentInvestigating
}

// Incident is a detected abnormal condition and its lifecycle state.
type Incident struct {
	ID               int64      `json:"id"`
	Key              string     `json:"key"`
	Title            string     `json:"title"`
	Severity         string     `json:"severity"`
	Status           string     `json:"status"`
	PrimaryService   string     `json:"primary_service"`
	AffectedServices []string   `json:"affected_services"`
	TriggerRule      string     `json:"trigger_rule"`
	StartedAt        time.Time  `json:"started_at"`
	DetectedAt       time.Time  `json:"detected_at"`
	LastSignalAt     time.Time  `json:"last_signal_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
	Signals          []Signal   `json:"signals,omitempty"`
	Notes            []Note     `json:"notes,omitempty"`
}

// Signal is one detection rule firing for a service (or caller→peer edge).
type Signal struct {
	ID         int64     `json:"id"`
	IncidentID int64     `json:"incident_id"`
	Rule       string    `json:"rule"`
	Service    string    `json:"service"`
	Peer       string    `json:"peer,omitempty"`
	Severity   string    `json:"severity"`
	Value      float64   `json:"value"`
	Baseline   float64   `json:"baseline"`
	Threshold  float64   `json:"threshold"`
	PeakValue  float64   `json:"peak_value"`
	Unit       string    `json:"unit"`
	Message    string    `json:"message"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Active     bool      `json:"active"`
	Exemplars  []string  `json:"exemplar_trace_ids"`
}

// Note is an operator-authored investigation note.
type Note struct {
	ID         int64     `json:"id"`
	IncidentID int64     `json:"incident_id"`
	Author     string    `json:"author"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

// Event kinds recorded on the timeline.
const (
	EventDetection     = "detection"
	EventSignal        = "signal"
	EventServiceState  = "service_state"
	EventFaultInjected = "fault_injected"
	EventFaultStopped  = "fault_stopped"
	EventRecovery      = "recovery"
	EventInvestigation = "investigation"
	EventReplay        = "replay"
	EventReport        = "report"
	EventTelemetry     = "telemetry"
)

// Event is one entry on the chronological timeline.
type Event struct {
	ID           int64           `json:"id"`
	Time         time.Time       `json:"time"`
	Kind         string          `json:"kind"`
	Severity     string          `json:"severity,omitempty"`
	Title        string          `json:"title"`
	Detail       json.RawMessage `json:"detail,omitempty"`
	IncidentID   *int64          `json:"incident_id,omitempty"`
	Service      string          `json:"service,omitempty"`
	TraceID      string          `json:"trace_id,omitempty"`
	ExperimentID *int64          `json:"experiment_id,omitempty"`
	ReplayID     *int64          `json:"replay_id,omitempty"`
}

// Phases of a captured dataset.
const (
	PhaseBaseline = "baseline"
	PhaseIncident = "incident"
)

// DatasetSpan is a span preserved in an incident dataset.
type DatasetSpan struct {
	Span
	Phase string `json:"phase"`
}

// Dataset is an immutable capture of the telemetry around an incident.
type Dataset struct {
	ID            int64         `json:"id"`
	IncidentID    int64         `json:"incident_id"`
	Version       int           `json:"version"`
	CapturedAt    time.Time     `json:"captured_at"`
	BaselineStart time.Time     `json:"baseline_start"`
	IncidentStart time.Time     `json:"incident_start"`
	WindowEnd     time.Time     `json:"window_end"`
	TraceCount    int           `json:"trace_count"`
	SpanCount     int           `json:"span_count"`
	Checksum      string        `json:"checksum"`
	Meta          DatasetMeta   `json:"meta"`
	ServiceSeries []StatBucket  `json:"service_series,omitempty"`
	EdgeSeries    []EdgeBucket  `json:"edge_series,omitempty"`
	Spans         []DatasetSpan `json:"-"`
}

// DatasetMeta records how a dataset was sampled.
type DatasetMeta struct {
	BaselineTracesAvailable int `json:"baseline_traces_available"`
	BaselineTracesSampled   int `json:"baseline_traces_sampled"`
	IncidentTracesAvailable int `json:"incident_traces_available"`
	IncidentTracesSampled   int `json:"incident_traces_sampled"`
	BucketSeconds           int `json:"bucket_seconds"`
}

// Evidence classes. These keep observed facts, correlations, inferences and
// simulation output distinguishable everywhere they are shown.
const (
	ClassObserved    = "observed"
	ClassCorrelation = "correlation"
	ClassInference   = "inference"
	ClassSimulated   = "simulated"
)

// Evidence polarities.
const (
	PolaritySupporting    = "supporting"
	PolarityContradicting = "contradicting"
	PolarityNeutral       = "neutral"
)

// Evidence is one item of evidence attached to a hypothesis.
type Evidence struct {
	Check    string   `json:"check"`
	Polarity string   `json:"polarity"`
	Class    string   `json:"class"`
	Points   int      `json:"points"`
	Text     string   `json:"text"`
	Service  string   `json:"service,omitempty"`
	Peer     string   `json:"peer,omitempty"`
	Metric   string   `json:"metric,omitempty"`
	TraceIDs []string `json:"trace_ids,omitempty"`
}

// Hypothesis is a ranked candidate explanation for an incident.
type Hypothesis struct {
	ID                    int64           `json:"id"`
	DatasetID             int64           `json:"dataset_id"`
	IncidentID            int64           `json:"incident_id"`
	Rank                  int             `json:"rank"`
	Kind                  string          `json:"kind"`
	Service               string          `json:"service"`
	Peer                  string          `json:"peer,omitempty"`
	Title                 string          `json:"title"`
	Statement             string          `json:"statement"`
	Score                 int             `json:"score"`
	MaxScore              int             `json:"max_score"`
	Evidence              []Evidence      `json:"evidence"`
	SuggestedIntervention json.RawMessage `json:"suggested_intervention,omitempty"`
	MatchesKnownFault     bool            `json:"matches_known_fault"`
}

// Experiment is a failure-laboratory run.
type Experiment struct {
	ID         int64           `json:"id"`
	Scenario   string          `json:"scenario"`
	Name       string          `json:"name"`
	Params     json.RawMessage `json:"params"`
	Targets    []string        `json:"targets"`
	Seed       int64           `json:"seed"`
	Status     string          `json:"status"`
	StartedAt  time.Time       `json:"started_at"`
	PlannedEnd time.Time       `json:"planned_end"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
}

// Experiment states.
const (
	ExperimentRunning   = "running"
	ExperimentStopped   = "stopped"
	ExperimentCompleted = "completed"
	ExperimentFailed    = "failed"
)

// ReplayRun is a stored counterfactual replay and its result.
type ReplayRun struct {
	ID              int64           `json:"id"`
	IncidentID      int64           `json:"incident_id"`
	DatasetID       int64           `json:"dataset_id"`
	DatasetVersion  int             `json:"dataset_version"`
	Name            string          `json:"name"`
	Interventions   json.RawMessage `json:"interventions"`
	Seed            int64           `json:"seed"`
	EngineVersion   string          `json:"engine_version"`
	DatasetChecksum string          `json:"dataset_checksum"`
	Result          json.RawMessage `json:"result,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	DurationMs      int64           `json:"duration_ms"`
}
