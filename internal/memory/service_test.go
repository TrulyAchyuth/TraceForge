package memory

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

// Requires a disposable PostgreSQL database: TF_TEST_DATABASE_URL=postgres://...
// The test creates and deletes its own rows only.
func TestSnapshotSimilarAndSeedAgainstPostgres(t *testing.T) {
	url := os.Getenv("TF_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TF_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(st, investigate.New(st, log), log)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	mk := func(title string) model.Incident {
		in, err := st.CreateIncident(ctx, model.Incident{Title: title, Severity: "critical", Status: "open", PrimaryService: "checkout",
			AffectedServices: []string{"checkout", "payment"}, TriggerRule: "dependency_degradation",
			StartedAt: now.Add(-10 * time.Minute), DetectedAt: now.Add(-9 * time.Minute), LastSignalAt: now.Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertSignal(ctx, model.Signal{IncidentID: in.ID, Rule: "dependency_degradation", Service: "checkout", Peer: "payment",
			Severity: "critical", Unit: "ratio", FirstSeen: now.Add(-9 * time.Minute), LastSeen: now, Exemplars: []string{}}); err != nil {
			t.Fatal(err)
		}
		return in
	}
	old, cur := mk("old"), mk("current")
	t.Cleanup(func() {
		_, _ = svc.Unseed(ctx)
		if conn, err := pgx.Connect(ctx, url); err == nil {
			defer conn.Close(ctx)
			_, _ = conn.Exec(ctx, `DELETE FROM incidents WHERE id = ANY($1)`, []int64{old.ID, cur.ID})
		}
	})

	if err := svc.Snapshot(ctx, old.ID); err != nil { // open: must be a no-op
		t.Fatal(err)
	}
	if recs, _ := svc.Records(ctx); len(recs) != 0 && recs[0].IncidentID != nil && *recs[0].IncidentID == old.ID {
		t.Fatal("an open incident must not be snapshotted")
	}
	if err := st.SetIncidentStatus(ctx, old.ID, "resolved", now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Snapshot(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Snapshot(ctx, old.ID); err != nil { // idempotent refresh
		t.Fatal(err)
	}
	if n, err := svc.Seed(ctx); err != nil || n != len(Seeds()) {
		t.Fatalf("seed: %d %v", n, err)
	}
	if _, err := svc.Seed(ctx); err != nil {
		t.Fatal("seeding twice must be harmless")
	}

	sim, err := svc.Similar(ctx, cur.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawReal, sawSim bool
	for _, m := range sim.Matches {
		if m.Record.IncidentID != nil && *m.Record.IncidentID == old.ID {
			sawReal = true
		}
		if m.Record.Simulated {
			sawSim = true
		}
		if m.Record.IncidentID != nil && *m.Record.IncidentID == cur.ID {
			t.Fatal("an incident must not match itself")
		}
	}
	if !sawReal || !sawSim || sim.SimulatedCount != len(Seeds()) {
		t.Fatalf("real=%v simulated=%v counts=%d/%d", sawReal, sawSim, sim.HistoricalCount, sim.SimulatedCount)
	}
	events, _ := st.ListEvents(ctx, store.EventFilter{IncidentID: &old.ID, Limit: 50})
	n := 0
	for _, e := range events {
		if e.Title == "Saved to incident memory" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one memory event, got %d", n)
	}
	if removed, err := svc.Unseed(ctx); err != nil || removed != int64(len(Seeds())) {
		t.Fatalf("unseed: %d %v", removed, err)
	}
	if err := svc.Sync(ctx); err != nil { // old is already stored; sync must be harmless
		t.Fatal(err)
	}
}
