package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

const incidentCols = `id, title, severity, status, primary_service, affected_services, trigger_rule,
	started_at, detected_at, last_signal_at, resolved_at`

// IncidentKey formats the human-facing incident identifier.
func IncidentKey(id int64) string { return fmt.Sprintf("INC-%04d", id) }

func scanIncident(row pgx.CollectableRow) (model.Incident, error) {
	var in model.Incident
	err := row.Scan(&in.ID, &in.Title, &in.Severity, &in.Status, &in.PrimaryService, &in.AffectedServices,
		&in.TriggerRule, &in.StartedAt, &in.DetectedAt, &in.LastSignalAt, &in.ResolvedAt)
	in.Key = IncidentKey(in.ID)
	in.StartedAt, in.DetectedAt, in.LastSignalAt = in.StartedAt.UTC(), in.DetectedAt.UTC(), in.LastSignalAt.UTC()
	if in.ResolvedAt != nil {
		t := in.ResolvedAt.UTC()
		in.ResolvedAt = &t
	}
	return in, err
}

// CreateIncident inserts a new incident and returns it with its ID.
func (s *Store) CreateIncident(ctx context.Context, in model.Incident) (model.Incident, error) {
	rows, err := s.pool.Query(ctx, `
		INSERT INTO incidents (title, severity, status, primary_service, affected_services, trigger_rule,
			started_at, detected_at, last_signal_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+incidentCols,
		in.Title, in.Severity, in.Status, in.PrimaryService, in.AffectedServices, in.TriggerRule,
		in.StartedAt, in.DetectedAt, in.LastSignalAt)
	if err != nil {
		return in, err
	}
	return pgx.CollectExactlyOneRow(rows, scanIncident)
}

// GetIncident returns an incident with its signals and notes.
func (s *Store) GetIncident(ctx context.Context, id int64) (model.Incident, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+incidentCols+` FROM incidents WHERE id = $1`, id)
	if err != nil {
		return model.Incident{}, err
	}
	in, err := pgx.CollectExactlyOneRow(rows, scanIncident)
	if errors.Is(err, pgx.ErrNoRows) {
		return in, ErrNotFound
	}
	if err != nil {
		return in, err
	}
	if in.Signals, err = s.ListSignals(ctx, id); err != nil {
		return in, err
	}
	in.Notes, err = s.ListNotes(ctx, id)
	return in, err
}

// IncidentFilter selects incidents for listing.
type IncidentFilter struct {
	Statuses []string
	Limit    int
	Offset   int
}

// ListIncidents returns incidents newest first and the total matching count.
func (s *Store) ListIncidents(ctx context.Context, f IncidentFilter) ([]model.Incident, int, error) {
	where, args := "TRUE", []any{}
	if len(f.Statuses) > 0 {
		where, args = "status = ANY($1)", []any{f.Statuses}
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM incidents WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, `SELECT `+incidentCols+` FROM incidents WHERE `+where+
		` ORDER BY detected_at DESC, id DESC LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, scanIncident)
	return list, total, err
}

// ActiveIncidents returns incidents that still accept detection signals.
func (s *Store) ActiveIncidents(ctx context.Context) ([]model.Incident, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+incidentCols+` FROM incidents
		WHERE status IN ('open', 'acknowledged', 'investigating') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanIncident)
}

// IncidentIntervals returns [started_at, resolved_at] spans of incidents that
// overlap the period since the given time. Unresolved incidents end at "now".
func (s *Store) IncidentIntervals(ctx context.Context, since, now time.Time) ([][2]time.Time, error) {
	rows, err := s.pool.Query(ctx, `SELECT started_at, COALESCE(resolved_at, $2) FROM incidents
		WHERE COALESCE(resolved_at, $2) >= $1`, since, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]time.Time, error) {
		var iv [2]time.Time
		err := row.Scan(&iv[0], &iv[1])
		return iv, err
	})
}

// UpdateIncidentDetection updates the fields the detector maintains.
func (s *Store) UpdateIncidentDetection(ctx context.Context, id int64, severity string, affected []string, lastSignal time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE incidents SET severity = $2, affected_services = $3, last_signal_at = $4 WHERE id = $1`,
		id, severity, affected, lastSignal)
	return err
}

