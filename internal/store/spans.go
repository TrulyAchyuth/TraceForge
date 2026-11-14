package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TrulyAchyuth/TraceForge/internal/hist"
	"github.com/TrulyAchyuth/TraceForge/internal/model"
)

// serviceSep separates service names packed into a single text parameter.
const serviceSep = "\x1f"

type spanKey struct{ trace, span string }

// WriteSpans stores a batch of spans and updates the per-trace summaries and
// service rollups, all in one transaction. Spans already present (same trace
// and span ID) are skipped and do not affect rollups, so re-delivered batches
// are idempotent. It returns the number of newly inserted spans.
func (s *Store) WriteSpans(ctx context.Context, spans []model.Span) (int, error) {
	if len(spans) == 0 {
		return 0, nil
	}
	// De-duplicate within the batch, keeping the first occurrence.
	byKey := make(map[spanKey]*model.Span, len(spans))
	ordered := make([]*model.Span, 0, len(spans))
	for i := range spans {
		k := spanKey{spans[i].TraceID, spans[i].SpanID}
		if _, dup := byKey[k]; dup {
			continue
		}
		byKey[k] = &spans[i]
		ordered = append(ordered, &spans[i])
	}

	n := len(ordered)
	traceIDs, spanIDs, parents := make([]string, n), make([]string, n), make([]string, n)
	services, names := make([]string, n), make([]string, n)
	kinds, statuses, edgeStates := make([]int16, n), make([]int16, n), make([]int16, n)
	starts := make([]time.Time, n)
	durations := make([]int64, n)
	messages, attrs, resources, events := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	peers, errTypes := make([]string, n), make([]string, n)
	resends := make([]int32, n)
	for i, sp := range ordered {
		traceIDs[i], spanIDs[i], parents[i] = sp.TraceID, sp.SpanID, sp.ParentSpanID
		services[i], names[i] = sp.Service, sp.Name
		kinds[i], statuses[i] = int16(sp.Kind), int16(sp.StatusCode)
		starts[i], durations[i] = sp.Start, sp.DurationNs
		messages[i] = sp.StatusMessage
		var err error
		if attrs[i], err = marshalObject(sp.Attributes); err != nil {
			return 0, fmt.Errorf("encode attributes: %w", err)
		}
		if resources[i], err = marshalObject(sp.Resource); err != nil {
			return 0, fmt.Errorf("encode resource: %w", err)
		}
		if len(sp.Events) == 0 {
			events[i] = "[]"
		} else {
			b, err := json.Marshal(sp.Events)
			if err != nil {
				return 0, fmt.Errorf("encode events: %w", err)
			}
			events[i] = string(b)
		}
		peers[i], errTypes[i], resends[i] = sp.PeerService, sp.ErrorType, int32(sp.ResendCount)
		if sp.Kind == model.KindClient {
			edgeStates[i] = 0
		} else {
			edgeStates[i] = 1
		}
	}

	inserted := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			INSERT INTO spans (trace_id, span_id, parent_span_id, service, name, kind, start_time, duration_ns,
				status_code, status_message, attributes, resource, events, peer_service, resend_count, error_type, edge_state)
			SELECT t.a, t.b, t.c, t.d, t.e, t.f, t.g, t.h, t.i, t.j, t.k::jsonb, t.l::jsonb, t.m::jsonb, t.n, t.o, t.p, t.q
			FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::smallint[], $7::timestamptz[],
				$8::bigint[], $9::smallint[], $10::text[], $11::text[], $12::text[], $13::text[], $14::text[],
				$15::int[], $16::text[], $17::smallint[]) AS t(a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q)
			ON CONFLICT (trace_id, span_id) DO NOTHING
			RETURNING trace_id, span_id`,
			traceIDs, spanIDs, parents, services, names, kinds, starts, durations, statuses, messages,
			attrs, resources, events, peers, resends, errTypes, edgeStates)
		if err != nil {
			return fmt.Errorf("insert spans: %w", err)
		}
		var fresh []*model.Span
		for rows.Next() {
			var k spanKey
			if err := rows.Scan(&k.trace, &k.span); err != nil {
				rows.Close()
				return err
			}
			fresh = append(fresh, byKey[k])
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("insert spans: %w", err)
		}
		inserted = len(fresh)
		if inserted == 0 {
			return nil
		}
		if err := upsertTraces(ctx, tx, fresh); err != nil {
			return fmt.Errorf("upsert traces: %w", err)
		}
		if err := upsertServiceStats(ctx, tx, fresh); err != nil {
			return fmt.Errorf("upsert service stats: %w", err)
		}
		return nil
	})
	return inserted, err
}

func marshalObject(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

func upsertTraces(ctx context.Context, tx pgx.Tx, spans []*model.Span) error {
	type agg struct {
		start, end         time.Time
		rootService, rootN string
		count, errs        int32
		services           map[string]struct{}
	}
	byTrace := map[string]*agg{}
	for _, sp := range spans {
		a := byTrace[sp.TraceID]
		if a == nil {
			a = &agg{start: sp.Start, end: sp.End(), services: map[string]struct{}{}}
			byTrace[sp.TraceID] = a
		}
		if sp.Start.Before(a.start) {
			a.start = sp.Start
		}
		if sp.End().After(a.end) {
			a.end = sp.End()
		}
		if sp.ParentSpanID == "" {
			a.rootService, a.rootN = sp.Service, sp.Name
		}
		a.count++
		if sp.IsError() {
			a.errs++
		}
		a.services[sp.Service] = struct{}{}
	}
	ids := make([]string, 0, len(byTrace))
	for id := range byTrace {
		ids = append(ids, id)
	}
	sort.Strings(ids) // stable lock order across concurrent writers
	n := len(ids)
	starts, ends := make([]time.Time, n), make([]time.Time, n)
	rootServices, rootNames, svcLists := make([]string, n), make([]string, n), make([]string, n)
	counts, errs := make([]int32, n), make([]int32, n)
	for i, id := range ids {
		a := byTrace[id]
		starts[i], ends[i] = a.start, a.end
		rootServices[i], rootNames[i] = a.rootService, a.rootN
		counts[i], errs[i] = a.count, a.errs
		list := make([]string, 0, len(a.services))
		for svc := range a.services {
			list = append(list, svc)
		}
		sort.Strings(list)
		svcLists[i] = strings.Join(list, serviceSep)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO traces (trace_id, start_time, end_time, root_service, root_name, span_count, error_count, services)
		SELECT t.id, t.st, t.en, t.rs, t.rn, t.sc, t.ec, string_to_array(t.sv, $9)
		FROM unnest($1::text[], $2::timestamptz[], $3::timestamptz[], $4::text[], $5::text[], $6::int[], $7::int[], $8::text[])
			AS t(id, st, en, rs, rn, sc, ec, sv)
		ON CONFLICT (trace_id) DO UPDATE SET
			start_time = LEAST(traces.start_time, EXCLUDED.start_time),
			end_time = GREATEST(traces.end_time, EXCLUDED.end_time),
			root_service = CASE WHEN EXCLUDED.root_service <> '' THEN EXCLUDED.root_service ELSE traces.root_service END,
			root_name = CASE WHEN EXCLUDED.root_service <> '' THEN EXCLUDED.root_name ELSE traces.root_name END,
			span_count = traces.span_count + EXCLUDED.span_count,
			error_count = traces.error_count + EXCLUDED.error_count,
			services = ARRAY(SELECT DISTINCT u FROM unnest(traces.services || EXCLUDED.services) AS u ORDER BY u)`,
		ids, starts, ends, rootServices, rootNames, counts, errs, svcLists, serviceSep)
	return err
}

