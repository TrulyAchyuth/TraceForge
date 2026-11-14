package store

import (
	"context"
	"encoding/json"
	"time"
)

// MemoryRow is one stored incident-memory record.
type MemoryRow struct {
	ID         int64
	IncidentID *int64
	Simulated  bool
	Key        string
	Record     json.RawMessage
}

// UpsertMemory stores or refreshes the snapshot of a real incident.
func (s *Store) UpsertMemory(ctx context.Context, incidentID int64, key string, record json.RawMessage) (id int64, inserted bool, err error) {
	err = s.pool.QueryRow(ctx, `
		INSERT INTO incident_memory (incident_id, simulated, key, record) VALUES ($1, FALSE, $2, $3)
		ON CONFLICT (incident_id) DO UPDATE SET record = EXCLUDED.record, updated_at = now()
		RETURNING id, (xmax = 0)`, incidentID, key, record).Scan(&id, &inserted)
	return id, inserted, err
}

// InsertSimulatedMemory stores a simulated demo record; an existing key is left alone.
func (s *Store) InsertSimulatedMemory(ctx context.Context, key string, record json.RawMessage) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO incident_memory (incident_id, simulated, key, record) VALUES (NULL, TRUE, $1, $2)
		ON CONFLICT (key) DO NOTHING`, key, record)
	return err
}

// DeleteSimulatedMemory removes every simulated record and reports how many.
func (s *Store) DeleteSimulatedMemory(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM incident_memory WHERE simulated`)
	return tag.RowsAffected(), err
}

// ListMemory returns all records in id order.
func (s *Store) ListMemory(ctx context.Context) ([]MemoryRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, incident_id, simulated, key, record FROM incident_memory ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryRow{}
	for rows.Next() {
		var r MemoryRow
		if err := rows.Scan(&r.ID, &r.IncidentID, &r.Simulated, &r.Key, &r.Record); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolvedWithoutMemory lists resolved or closed incidents that have no
// snapshot yet and were resolved at least `settle` ago.
func (s *Store) ResolvedWithoutMemory(ctx context.Context, settle time.Duration, now time.Time) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id FROM incidents i LEFT JOIN incident_memory m ON m.incident_id = i.id
		WHERE m.id IS NULL AND i.status IN ('resolved', 'closed') AND i.resolved_at <= $1 ORDER BY i.id`, now.Add(-settle))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
