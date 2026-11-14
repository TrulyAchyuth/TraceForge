package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TrulyAchyuth/TraceForge/internal/detect"
	"github.com/TrulyAchyuth/TraceForge/internal/investigate"
	"github.com/TrulyAchyuth/TraceForge/internal/lab"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/replay"
	"github.com/TrulyAchyuth/TraceForge/internal/report"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

// Bounds on operator-supplied content and per-incident growth.
const (
	maxNoteLen           = 4000
	maxAuthorLen         = 80
	maxDatasetsPerInc    = 20
	maxReplaysPerInc     = 200
	defaultIncidentLimit = 50
	maxIncidentLimit     = 200
	defaultEventLimit    = 200
	maxEventLimit        = 500
)

var incidentStatuses = map[string]bool{
	model.IncidentOpen: true, model.IncidentAcknowledged: true, model.IncidentInvestigating: true,
	model.IncidentResolved: true, model.IncidentClosed: true,
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	f := store.IncidentFilter{}
	var err error
	if f.Limit, err = intParam(r, "limit", defaultIncidentLimit, 1, maxIncidentLimit); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	if f.Offset, err = intParam(r, "offset", 0, 0, 1_000_000); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	switch status := r.URL.Query().Get("status"); status {
	case "":
	case "active":
		f.Statuses = []string{model.IncidentOpen, model.IncidentAcknowledged, model.IncidentInvestigating}
	default:
		for _, st := range strings.Split(status, ",") {
			if !incidentStatuses[st] {
				writeError(w, http.StatusBadRequest, "invalid_filter", "unknown status "+strconv.Quote(st))
				return
			}
			f.Statuses = append(f.Statuses, st)
		}
	}
	list, total, err := s.opt.Store.ListIncidents(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": nonNilIncidents(list), "total": total, "limit": f.Limit, "offset": f.Offset})
}

func (s *Server) handleIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	inc, err := s.opt.Store.GetIncident(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	events, err := s.opt.Store.ListEvents(r.Context(), store.EventFilter{IncidentID: &id, Limit: maxEventLimit, Ascending: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incident": inc, "history": events, "rules": detect.RuleDescriptions})
}

func (s *Server) handleIncidentUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !incidentStatuses[body.Status] {
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be one of open, acknowledged, investigating, resolved, closed")
		return
	}
	inc, err := s.opt.Store.GetIncident(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if inc.Status == body.Status {
		writeJSON(w, http.StatusOK, map[string]any{"incident": inc})
		return
	}
	now := s.now().UTC()
	if err := s.opt.Store.SetIncidentStatus(r.Context(), id, body.Status, now); err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(r.Context(), model.Event{Time: now, Kind: model.EventInvestigation, IncidentID: &id, Service: inc.PrimaryService,
		Title:  fmt.Sprintf("Status changed from %s to %s by operator", inc.Status, body.Status),
		Detail: mustJSON(map[string]string{"from": inc.Status, "to": body.Status})})
	if model.IncidentActive(inc.Status) && !model.IncidentActive(body.Status) {
		// Capture the final picture now that the incident is over. This runs
		// detached from the request so a slow capture cannot fail the update.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := s.opt.Investigations.Build(ctx, id, "incident "+body.Status+" by operator"); err != nil {
				s.opt.Log.Error("investigation build failed", "incident_id", id, "error", err)
			}
		}()
	}
	inc, err = s.opt.Store.GetIncident(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incident": inc})
}

func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	body.Body, body.Author = strings.TrimSpace(body.Body), strings.TrimSpace(body.Author)
	if body.Author == "" {
		body.Author = "operator"
	}
	switch {
	case body.Body == "":
		writeError(w, http.StatusBadRequest, "invalid_note", "note body must not be empty")
		return
	case utf8.RuneCountInString(body.Body) > maxNoteLen:
		writeError(w, http.StatusBadRequest, "invalid_note", fmt.Sprintf("note body must be at most %d characters", maxNoteLen))
		return
	case utf8.RuneCountInString(body.Author) > maxAuthorLen:
		writeError(w, http.StatusBadRequest, "invalid_note", fmt.Sprintf("author must be at most %d characters", maxAuthorLen))
		return
	}
	if _, err := s.opt.Store.GetIncident(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	note, err := s.opt.Store.AddNote(r.Context(), id, body.Author, body.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	preview := body.Body
	if utf8.RuneCountInString(preview) > 120 {
		preview = string([]rune(preview)[:120]) + "…"
	}
	s.event(r.Context(), model.Event{Time: s.now().UTC(), Kind: model.EventInvestigation, IncidentID: &id,
		Title: fmt.Sprintf("Note added by %s: %s", body.Author, preview)})
	writeJSON(w, http.StatusCreated, map[string]any{"note": note})
}

func (s *Server) handleInvestigation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	view, err := s.opt.Investigations.Load(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleInvestigationRebuild(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	existing, err := s.opt.Store.ListDatasets(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(existing) >= maxDatasetsPerInc {
		writeError(w, http.StatusConflict, "dataset_limit",
			fmt.Sprintf("this incident already has %d dataset versions, the maximum", maxDatasetsPerInc))
		return
	}
	if err := s.opt.Investigations.Build(r.Context(), id, "requested by operator"); err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.opt.Investigations.Load(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) handleReplays(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.opt.Store.GetIncident(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	runs, err := s.opt.Store.ListReplays(r.Context(), id, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replays": runs})
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.opt.Store.GetReplay(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replay": run})
}

func (s *Server) handleReplayRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		replay.Scenario
		DatasetID int64 `json:"dataset_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	inc, err := s.opt.Store.GetIncident(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	existing, err := s.opt.Store.ListReplays(r.Context(), id, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(existing) >= maxReplaysPerInc {
		writeError(w, http.StatusConflict, "replay_limit", fmt.Sprintf("this incident already has %d replays, the maximum", maxReplaysPerInc))
		return
	}
	var ds *model.Dataset
	if body.DatasetID > 0 {
		ds, err = s.opt.Store.LoadDataset(r.Context(), body.DatasetID)
		if err == nil && ds.IncidentID != id {
			err = store.ErrNotFound
		}
	} else {
		ds, err = s.opt.Investigations.LatestDataset(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "no_dataset", "no telemetry dataset has been captured for this incident yet; capture one from the investigation view first")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The dataset must still be exactly what was captured.
	if store.Checksum(ds.Spans) != ds.Checksum {
		writeError(w, http.StatusConflict, "dataset_integrity", investigate.ErrIntegrity.Error())
		return
	}
	sc := body.Scenario
	sc.Name = strings.TrimSpace(sc.Name)
	if sc.Name == "" {
		sc.Name = fmt.Sprintf("Scenario %d", len(existing)+1)
	}
	started := time.Now()
	res, err := replay.Run(ds, sc)
	if replay.IsValidation(err) {
		writeError(w, http.StatusBadRequest, "invalid_scenario", err.Error())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Prove isolation: the replay must not have altered its input.
	if store.Checksum(ds.Spans) != ds.Checksum {
		s.opt.Log.Error("replay mutated its dataset; discarding result", "dataset_id", ds.ID)
		writeError(w, http.StatusInternalServerError, "internal", "replay isolation check failed; the result was discarded")
		return
	}
	resultJSON, err := json.Marshal(res)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ivJSON, err := json.Marshal(res.Scenario.Interventions)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	run, err := s.opt.Store.SaveReplay(r.Context(), model.ReplayRun{IncidentID: id, DatasetID: ds.ID, DatasetVersion: ds.Version,
		Name: sc.Name, Interventions: ivJSON, Seed: sc.Seed, EngineVersion: replay.EngineVersion,
		DatasetChecksum: ds.Checksum, Result: resultJSON, DurationMs: time.Since(started).Milliseconds()})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if inc.ResolvedAt != nil { // keep the remembered interventions current
		if err := s.opt.Memory.Snapshot(r.Context(), id); err != nil {
			s.opt.Log.Warn("refresh incident memory", "incident", id, "error", err)
		}
	}
	sum := res.Summary
	s.event(r.Context(), model.Event{Time: s.now().UTC(), Kind: model.EventReplay, IncidentID: &id, ReplayID: &run.ID,
		Service: inc.PrimaryService,
		Title: fmt.Sprintf("Replay \"%s\" on dataset v%d: simulated error rate %.1f%% (recorded %.1f%%), p95 %.0f ms (recorded %.0f ms)",
			sc.Name, ds.Version, sum.Counterfactual.ErrorRate*100, sum.Original.ErrorRate*100, sum.Counterfactual.P95Ms, sum.Original.P95Ms),
		Detail: mustJSON(map[string]any{"interventions": res.Scenario.Interventions, "seed": sc.Seed, "dataset_version": ds.Version})})
	writeJSON(w, http.StatusCreated, map[string]any{"replay": run})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "md"
	}
	types := map[string]string{"md": "text/markdown; charset=utf-8", "html": "text/html; charset=utf-8", "json": "application/json; charset=utf-8"}
	contentType, known := types[format]
	if !known {
		writeError(w, http.StatusBadRequest, "invalid_format", "format must be md, html or json")
		return
	}
	view, err := s.opt.Investigations.Load(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	runs, err := s.opt.Store.ListReplays(r.Context(), id, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := s.now().UTC()
	rep, err := report.Build(view, runs, now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if sim, err := s.opt.Memory.Similar(r.Context(), id); err == nil {
		for _, m := range sim.Matches {
			sec := report.SimilarSection{Key: m.Record.Key, Title: m.Record.Title, Simulated: m.Record.Simulated,
				Overlap: m.Overlap, Shared: m.Shared, Differences: m.Differences}
			for _, iv := range m.Interventions {
				sec.Interventions = append(sec.Interventions, fmt.Sprintf("%s: %s — %s (%s)", iv.Name, iv.Effect, iv.Summary, iv.Class))
			}
			rep.Similar = append(rep.Similar, sec)
		}
	} else {
		s.opt.Log.Warn("report: incident memory unavailable", "incident", id, "error", err)
	}
	var data []byte
	switch format {
	case "md":
		data, err = rep.Markdown()
	case "html":
		data, err = rep.HTML()
	default:
		data, err = rep.JSON()
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(r.Context(), model.Event{Time: now, Kind: model.EventReport, IncidentID: &id,
		Title: fmt.Sprintf("Incident report generated (%s, %d bytes)", format, len(data))})
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-report.%s"`, view.Incident.Key, format))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.EventFilter{Service: q.Get("service")}
	var err error
	if f.Limit, err = intParam(r, "limit", defaultEventLimit, 1, maxEventLimit); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	if raw := q.Get("incident_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_filter", "incident_id must be a positive integer")
			return
		}
		f.IncidentID = &id
	}
	if raw := q.Get("before"); raw != "" {
		if f.BeforeID, err = strconv.ParseInt(raw, 10, 64); err != nil || f.BeforeID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_filter", "before must be a positive integer")
			return
		}
	}
	if raw := q.Get("kind"); raw != "" {
		f.Kinds = strings.Split(raw, ",")
	}
	if q.Get("range") != "" || q.Get("from") != "" {
		tr, err := s.parseRange(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
			return
		}
		f.From, f.To = tr.From, tr.To
	}
	events, err := s.opt.Store.ListEvents(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if events == nil {
		events = []model.Event{}
	}
	var next int64
	if len(events) == f.Limit {
		next = events[len(events)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_before": next})
}

func (s *Server) handleLab(w http.ResponseWriter, r *http.Request) {
	exps, err := s.opt.Store.ListExperiments(r.Context(), 25)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if exps == nil {
		exps = []model.Experiment{}
	}
	out := map[string]any{"enabled": s.opt.Lab.Enabled(), "scenarios": lab.Scenarios(), "experiments": exps,
		"targets": []lab.TargetStatus{}, "min_duration_seconds": lab.MinDuration, "max_duration_seconds": lab.MaxDuration}
	if s.opt.Lab.Enabled() {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		out["targets"] = s.opt.Lab.Status(ctx)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) labError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, lab.ErrDisabled):
		writeError(w, http.StatusConflict, "lab_disabled", err.Error())
	case errors.Is(err, lab.ErrBusy):
		writeError(w, http.StatusConflict, "lab_busy", err.Error())
	case errors.Is(err, lab.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_experiment", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such experiment")
	default:
		s.opt.Log.Error("lab request failed", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusBadGateway, "lab_target_error", "a lab target could not be configured: "+err.Error())
	}
}

func (s *Server) handleLabStart(w http.ResponseWriter, r *http.Request) {
	var req lab.StartRequest
	if !decodeBody(w, r, &req) {
		return
	}
	exp, err := s.opt.Lab.Start(r.Context(), req)
	if err != nil {
		s.labError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"experiment": exp})
}

func (s *Server) handleLabStop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	exp, err := s.opt.Lab.Stop(r.Context(), id, "stopped by operator")
	if err != nil {
		s.labError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"experiment": exp})
}

func (s *Server) handleLabReset(w http.ResponseWriter, r *http.Request) {
	if err := s.opt.Lab.Reset(r.Context()); err != nil {
		s.labError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "all lab targets reset"})
}