// SetIncidentStatus changes an incident's lifecycle status. resolved_at is set
// on the first transition to resolved or closed and cleared on reopening.
func (s *Store) SetIncidentStatus(ctx context.Context, id int64, status string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE incidents SET status = $2,
			resolved_at = CASE
				WHEN $2 IN ('resolved', 'closed') THEN COALESCE(resolved_at, $3)
				ELSE NULL END
		WHERE id = $1`, id, status, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSignal(row pgx.CollectableRow) (model.Signal, error) {
	var sg model.Signal
	err := row.Scan(&sg.ID, &sg.IncidentID, &sg.Rule, &sg.Service, &sg.Peer, &sg.Severity, &sg.Value, &sg.Baseline,
		&sg.Threshold, &sg.PeakValue, &sg.Unit, &sg.Message, &sg.FirstSeen, &sg.LastSeen, &sg.Active, &sg.Exemplars)
	sg.FirstSeen, sg.LastSeen = sg.FirstSeen.UTC(), sg.LastSeen.UTC()
	if sg.Exemplars == nil {
		sg.Exemplars = []string{}
	}
	return sg, err
}

const signalCols = `id, incident_id, rule, service, peer, severity, value, baseline, threshold, peak_value, unit,
	message, first_seen, last_seen, active, exemplars`

// ListSignals returns the signals attached to an incident.
func (s *Store) ListSignals(ctx context.Context, incidentID int64) ([]model.Signal, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+signalCols+` FROM incident_signals WHERE incident_id = $1 ORDER BY first_seen, id`, incidentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSignal)
}

// UpsertSignal records a rule firing. It reports whether the signal is new
// for the incident. Exemplars are kept from the first sighting unless empty.
func (s *Store) UpsertSignal(ctx context.Context, sg model.Signal) (created bool, err error) {
	err = s.pool.QueryRow(ctx, `
		INSERT INTO incident_signals (incident_id, rule, service, peer, severity, value, baseline, threshold,
			peak_value, unit, message, first_seen, last_seen, active, exemplars)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $6, $9, $10, $11, $11, TRUE, $12)
		ON CONFLICT (incident_id, rule, service, peer) DO UPDATE SET
			severity = EXCLUDED.severity, value = EXCLUDED.value, baseline = EXCLUDED.baseline,
			threshold = EXCLUDED.threshold, message = EXCLUDED.message, last_seen = EXCLUDED.last_seen, active = TRUE,
			peak_value = GREATEST(incident_signals.peak_value, EXCLUDED.value),
			exemplars = CASE WHEN cardinality(incident_signals.exemplars) = 0 THEN EXCLUDED.exemplars
				ELSE incident_signals.exemplars END
		RETURNING (xmax = 0)`,
		sg.IncidentID, sg.Rule, sg.Service, sg.Peer, sg.Severity, sg.Value, sg.Baseline, sg.Threshold,
		sg.Unit, sg.Message, sg.LastSeen, nonNil(sg.Exemplars)).Scan(&created)
	return created, err
}

// DeactivateStaleSignals marks signals not seen since cutoff as inactive.
func (s *Store) DeactivateStaleSignals(ctx context.Context, cutoff time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE incident_signals SET active = FALSE WHERE active AND last_seen < $1`, cutoff)
	return err
}

// AddNote appends an investigation note.
func (s *Store) AddNote(ctx context.Context, incidentID int64, author, body string) (model.Note, error) {
	n := model.Note{IncidentID: incidentID, Author: author, Body: body}
	err := s.pool.QueryRow(ctx, `INSERT INTO incident_notes (incident_id, author, body) VALUES ($1, $2, $3)
		RETURNING id, created_at`, incidentID, author, body).Scan(&n.ID, &n.CreatedAt)
	return n, err
}

