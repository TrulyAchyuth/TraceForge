package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

const datasetSpanCols = `phase, trace_id, span_id, parent_span_id, service, name, kind, start_time, duration_ns,
	status_code, status_message, attributes, peer_service, resend_count, error_type`

func scanDatasetSpan(row pgx.CollectableRow) (model.DatasetSpan, error) {
	var d model.DatasetSpan
	var kind, status int16
	var resend int32
	var attrs []byte
	if err := row.Scan(&d.Phase, &d.TraceID, &d.SpanID, &d.ParentSpanID, &d.Service, &d.Name, &kind, &d.Start,
		&d.DurationNs, &status, &d.StatusMessage, &attrs, &d.PeerService, &resend, &d.ErrorType); err != nil {
		return d, err
	}
	d.Kind, d.StatusCode, d.ResendCount, d.Start = int(kind), int(status), int(resend), d.Start.UTC()
	return d, json.Unmarshal(attrs, &d.Attributes)
}

// CaptureParams describes the windows and sample sizes of a dataset capture.
type CaptureParams struct {
	IncidentID        int64
	BaselineStart     time.Time
	IncidentStart     time.Time
	WindowEnd         time.Time
	MaxBaselineTraces int
	MaxIncidentTraces int
	MaxSpansPerTrace  int
}

// CaptureDataset copies a deterministic uniform sample of the traces around an
// incident, plus the rollup series for the same window, into a new immutable
// dataset version. Traces are sampled by ordering on md5(trace_id), which is
// uniform with respect to outcome and repeatable for the same stored data.
func (s *Store) CaptureDataset(ctx context.Context, p CaptureParams) (*model.Dataset, error) {
	svcSeries, err := s.ServiceStats(ctx, hist.TruncateBucket(p.BaselineStart), p.WindowEnd)
	if err != nil {
		return nil, err
	}
	edgeSeries, err := s.EdgeStats(ctx, hist.TruncateBucket(p.BaselineStart), p.WindowEnd)
	if err != nil {
		return nil, err
	}
	svcJSON, err := json.Marshal(svcSeries)
	if err != nil {
		return nil, err
	}
	edgeJSON, err := json.Marshal(edgeSeries)
	if err != nil {
		return nil, err
	}

	var id int64
	meta := model.DatasetMeta{BucketSeconds: hist.BucketSeconds}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// Serialise captures per incident so versions are gap-free.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, p.IncidentID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO datasets (incident_id, version, captured_at, baseline_start, incident_start, window_end,
				service_series, edge_series)
			SELECT $1, COALESCE(max(version), 0) + 1, now(), $2, $3, $4, $5, $6 FROM datasets WHERE incident_id = $1
			RETURNING id`, p.IncidentID, p.BaselineStart, p.IncidentStart, p.WindowEnd, svcJSON, edgeJSON).Scan(&id); err != nil {
			return err
		}
		phases := []struct {
			name      string
			from, to  time.Time
			limit     int
			available *int
			sampled   *int
		}{
			{model.PhaseBaseline, p.BaselineStart, p.IncidentStart, p.MaxBaselineTraces, &meta.BaselineTracesAvailable, &meta.BaselineTracesSampled},
			{model.PhaseIncident, p.IncidentStart, p.WindowEnd, p.MaxIncidentTraces, &meta.IncidentTracesAvailable, &meta.IncidentTracesSampled},
		}
		for _, ph := range phases {
			const eligible = `start_time >= $1 AND start_time < $2 AND root_service <> '' AND span_count <= $3`
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM traces WHERE `+eligible,
				ph.from, ph.to, p.MaxSpansPerTrace).Scan(ph.available); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO dataset_spans (dataset_id, `+datasetSpanCols+`)
				SELECT $5, $6, s.trace_id, s.span_id, s.parent_span_id, s.service, s.name, s.kind, s.start_time,
					s.duration_ns, s.status_code, s.status_message, s.attributes, s.peer_service, s.resend_count, s.error_type
				FROM spans s
				WHERE s.trace_id IN (SELECT trace_id FROM traces WHERE `+eligible+` ORDER BY md5(trace_id) LIMIT $4)
				ON CONFLICT DO NOTHING`, ph.from, ph.to, p.MaxSpansPerTrace, ph.limit, id, ph.name); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(DISTINCT trace_id) FROM dataset_spans WHERE dataset_id = $1 AND phase = $2`,
				id, ph.name).Scan(ph.sampled); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT `+datasetSpanCols+` FROM dataset_spans WHERE dataset_id = $1
			ORDER BY trace_id, start_time, span_id`, id)
		if err != nil {
			return err
		}
		spans, err := pgx.CollectRows(rows, scanDatasetSpan)
		if err != nil {
			return err
		}
		metaJSON, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE datasets SET trace_count = $2, span_count = $3, checksum = $4, meta = $5 WHERE id = $1`,
			id, meta.BaselineTracesSampled+meta.IncidentTracesSampled, len(spans), Checksum(spans), metaJSON)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("capture dataset: %w", err)
	}
	return s.LoadDataset(ctx, id)
}

