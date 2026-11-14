package api

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/TrulyAchyuth/TraceForge/internal/detect"
	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
	"github.com/TrulyAchyuth/TraceForge/internal/store"
)

// stats are request statistics for a service, edge or the whole system.
type stats struct {
	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	ErrorRate  float64 `json:"error_rate"`
	RatePerSec float64 `json:"rate_per_sec"`
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
}

type agg struct {
	requests, errors int64
	hist             []int64
}

func newAgg() *agg { return &agg{hist: hist.New()} }

func (a *agg) add(requests, errors int64, h []int64) {
	a.requests += requests
	a.errors += errors
	hist.Add(a.hist, h)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func (a *agg) stats(seconds float64) stats {
	st := stats{Requests: a.requests, Errors: a.errors}
	if a.requests > 0 {
		st.ErrorRate = float64(a.errors) / float64(a.requests)
		st.P50Ms = round2(hist.Quantile(a.hist, 0.50))
		st.P95Ms = round2(hist.Quantile(a.hist, 0.95))
		st.P99Ms = round2(hist.Quantile(a.hist, 0.99))
	}
	if seconds > 0 {
		st.RatePerSec = round2(float64(a.requests) / seconds)
	}
	return st
}

// point is one step of a time series.
type point struct {
	Time time.Time `json:"t"`
	stats
}

// series buckets rollup rows into steps. Steps with no data are present with
// zero counts so charts show gaps in traffic honestly.
type series struct {
	tr    timeRange
	steps []*agg
}

func newSeries(tr timeRange) *series {
	n := int(math.Ceil(tr.To.Sub(tr.From).Seconds() / float64(tr.StepSeconds)))
	s := &series{tr: tr, steps: make([]*agg, max(n, 1))}
	for i := range s.steps {
		s.steps[i] = newAgg()
	}
	return s
}

func (s *series) add(bucket time.Time, requests, errors int64, h []int64) {
	i := int(bucket.Sub(s.tr.From).Seconds()) / s.tr.StepSeconds
	if i < 0 || i >= len(s.steps) {
		return
	}
	s.steps[i].add(requests, errors, h)
}

func (s *series) points() []point {
	out := make([]point, len(s.steps))
	for i, a := range s.steps {
		out[i] = point{Time: s.tr.From.Add(time.Duration(i*s.tr.StepSeconds) * time.Second), stats: a.stats(float64(s.tr.StepSeconds))}
	}
	return out
}

// loadRollups fetches service and edge rollups for a window aligned to
// bucket boundaries.
func (s *Server) loadRollups(r *http.Request, tr timeRange) ([]model.StatBucket, []model.EdgeBucket, error) {
	from := hist.TruncateBucket(tr.From)
	svc, err := s.opt.Store.ServiceStats(r.Context(), from, tr.To)
	if err != nil {
		return nil, nil, err
	}
	edges, err := s.opt.Store.EdgeStats(r.Context(), from, tr.To)
	return svc, edges, err
}

// entryServices returns the services no other service calls: the points
// where requests enter the traced system. With no edges, every service is one.
func entryServices(svc []model.StatBucket, edges []model.EdgeBucket) []string {
	called := map[string]bool{}
	for _, e := range edges {
		called[e.Callee] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range svc {
		if !called[b.Service] && !seen[b.Service] {
			seen[b.Service] = true
			out = append(out, b.Service)
		}
	}
	sort.Strings(out)
	return out
}

type serviceRow struct {
	Name     string     `json:"name"`
	Status   string     `json:"status"`
	Since    *time.Time `json:"status_since,omitempty"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	stats
	Series []point `json:"series,omitempty"`
}

func (s *Server) serviceRows(r *http.Request, tr timeRange, svc []model.StatBucket, withSeries bool) ([]serviceRow, error) {
	health, err := s.opt.Store.ListServiceHealth(r.Context())
	if err != nil {
		return nil, err
	}
	aggs := map[string]*agg{}
	seriesBy := map[string]*series{}
	for _, b := range svc {
		a := aggs[b.Service]
		if a == nil {
			a = newAgg()
			aggs[b.Service] = a
			seriesBy[b.Service] = newSeries(tr)
		}
		a.add(b.Requests, b.Errors, b.Hist)
		seriesBy[b.Service].add(b.Bucket, b.Requests, b.Errors, b.Hist)
	}
	seconds := tr.To.Sub(tr.From).Seconds()
	rows := map[string]*serviceRow{}
	for name, a := range aggs {
		row := &serviceRow{Name: name, Status: "unknown", stats: a.stats(seconds)}
		if withSeries {
			row.Series = seriesBy[name].points()
		}
		rows[name] = row
	}
	for _, h := range health {
		row := rows[h.Service]
		if row == nil {
			// Known service with no traffic in this window (e.g. it is down).
			row = &serviceRow{Name: h.Service}
			if withSeries {
				row.Series = newSeries(tr).points()
			}
			rows[h.Service] = row
		}
		since, seen := h.Since.UTC(), h.LastSeen.UTC()
		row.Status, row.Since, row.LastSeen = h.Status, &since, &seen
	}
	out := make([]serviceRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	tr, err := s.parseRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
		return
	}
	svc, edges, err := s.loadRollups(r, tr)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	entries := entryServices(svc, edges)
	isEntry := map[string]bool{}
	for _, e := range entries {
		isEntry[e] = true
	}
	total, ser := newAgg(), newSeries(tr)
	for _, b := range svc {
		if isEntry[b.Service] {
			total.add(b.Requests, b.Errors, b.Hist)
			ser.add(b.Bucket, b.Requests, b.Errors, b.Hist)
		}
	}
	rows, err := s.serviceRows(r, tr, svc, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	healthCounts := map[string]int{}
	for _, row := range rows {
		healthCounts[row.Status]++
	}
	active, err := s.opt.Store.ActiveIncidents(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	recent, _, err := s.opt.Store.ListIncidents(r.Context(), store.IncidentFilter{Limit: 6})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	systemHealth := detect.HealthHealthy
	switch {
	case len(rows) == 0:
		systemHealth = "no_data"
	case healthCounts[detect.HealthCritical] > 0:
		systemHealth = detect.HealthCritical
	case healthCounts[detect.HealthDegraded] > 0 || len(active) > 0:
		systemHealth = detect.HealthDegraded
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range":            tr,
		"system_health":    systemHealth,
		"entry_services":   entries,
		"totals":           total.stats(tr.To.Sub(tr.From).Seconds()),
		"series":           ser.points(),
		"service_count":    len(rows),
		"health_counts":    healthCounts,
		"services":         rows,
		"active_incidents": len(active),
		"recent_incidents": nonNilIncidents(recent),
		"ingest":           s.opt.Pipeline.Stats(),
	})
}

func nonNilIncidents(in []model.Incident) []model.Incident {
	if in == nil {
		return []model.Incident{}
	}
	return in
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	tr, err := s.parseRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
		return
	}
	svc, _, err := s.loadRollups(r, tr)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.serviceRows(r, tr, svc, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"range": tr, "services": rows})
}

type edgeRow struct {
	Caller     string  `json:"caller"`
	Callee     string  `json:"callee"`
	Calls      int64   `json:"calls"`
	Errors     int64   `json:"errors"`
	Retries    int64   `json:"retries"`
	ErrorRate  float64 `json:"error_rate"`
	RetryRatio float64 `json:"retry_ratio"`
	RatePerSec float64 `json:"rate_per_sec"`
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	// Change is "new" when the edge did not exist in the preceding window of
	// equal length, "removed" when it existed then but not now, else "".
	Change string `json:"change"`
}

func edgeRows(edges []model.EdgeBucket, seconds float64) map[[2]string]*edgeRow {
	type acc struct {
		row  *edgeRow
		hist []int64
	}
	accs := map[[2]string]*acc{}
	for _, b := range edges {
		k := [2]string{b.Caller, b.Callee}
		a := accs[k]
		if a == nil {
			a = &acc{row: &edgeRow{Caller: b.Caller, Callee: b.Callee}, hist: hist.New()}
			accs[k] = a
		}
		a.row.Calls += b.Calls
		a.row.Errors += b.Errors
		a.row.Retries += b.Retries
		hist.Add(a.hist, b.Hist)
	}
	out := map[[2]string]*edgeRow{}
	for k, a := range accs {
		if a.row.Calls > 0 {
			a.row.ErrorRate = float64(a.row.Errors) / float64(a.row.Calls)
			a.row.RetryRatio = float64(a.row.Retries) / float64(a.row.Calls)
			a.row.P50Ms = round2(hist.Quantile(a.hist, 0.50))
			a.row.P95Ms = round2(hist.Quantile(a.hist, 0.95))
		}
		if seconds > 0 {
			a.row.RatePerSec = round2(float64(a.row.Calls) / seconds)
		}
		out[k] = a.row
	}
	return out
}

func sortedEdgeRows(m map[[2]string]*edgeRow) []edgeRow {
	out := make([]edgeRow, 0, len(m))
	for _, row := range m {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Caller != out[j].Caller {
			return out[i].Caller < out[j].Caller
		}
		return out[i].Callee < out[j].Callee
	})
	return out
}

func (s *Server) handleDependencies(w http.ResponseWriter, r *http.Request) {
	tr, err := s.parseRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
		return
	}
	svc, edges, err := s.loadRollups(r, tr)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	span := tr.To.Sub(tr.From)
	current := edgeRows(edges, span.Seconds())
	prevFrom := hist.TruncateBucket(tr.From.Add(-span))
	prevEdges, err := s.opt.Store.EdgeStats(r.Context(), prevFrom, hist.TruncateBucket(tr.From))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	previous := edgeRows(prevEdges, span.Seconds())
	comparable := len(previous) > 0
	if comparable {
		for k, row := range current {
			if _, existed := previous[k]; !existed {
				row.Change = "new"
			}
		}
		for k, old := range previous {
			if _, exists := current[k]; !exists {
				current[k] = &edgeRow{Caller: old.Caller, Callee: old.Callee, Change: "removed"}
			}
		}
	}
	rows, err := s.serviceRows(r, tr, svc, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Every edge endpoint must be a node, including callees that report no
	// spans of their own (down or uninstrumented).
	known := map[string]bool{}
	for _, row := range rows {
		known[row.Name] = true
	}
	for k := range current {
		for _, name := range k {
			if !known[name] {
				known[name] = true
				rows = append(rows, serviceRow{Name: name, Status: "unknown"})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{
		"range": tr, "nodes": rows, "edges": sortedEdgeRows(current), "compared_with_previous_window": comparable,
	})
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tr, err := s.parseRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
		return
	}
	svc, edges, err := s.loadRollups(r, tr)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.serviceRows(r, tr, svc, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var row *serviceRow
	for i := range rows {
		if rows[i].Name == name {
			row = &rows[i]
		}
	}
	all := sortedEdgeRows(edgeRows(edges, tr.To.Sub(tr.From).Seconds()))
	incoming, outgoing := []edgeRow{}, []edgeRow{}
	for _, e := range all {
		if e.Callee == name {
			incoming = append(incoming, e)
		}
		if e.Caller == name {
			outgoing = append(outgoing, e)
		}
	}
	if row == nil && len(incoming) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no telemetry for this service in the selected range")
		return
	}
	if row == nil {
		row = &serviceRow{Name: name, Status: "unknown", Series: newSeries(tr).points()}
	}
	incidents, _, err := s.opt.Store.ListIncidents(r.Context(), store.IncidentFilter{Limit: 50})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	related := []model.Incident{}
	for _, in := range incidents {
		for _, a := range in.AffectedServices {
			if a == name && len(related) < 8 {
				related = append(related, in)
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": tr, "service": row, "incoming": incoming, "outgoing": outgoing, "incidents": related,
	})
}

var traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Trace search page-size bounds.
const (
	defaultTraceLimit = 50
	maxTraceLimit     = 200
)

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	tr, err := s.parseRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_range", err.Error())
		return
	}
	q := r.URL.Query()
	f := store.TraceFilter{Service: q.Get("service"), Status: q.Get("status"), From: tr.From, To: tr.To, Cursor: q.Get("cursor")}
	if f.Status != "" && f.Status != "error" && f.Status != "ok" {
		writeError(w, http.StatusBadRequest, "invalid_filter", "status must be error or ok")
		return
	}
	if f.Limit, err = intParam(r, "limit", defaultTraceLimit, 1, maxTraceLimit); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	for name, dst := range map[string]*int64{"min_duration_ms": &f.MinDurationNs, "max_duration_ms": &f.MaxDurationNs} {
		raw := q.Get(name)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v > 3.6e6 || math.IsNaN(v) {
			writeError(w, http.StatusBadRequest, "invalid_filter", name+" must be a number of milliseconds between 0 and 3600000")
			return
		}
		*dst = int64(v * 1e6)
	}
	if f.MaxDurationNs > 0 && f.MinDurationNs > f.MaxDurationNs {
		writeError(w, http.StatusBadRequest, "invalid_filter", "min_duration_ms must not exceed max_duration_ms")
		return
	}
	list, next, err := s.opt.Store.SearchTraces(r.Context(), f)
	if err == store.ErrBadCursor {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is not valid")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if list == nil {
		list = []model.TraceSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"range": tr, "traces": list, "next_cursor": next})
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !traceIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "trace ID must be 32 lowercase hexadecimal characters")
		return
	}
	spans, preserved, err := s.opt.Store.GetTrace(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	start, end := spans[0].Start, spans[0].End()
	services := map[string]bool{}
	errorsN, roots := 0, 0
	ids := map[string]bool{}
	for i := range spans {
		ids[spans[i].SpanID] = true
	}
	orphans := 0
	for i := range spans {
		sp := &spans[i]
		if sp.Start.Before(start) {
			start = sp.Start
		}
		if sp.End().After(end) {
			end = sp.End()
		}
		services[sp.Service] = true
		if sp.IsError() {
			errorsN++
		}
		switch {
		case sp.ParentSpanID == "":
			roots++
		case !ids[sp.ParentSpanID]:
			orphans++
		}
	}
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{
		"trace_id": id, "start_time": start, "duration_ns": end.Sub(start).Nanoseconds(), "span_count": len(spans),
		"error_count": errorsN, "services": names, "root_count": roots, "orphan_count": orphans,
		"truncated": len(spans) >= store.MaxTraceSpans, "preserved_copy": preserved, "spans": spans,
	})
}

func (s *Server) handleIngestStats(w http.ResponseWriter, r *http.Request) {
	totals, err := s.opt.Store.Totals(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pipeline": s.opt.Pipeline.Stats(), "stored": totals})
}