// ListNotes returns an incident's notes, oldest first.
func (s *Store) ListNotes(ctx context.Context, incidentID int64) ([]model.Note, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, incident_id, author, body, created_at FROM incident_notes
		WHERE incident_id = $1 ORDER BY created_at, id`, incidentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[model.Note])
}

// AddEvent records a timeline event and returns its ID.
func (s *Store) AddEvent(ctx context.Context, ev model.Event) (int64, error) {
	detail := ev.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO events (ts, kind, severity, title, detail, incident_id, service, trace_id, experiment_id, replay_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		ev.Time, ev.Kind, ev.Severity, ev.Title, []byte(detail), ev.IncidentID, ev.Service, ev.TraceID,
		ev.ExperimentID, ev.ReplayID).Scan(&id)
	return id, err
}

// EventFilter selects timeline events.
type EventFilter struct {
	IncidentID *int64
	Kinds      []string
	Service    string
	From, To   time.Time
	BeforeID   int64
	Limit      int
	Ascending  bool
}

// ListEvents returns timeline events, newest first unless Ascending is set.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]model.Event, error) {
	where := []string{"TRUE"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.IncidentID != nil {
		add("incident_id = $%d", *f.IncidentID)
	}
	if len(f.Kinds) > 0 {
		add("kind = ANY($%d)", f.Kinds)
	}
	if f.Service != "" {
		add("service = $%d", f.Service)
	}
	if !f.From.IsZero() {
		add("ts >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("ts < $%d", f.To)
	}
	if f.BeforeID > 0 {
		add("id < $%d", f.BeforeID)
	}
	order := "ts DESC, id DESC"
	if f.Ascending {
		order = "ts ASC, id ASC"
	}
	args = append(args, f.Limit)
	rows, err := s.pool.Query(ctx, `
		SELECT id, ts, kind, severity, title, detail, incident_id, service, trace_id, experiment_id, replay_id
		FROM events WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Event, error) {
		var ev model.Event
		var detail []byte
		err := row.Scan(&ev.ID, &ev.Time, &ev.Kind, &ev.Severity, &ev.Title, &detail, &ev.IncidentID, &ev.Service,
			&ev.TraceID, &ev.ExperimentID, &ev.ReplayID)
		ev.Time, ev.Detail = ev.Time.UTC(), detail
		return ev, err
	})
}

// ServiceHealth is the last evaluated health state of a service.
type ServiceHealth struct {
	Service  string    `json:"service"`
	Status   string    `json:"status"`
	Since    time.Time `json:"since"`
	LastSeen time.Time `json:"last_seen"`
}

// ListServiceHealth returns the stored health state for every known service.
func (s *Store) ListServiceHealth(ctx context.Context) ([]ServiceHealth, error) {
	rows, err := s.pool.Query(ctx, `SELECT service, status, since, last_seen FROM service_health ORDER BY service`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ServiceHealth])
}

// SetServiceHealth stores a service's health. "since" only moves when the
// status changes; lastSeen is only advanced, never moved back.
func (s *Store) SetServiceHealth(ctx context.Context, service, status string, at, lastSeen time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO service_health (service, status, since, last_seen) VALUES ($1, $2, $3, $4)
		ON CONFLICT (service) DO UPDATE SET
			since = CASE WHEN service_health.status <> EXCLUDED.status THEN EXCLUDED.since ELSE service_health.since END,
			status = EXCLUDED.status,
			last_seen = GREATEST(service_health.last_seen, EXCLUDED.last_seen)`, service, status, at, lastSeen)
	return err
}

// GetSetting loads a JSON setting into dst. It reports whether the key exists.
func (s *Store) GetSetting(ctx context.Context, key string, dst any) (bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, dst)
}

// PutSetting stores a JSON setting.
func (s *Store) PutSetting(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, key, raw)
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