type statKey struct {
	bucket  time.Time
	service string
}

type statAgg struct {
	requests, errors, sum int64
	hist                  []int64
}

func upsertServiceStats(ctx context.Context, tx pgx.Tx, spans []*model.Span) error {
	aggs := map[statKey]*statAgg{}
	for _, sp := range spans {
		if !sp.IsEntry() {
			continue
		}
		k := statKey{hist.TruncateBucket(sp.Start), sp.Service}
		a := aggs[k]
		if a == nil {
			a = &statAgg{hist: hist.New()}
			aggs[k] = a
		}
		a.requests++
		if sp.IsError() {
			a.errors++
		}
		a.sum += sp.DurationNs
		a.hist[hist.Index(sp.DurationNs)]++
	}
	if len(aggs) == 0 {
		return nil
	}
	keys := make([]statKey, 0, len(aggs))
	for k := range aggs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if !keys[i].bucket.Equal(keys[j].bucket) {
			return keys[i].bucket.Before(keys[j].bucket)
		}
		return keys[i].service < keys[j].service
	})
	n := len(keys)
	buckets, services, hists := make([]time.Time, n), make([]string, n), make([]string, n)
	reqs, errs, sums := make([]int64, n), make([]int64, n), make([]int64, n)
	for i, k := range keys {
		a := aggs[k]
		buckets[i], services[i] = k.bucket, k.service
		reqs[i], errs[i], sums[i], hists[i] = a.requests, a.errors, a.sum, histLiteral(a.hist)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO service_stats (bucket, service, requests, errors, duration_sum_ns, hist)
		SELECT t.b, t.s, t.r, t.e, t.d, t.h::bigint[]
		FROM unnest($1::timestamptz[], $2::text[], $3::bigint[], $4::bigint[], $5::bigint[], $6::text[]) AS t(b, s, r, e, d, h)
		ON CONFLICT (bucket, service) DO UPDATE SET
			requests = service_stats.requests + EXCLUDED.requests,
			errors = service_stats.errors + EXCLUDED.errors,
			duration_sum_ns = service_stats.duration_sum_ns + EXCLUDED.duration_sum_ns,
			hist = tf_hist_add(service_stats.hist, EXCLUDED.hist)`,
		buckets, services, reqs, errs, sums, hists)
	return err
}

// histLiteral renders a histogram as a PostgreSQL array literal.
func histLiteral(h []int64) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range h {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(v, 10))
	}
	b.WriteByte('}')
	return b.String()
}

// ResolveEdges pairs pending CLIENT spans with the service that handled them
// and folds them into the edge rollups. A client span is paired with a child
// span from a different service; when no such span exists (callee down or not
// instrumented) the span's own peer attribute names the callee. Spans younger
// than grace are left pending so that late-arriving server spans can pair.
// It returns the number of client spans processed.
func (s *Store) ResolveEdges(ctx context.Context, grace time.Duration, limit int) (int, error) {
	processed := 0
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT s.trace_id, s.span_id, s.service, s.start_time, s.duration_ns, s.status_code, s.resend_count,
				COALESCE((SELECT c.service FROM spans c
					WHERE c.trace_id = s.trace_id AND c.parent_span_id = s.span_id AND c.service <> s.service
					LIMIT 1), s.peer_service) AS callee
			FROM spans s
			WHERE s.kind = 3 AND s.edge_state = 0 AND s.ingested_at < now() - make_interval(secs => $1)
			ORDER BY s.ingested_at
			LIMIT $2
			FOR UPDATE OF s SKIP LOCKED`, grace.Seconds(), limit)
		if err != nil {
			return err
		}
		type edgeKey struct {
			bucket         time.Time
			caller, callee string
		}
		type edgeAgg struct {
			calls, errors, retries, sum int64
			hist                        []int64
		}
		aggs := map[edgeKey]*edgeAgg{}
		var doneTrace, doneSpan []string
		var doneState []int16
		for rows.Next() {
			var traceID, spanID, caller, callee string
			var start time.Time
			var dur int64
			var status int16
			var resend int32
			if err := rows.Scan(&traceID, &spanID, &caller, &start, &dur, &status, &resend, &callee); err != nil {
				rows.Close()
				return err
			}
			doneTrace, doneSpan = append(doneTrace, traceID), append(doneSpan, spanID)
			if callee == "" || callee == caller {
				doneState = append(doneState, 2)
				continue
			}
			doneState = append(doneState, 1)
			k := edgeKey{hist.TruncateBucket(start), caller, callee}
			a := aggs[k]
			if a == nil {
				a = &edgeAgg{hist: hist.New()}
				aggs[k] = a
			}
			a.calls++
			if status == model.StatusError {
				a.errors++
			}
			if resend > 0 {
				a.retries++
			}
			a.sum += dur
			a.hist[hist.Index(dur)]++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		processed = len(doneTrace)
		if processed == 0 {
			return nil
		}
		if len(aggs) > 0 {
			keys := make([]edgeKey, 0, len(aggs))
			for k := range aggs {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := keys[i], keys[j]
				if !a.bucket.Equal(b.bucket) {
					return a.bucket.Before(b.bucket)
				}
				if a.caller != b.caller {
					return a.caller < b.caller
				}
				return a.callee < b.callee
			})
			n := len(keys)
			buckets, callers, callees, hists := make([]time.Time, n), make([]string, n), make([]string, n), make([]string, n)
			calls, errs, retries, sums := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
			for i, k := range keys {
				a := aggs[k]
				buckets[i], callers[i], callees[i] = k.bucket, k.caller, k.callee
				calls[i], errs[i], retries[i], sums[i], hists[i] = a.calls, a.errors, a.retries, a.sum, histLiteral(a.hist)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO edge_stats (bucket, caller, callee, calls, errors, retries, duration_sum_ns, hist)
				SELECT t.b, t.c1, t.c2, t.n, t.e, t.r, t.d, t.h::bigint[]
				FROM unnest($1::timestamptz[], $2::text[], $3::text[], $4::bigint[], $5::bigint[], $6::bigint[], $7::bigint[], $8::text[])
					AS t(b, c1, c2, n, e, r, d, h)
				ON CONFLICT (bucket, caller, callee) DO UPDATE SET
					calls = edge_stats.calls + EXCLUDED.calls,
					errors = edge_stats.errors + EXCLUDED.errors,
					retries = edge_stats.retries + EXCLUDED.retries,
					duration_sum_ns = edge_stats.duration_sum_ns + EXCLUDED.duration_sum_ns,
					hist = tf_hist_add(edge_stats.hist, EXCLUDED.hist)`,
				buckets, callers, callees, calls, errs, retries, sums, hists); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			UPDATE spans SET edge_state = t.st
			FROM unnest($1::text[], $2::text[], $3::smallint[]) AS t(tid, sid, st)
			WHERE spans.trace_id = t.tid AND spans.span_id = t.sid`, doneTrace, doneSpan, doneState)
		return err
	})
	return processed, err
}

