package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

// settle is how long after resolution an incident is snapshotted, so the
// final investigation capture has landed first.
const settle = 30 * time.Second

// Service reads and writes incident memory.
type Service struct {
	store *store.Store
	inv   *investigate.Service
	log   *slog.Logger
	now   func() time.Time
}

// NewService creates the memory service.
func NewService(st *store.Store, inv *investigate.Service, log *slog.Logger) *Service {
	return &Service{store: st, inv: inv, log: log, now: time.Now}
}

// Records loads every stored record.
func (s *Service) Records(ctx context.Context) ([]Record, error) {
	rows, err := s.store.ListMemory(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		var r Record
		if err := json.Unmarshal(row.Record, &r); err != nil {
			return nil, fmt.Errorf("decode memory record %s: %w", row.Key, err)
		}
		r.ID, r.Key, r.IncidentID, r.Simulated = row.ID, row.Key, row.IncidentID, row.Simulated
		out = append(out, r)
	}
	return out, nil
}

// Get returns one record by id.
func (s *Service) Get(ctx context.Context, id int64) (Record, error) {
	all, err := s.Records(ctx)
	if err != nil {
		return Record{}, err
	}
	for _, r := range all {
		if r.ID == id {
			return r, nil
		}
	}
	return Record{}, store.ErrNotFound
}

// Current builds the live record of an incident, resolved or not.
func (s *Service) Current(ctx context.Context, incidentID int64) (Record, error) {
	v, err := s.inv.Load(ctx, incidentID)
	if err != nil {
		return Record{}, err
	}
	runs, err := s.store.ListReplays(ctx, incidentID, true)
	if err != nil {
		return Record{}, err
	}
	return FromView(v, runs, s.now()), nil
}

// Similar compares an incident with everything in memory.
func (s *Service) Similar(ctx context.Context, incidentID int64) (Similarity, error) {
	cur, err := s.Current(ctx, incidentID)
	if err != nil {
		return Similarity{}, err
	}
	hist, err := s.Records(ctx)
	if err != nil {
		return Similarity{}, err
	}
	return Find(cur, hist), nil
}

// Snapshot stores the record of a resolved incident, replacing an earlier
// snapshot. It is a no-op for incidents that are still open.
func (s *Service) Snapshot(ctx context.Context, incidentID int64) error {
	cur, err := s.Current(ctx, incidentID)
	if err != nil {
		return err
	}
	if cur.Recovery.ResolvedAt == nil {
		return nil
	}
	raw, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	_, inserted, err := s.store.UpsertMemory(ctx, incidentID, cur.Key, raw)
	if err != nil {
		return err
	}
	if inserted {
		_, err = s.store.AddEvent(ctx, model.Event{Time: s.now().UTC(), Kind: model.EventInvestigation, IncidentID: &incidentID,
			Title: "Saved to incident memory", Detail: json.RawMessage(`{"method_version":"` + MethodVersion + `"}`)})
	}
	return err
}

// Sync snapshots every resolved incident that has none.
func (s *Service) Sync(ctx context.Context) error {
	ids, err := s.store.ResolvedWithoutMemory(ctx, settle, s.now())
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := s.Snapshot(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("incident %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Run syncs periodically until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("incident memory sync failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Seed inserts the simulated demonstration records.
func (s *Service) Seed(ctx context.Context) (int, error) {
	seeds := Seeds()
	for _, r := range seeds {
		raw, err := json.Marshal(r)
		if err != nil {
			return 0, err
		}
		if err := s.store.InsertSimulatedMemory(ctx, r.Key, raw); err != nil {
			return 0, err
		}
	}
	return len(seeds), nil
}

// Unseed removes the simulated records.
func (s *Service) Unseed(ctx context.Context) (int64, error) {
	return s.store.DeleteSimulatedMemory(ctx)
}