// Checksum fingerprints the structural content of dataset spans: identity,
// hierarchy, service, name, kind, timing, status and retry fields. Spans must
// be in canonical order (trace_id, start_time, span_id).
func Checksum(spans []model.DatasetSpan) string {
	h := sha256.New()
	for i := range spans {
		sp := &spans[i]
		fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%d|%d|%d|%d|%s|%d|%s\n", sp.Phase, sp.TraceID, sp.SpanID, sp.ParentSpanID,
			sp.Service, sp.Name, sp.Kind, sp.Start.UnixNano(), sp.DurationNs, sp.StatusCode, sp.PeerService,
			sp.ResendCount, sp.ErrorType)
	}
	return hex.EncodeToString(h.Sum(nil))
}

const datasetCols = `id, incident_id, version, captured_at, baseline_start, incident_start, window_end,
	trace_count, span_count, checksum, meta`

func scanDatasetHeader(row pgx.Row, d *model.Dataset, extra ...any) error {
	var meta []byte
	dest := append([]any{&d.ID, &d.IncidentID, &d.Version, &d.CapturedAt, &d.BaselineStart, &d.IncidentStart,
		&d.WindowEnd, &d.TraceCount, &d.SpanCount, &d.Checksum, &meta}, extra...)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	d.CapturedAt, d.BaselineStart = d.CapturedAt.UTC(), d.BaselineStart.UTC()
	d.IncidentStart, d.WindowEnd = d.IncidentStart.UTC(), d.WindowEnd.UTC()
	return json.Unmarshal(meta, &d.Meta)
}

// LoadDataset returns a dataset with its series and spans in canonical order.
func (s *Store) LoadDataset(ctx context.Context, id int64) (*model.Dataset, error) {
	d := &model.Dataset{}
	var svc, edge []byte
	err := scanDatasetHeader(s.pool.QueryRow(ctx, `SELECT `+datasetCols+`, service_series, edge_series
		FROM datasets WHERE id = $1`, id), d, &svc, &edge)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(svc, &d.ServiceSeries); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(edge, &d.EdgeSeries); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+datasetSpanCols+` FROM dataset_spans WHERE dataset_id = $1
		ORDER BY trace_id, start_time, span_id`, id)
	if err != nil {
		return nil, err
	}
	d.Spans, err = pgx.CollectRows(rows, scanDatasetSpan)
	return d, err
}

// ListDatasets returns dataset headers for an incident, newest version first.
func (s *Store) ListDatasets(ctx context.Context, incidentID int64) ([]model.Dataset, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+datasetCols+` FROM datasets WHERE incident_id = $1 ORDER BY version DESC`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Dataset
	for rows.Next() {
		var d model.Dataset
		if err := scanDatasetHeader(rows, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SaveHypotheses stores the ranked hypotheses derived from a dataset.
func (s *Store) SaveHypotheses(ctx context.Context, datasetID, incidentID int64, hyps []model.Hypothesis) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM hypotheses WHERE dataset_id = $1`, datasetID); err != nil {
			return err
		}
		for _, h := range hyps {
			ev, err := json.Marshal(h.Evidence)
			if err != nil {
				return err
			}
			var iv any
			if len(h.SuggestedIntervention) > 0 {
				iv = []byte(h.SuggestedIntervention)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO hypotheses (dataset_id, incident_id, rank, kind, service, peer, title, statement, score,
					max_score, evidence, suggested_intervention, matches_known_fault)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
				datasetID, incidentID, h.Rank, h.Kind, h.Service, h.Peer, h.Title, h.Statement, h.Score, h.MaxScore,
				ev, iv, h.MatchesKnownFault); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListHypotheses returns the hypotheses of a dataset in rank order.
func (s *Store) ListHypotheses(ctx context.Context, datasetID int64) ([]model.Hypothesis, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, dataset_id, incident_id, rank, kind, service, peer, title, statement, score, max_score, evidence,
			suggested_intervention, matches_known_fault
		FROM hypotheses WHERE dataset_id = $1 ORDER BY rank`, datasetID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Hypothesis, error) {
		var h model.Hypothesis
		var ev, iv []byte
		if err := row.Scan(&h.ID, &h.DatasetID, &h.IncidentID, &h.Rank, &h.Kind, &h.Service, &h.Peer, &h.Title,
			&h.Statement, &h.Score, &h.MaxScore, &ev, &iv, &h.MatchesKnownFault); err != nil {
			return h, err
		}
		h.SuggestedIntervention = iv
		return h, json.Unmarshal(ev, &h.Evidence)
	})
}

const replayCols = `r.id, r.incident_id, r.dataset_id, d.version, r.name, r.interventions, r.seed, r.engine_version,
	r.dataset_checksum, r.created_at, r.duration_ms`