// ServiceStats returns service rollup buckets in [from, to).
func (s *Store) ServiceStats(ctx context.Context, from, to time.Time) ([]model.StatBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT bucket, service, requests, errors, duration_sum_ns, hist
		FROM service_stats WHERE bucket >= $1 AND bucket < $2 ORDER BY bucket, service`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.StatBucket, error) {
		var b model.StatBucket
		err := row.Scan(&b.Bucket, &b.Service, &b.Requests, &b.Errors, &b.DurationSumNs, &b.Hist)
		b.Bucket = b.Bucket.UTC()
		return b, err
	})
}

// EdgeStats returns edge rollup buckets in [from, to).
func (s *Store) EdgeStats(ctx context.Context, from, to time.Time) ([]model.EdgeBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT bucket, caller, callee, calls, errors, retries, duration_sum_ns, hist
		FROM edge_stats WHERE bucket >= $1 AND bucket < $2 ORDER BY bucket, caller, callee`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.EdgeBucket, error) {
		var b model.EdgeBucket
		err := row.Scan(&b.Bucket, &b.Caller, &b.Callee, &b.Calls, &b.Errors, &b.Retries, &b.DurationSumNs, &b.Hist)
		b.Bucket = b.Bucket.UTC()
		return b, err
	})
}

// TraceFilter selects traces for search.
type TraceFilter struct {
	Service       string
	Status        string // "", "error" or "ok"
	MinDurationNs int64
	MaxDurationNs int64
	From, To      time.Time
	Limit         int
	Cursor        string
}