func (s *Server) handleDetectionGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"config": s.opt.Engine.Config(), "defaults": detect.DefaultConfig(),
		"rules": detect.RuleDescriptions})
}

func (s *Server) handleDetectionPut(w http.ResponseWriter, r *http.Request) {
	var cfg detect.Config
	if !decodeBody(w, r, &cfg) {
		return
	}
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	if err := s.opt.Engine.SetConfig(r.Context(), cfg); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": cfg})
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	totals, err := s.opt.Store.Totals(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	migrations, err := s.opt.Store.AppliedMigrations(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	lastTick, lastErr := s.opt.Engine.Status()
	var tick *time.Time
	if !lastTick.IsZero() {
		tick = &lastTick
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    Version,
		"started_at": s.started,
		"auth": map[string]bool{
			"api_token_required":    s.opt.Token != "",
			"ingest_token_required": s.opt.IngestToken,
		},
		"retention_hours": s.opt.RetentionHours,
		"detection": map[string]any{
			"interval_seconds": s.opt.DetectInterval.Seconds(), "last_evaluation": tick, "last_error": lastErr,
		},
		"lab":           map[string]any{"enabled": s.opt.Lab.Enabled(), "targets": s.opt.Lab.TargetNames()},
		"replay_engine": replay.EngineVersion,
		"migrations":    migrations,
		"stored":        totals,
		"data_source":   "Live telemetry received over OTLP. TraceForge ships no built-in demo data; the bundled sample application generates real spans.",
	})
}

func (s *Server) event(ctx context.Context, ev model.Event) {
	if _, err := s.opt.Store.AddEvent(ctx, ev); err != nil {
		s.opt.Log.Warn("record timeline event failed", "kind", ev.Kind, "error", err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