func scanReplay(row pgx.Row, r *model.ReplayRun, extra ...any) error {
	var iv []byte
	dest := append([]any{&r.ID, &r.IncidentID, &r.DatasetID, &r.DatasetVersion, &r.Name, &iv, &r.Seed,
		&r.EngineVersion, &r.DatasetChecksum, &r.CreatedAt, &r.DurationMs}, extra...)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	r.Interventions, r.CreatedAt = iv, r.CreatedAt.UTC()
	return nil
}

// SaveReplay stores a replay run and returns its ID and creation time.
func (s *Store) SaveReplay(ctx context.Context, r model.ReplayRun) (model.ReplayRun, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO replay_runs (incident_id, dataset_id, name, interventions, seed, engine_version, dataset_checksum,
			result, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id, created_at`,
		r.IncidentID, r.DatasetID, r.Name, []byte(r.Interventions), r.Seed, r.EngineVersion, r.DatasetChecksum,
		[]byte(r.Result), r.DurationMs).Scan(&r.ID, &r.CreatedAt)
	return r, err
}

// GetReplay returns a replay run including its result.
func (s *Store) GetReplay(ctx context.Context, id int64) (model.ReplayRun, error) {
	var r model.ReplayRun
	var result []byte
	err := scanReplay(s.pool.QueryRow(ctx, `SELECT `+replayCols+`, r.result FROM replay_runs r
		JOIN datasets d ON d.id = r.dataset_id WHERE r.id = $1`, id), &r, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	r.Result = result
	return r, err
}

// ListReplays returns an incident's replay runs, newest first. withResult
// controls whether the (potentially large) result documents are included.
func (s *Store) ListReplays(ctx context.Context, incidentID int64, withResult bool) ([]model.ReplayRun, error) {
	cols := replayCols
	if withResult {
		cols += `, r.result`
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM replay_runs r JOIN datasets d ON d.id = r.dataset_id
		WHERE r.incident_id = $1 ORDER BY r.created_at DESC, r.id DESC`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ReplayRun{}
	for rows.Next() {
		var r model.ReplayRun
		var result []byte
		if withResult {
			err = scanReplay(rows, &r, &result)
		} else {
			err = scanReplay(rows, &r)
		}
		if err != nil {
			return nil, err
		}
		r.Result = result
		out = append(out, r)
	}
	return out, rows.Err()
}

const experimentCols = `id, scenario, name, params, targets, seed, status, started_at, planned_end, ended_at, stop_reason`

func scanExperiment(row pgx.CollectableRow) (model.Experiment, error) {
	var e model.Experiment
	var params []byte
	err := row.Scan(&e.ID, &e.Scenario, &e.Name, &params, &e.Targets, &e.Seed, &e.Status, &e.StartedAt,
		&e.PlannedEnd, &e.EndedAt, &e.StopReason)
	e.Params, e.StartedAt, e.PlannedEnd = params, e.StartedAt.UTC(), e.PlannedEnd.UTC()
	return e, err
}

// CreateExperiment records a started experiment.
func (s *Store) CreateExperiment(ctx context.Context, e model.Experiment) (model.Experiment, error) {
	rows, err := s.pool.Query(ctx, `
		INSERT INTO experiments (scenario, name, params, targets, seed, status, started_at, planned_end)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+experimentCols,
		e.Scenario, e.Name, []byte(e.Params), e.Targets, e.Seed, e.Status, e.StartedAt, e.PlannedEnd)
	if err != nil {
		return e, err
	}
	return pgx.CollectExactlyOneRow(rows, scanExperiment)
}

// FinishExperiment marks a running experiment as ended. It reports whether
// the experiment was running; a second call is a no-op.
func (s *Store) FinishExperiment(ctx context.Context, id int64, status, reason string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE experiments SET status = $2, stop_reason = $3, ended_at = $4
		WHERE id = $1 AND status = 'running'`, id, status, reason, at)
	return tag.RowsAffected() > 0, err
}

// GetExperiment returns one experiment.
func (s *Store) GetExperiment(ctx context.Context, id int64) (model.Experiment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+experimentCols+` FROM experiments WHERE id = $1`, id)
	if err != nil {
		return model.Experiment{}, err
	}
	e, err := pgx.CollectExactlyOneRow(rows, scanExperiment)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

// ListExperiments returns experiments newest first.
func (s *Store) ListExperiments(ctx context.Context, limit int) ([]model.Experiment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+experimentCols+` FROM experiments ORDER BY started_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanExperiment)
}

// RunningExperiments returns experiments that have not ended.
func (s *Store) RunningExperiments(ctx context.Context) ([]model.Experiment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+experimentCols+` FROM experiments WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanExperiment)
}

// ExperimentsOverlapping returns experiments active at any point in [from, to].
func (s *Store) ExperimentsOverlapping(ctx context.Context, from, to time.Time) ([]model.Experiment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+experimentCols+` FROM experiments
		WHERE started_at <= $2 AND COALESCE(ended_at, planned_end) >= $1 ORDER BY started_at`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanExperiment)
}