// ErrBadCursor is returned for a pagination cursor that cannot be decoded.
var ErrBadCursor = errors.New("invalid cursor")

// SearchTraces returns trace summaries newest first, with keyset pagination.
func (s *Store) SearchTraces(ctx context.Context, f TraceFilter) ([]model.TraceSummary, string, error) {
	where := []string{"start_time >= $1", "start_time < $2"}
	args := []any{f.From, f.To}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Service != "" {
		add("services @> ARRAY[$%d::text]", f.Service)
	}
	switch f.Status {
	case "error":
		where = append(where, "error_count > 0")
	case "ok":
		where = append(where, "error_count = 0")
	}
	if f.MinDurationNs > 0 {
		add("(EXTRACT(EPOCH FROM (end_time - start_time)) * 1e9) >= $%d", f.MinDurationNs)
	}
	if f.MaxDurationNs > 0 {
		add("(EXTRACT(EPOCH FROM (end_time - start_time)) * 1e9) <= $%d", f.MaxDurationNs)
	}
	if f.Cursor != "" {
		ts, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", ErrBadCursor
		}
		args = append(args, ts, id)
		where = append(where, fmt.Sprintf("(start_time, trace_id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, `
		SELECT trace_id, start_time, (EXTRACT(EPOCH FROM (end_time - start_time)) * 1e9)::bigint,
			root_service, root_name, span_count, error_count, services
		FROM traces WHERE `+strings.Join(where, " AND ")+`
		ORDER BY start_time DESC, trace_id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, "", err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.TraceSummary, error) {
		var t model.TraceSummary
		err := row.Scan(&t.TraceID, &t.Start, &t.DurationNs, &t.RootService, &t.RootName, &t.SpanCount, &t.ErrorCount, &t.Services)
		t.Start = t.Start.UTC()
		return t, err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > f.Limit {
		out = out[:f.Limit]
		last := out[len(out)-1]
		next = encodeCursor(last.Start, last.TraceID)
	}
	return out, next, nil
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", err
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	ns, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, "", err
	}
	return time.Unix(0, ns).UTC(), id, nil
}

// MaxTraceSpans caps how many spans are returned for one trace.
const MaxTraceSpans = 5000

// GetTrace returns the spans of a trace ordered by start time. When the live
// spans have been purged by retention it falls back to any copy preserved in
// an incident dataset; preserved reports whether that fallback was used.
func (s *Store) GetTrace(ctx context.Context, traceID string) (spans []model.Span, preserved bool, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT trace_id, span_id, parent_span_id, service, name, kind, start_time, duration_ns, status_code,
			status_message, attributes, resource, events, peer_service, resend_count, error_type
		FROM spans WHERE trace_id = $1 ORDER BY start_time, span_id LIMIT $2`, traceID, MaxTraceSpans)
	if err != nil {
		return nil, false, err
	}
	spans, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Span, error) {
		var sp model.Span
		var kind, status int16
		var resend int32
		var attrs, resource, events []byte
		if err := row.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &sp.Service, &sp.Name, &kind, &sp.Start,
			&sp.DurationNs, &status, &sp.StatusMessage, &attrs, &resource, &events, &sp.PeerService, &resend, &sp.ErrorType); err != nil {
			return sp, err
		}
		sp.Kind, sp.StatusCode, sp.ResendCount, sp.Start = int(kind), int(status), int(resend), sp.Start.UTC()
		if err := json.Unmarshal(attrs, &sp.Attributes); err != nil {
			return sp, err
		}
		if err := json.Unmarshal(resource, &sp.Resource); err != nil {
			return sp, err
		}
		return sp, json.Unmarshal(events, &sp.Events)
	})
	if err != nil {
		return nil, false, err
	}
	if len(spans) > 0 {
		return spans, false, nil
	}
	rows, err = s.pool.Query(ctx, `
		SELECT DISTINCT ON (span_id) `+datasetSpanCols+`
		FROM dataset_spans WHERE trace_id = $1 ORDER BY span_id, dataset_id DESC LIMIT $2`, traceID, MaxTraceSpans)
	if err != nil {
		return nil, false, err
	}
	ds, err := pgx.CollectRows(rows, scanDatasetSpan)
	if err != nil {
		return nil, false, err
	}
	if len(ds) == 0 {
		return nil, false, ErrNotFound
	}
	for _, d := range ds {
		spans = append(spans, d.Span)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
	return spans, true, nil
}

// ExemplarTraces returns up to limit trace IDs of entry spans for a service
// in [from, to): error spans when errorsOnly is set, otherwise the slowest.
func (s *Store) ExemplarTraces(ctx context.Context, service string, from, to time.Time, errorsOnly bool, limit int) ([]string, error) {
	q := `SELECT trace_id FROM spans WHERE service = $1 AND start_time >= $2 AND start_time < $3
		AND (kind IN (2, 5) OR parent_span_id = '')`
	if errorsOnly {
		q += ` AND status_code = 2 ORDER BY start_time DESC LIMIT $4`
	} else {
		q += ` ORDER BY duration_ns DESC LIMIT $4`
	}
	rows, err := s.pool.Query(ctx, q, service, from, to, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return dedupe(ids), nil
}

// ExemplarEdgeTraces returns trace IDs of client calls from caller that name
// callee (by pairing or peer attribute) in [from, to), errors first.
func (s *Store) ExemplarEdgeTraces(ctx context.Context, caller, callee string, from, to time.Time, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.trace_id FROM spans s
		WHERE s.service = $1 AND s.kind = 3 AND s.start_time >= $3 AND s.start_time < $4
			AND (s.peer_service = $2 OR EXISTS (SELECT 1 FROM spans c
				WHERE c.trace_id = s.trace_id AND c.parent_span_id = s.span_id AND c.service = $2))
		ORDER BY (s.status_code = 2) DESC, s.duration_ns DESC LIMIT $5`, caller, callee, from, to, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return dedupe(ids), nil
}

func dedupe(ids []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// TelemetryTotals reports the stored span and trace counts and time bounds.
type TelemetryTotals struct {
	Spans      int64      `json:"spans"`
	Traces     int64      `json:"traces"`
	OldestSpan *time.Time `json:"oldest_span,omitempty"`
	NewestSpan *time.Time `json:"newest_span,omitempty"`
}

// Totals returns stored telemetry totals. The counts are exact, which is
// affordable at the scale a single-node deployment targets.
func (s *Store) Totals(ctx context.Context) (TelemetryTotals, error) {
	var t TelemetryTotals
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM spans), (SELECT count(*) FROM traces),
		(SELECT min(start_time) FROM spans), (SELECT max(start_time) FROM spans)`).Scan(&t.Spans, &t.Traces, &t.OldestSpan, &t.NewestSpan)
	return t, err
}
