-- TraceForge initial schema.

-- Element-wise addition of two equal-length histograms, used by rollup upserts.
CREATE FUNCTION tf_hist_add(a BIGINT[], b BIGINT[]) RETURNS BIGINT[] AS $$
    SELECT CASE
        WHEN a IS NULL THEN b
        WHEN b IS NULL THEN a
        ELSE ARRAY(SELECT x + y FROM unnest(a, b) WITH ORDINALITY AS t(x, y, i) ORDER BY i)
    END
$$ LANGUAGE sql IMMUTABLE;

-- Raw spans. Subject to retention.
CREATE TABLE spans (
    trace_id        TEXT        NOT NULL,
    span_id         TEXT        NOT NULL,
    parent_span_id  TEXT        NOT NULL DEFAULT '',
    service         TEXT        NOT NULL,
    name            TEXT        NOT NULL,
    kind            SMALLINT    NOT NULL,
    start_time      TIMESTAMPTZ NOT NULL,
    duration_ns     BIGINT      NOT NULL,
    status_code     SMALLINT    NOT NULL,
    status_message  TEXT        NOT NULL DEFAULT '',
    attributes      JSONB       NOT NULL DEFAULT '{}',
    resource        JSONB       NOT NULL DEFAULT '{}',
    events          JSONB       NOT NULL DEFAULT '[]',
    peer_service    TEXT        NOT NULL DEFAULT '',
    resend_count    INT         NOT NULL DEFAULT 0,
    error_type      TEXT        NOT NULL DEFAULT '',
    -- 0 = client span awaiting edge resolution, 1 = resolved, 2 = no callee found
    edge_state      SMALLINT    NOT NULL DEFAULT 0,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (trace_id, span_id)
);
CREATE INDEX spans_start_idx ON spans (start_time);
CREATE INDEX spans_service_start_idx ON spans (service, start_time);
CREATE INDEX spans_parent_idx ON spans (trace_id, parent_span_id);
CREATE INDEX spans_edge_pending_idx ON spans (ingested_at) WHERE kind = 3 AND edge_state = 0;

-- One row per trace, maintained at ingest, for trace search.
CREATE TABLE traces (
    trace_id      TEXT        PRIMARY KEY,
    start_time    TIMESTAMPTZ NOT NULL,
    end_time      TIMESTAMPTZ NOT NULL,
    root_service  TEXT        NOT NULL DEFAULT '',
    root_name     TEXT        NOT NULL DEFAULT '',
    span_count    INT         NOT NULL DEFAULT 0,
    error_count   INT         NOT NULL DEFAULT 0,
    services      TEXT[]      NOT NULL DEFAULT '{}'
);
CREATE INDEX traces_start_idx ON traces (start_time DESC, trace_id);
CREATE INDEX traces_services_idx ON traces USING GIN (services);

-- 10-second rollup of entry spans per service.
CREATE TABLE service_stats (
    bucket           TIMESTAMPTZ NOT NULL,
    service          TEXT        NOT NULL,
    requests         BIGINT      NOT NULL,
    errors           BIGINT      NOT NULL,
    duration_sum_ns  BIGINT      NOT NULL,
    hist             BIGINT[]    NOT NULL,
    PRIMARY KEY (bucket, service)
);

-- 10-second rollup of client calls per caller→callee edge.
CREATE TABLE edge_stats (
    bucket           TIMESTAMPTZ NOT NULL,
    caller           TEXT        NOT NULL,
    callee           TEXT        NOT NULL,
    calls            BIGINT      NOT NULL,
    errors           BIGINT      NOT NULL,
    retries          BIGINT      NOT NULL,
    duration_sum_ns  BIGINT      NOT NULL,
    hist             BIGINT[]    NOT NULL,
    PRIMARY KEY (bucket, caller, callee)
);

CREATE TABLE incidents (
    id                 BIGSERIAL   PRIMARY KEY,
    title              TEXT        NOT NULL,
    severity           TEXT        NOT NULL,
    status             TEXT        NOT NULL,
    primary_service    TEXT        NOT NULL,
    affected_services  TEXT[]      NOT NULL DEFAULT '{}',
    trigger_rule       TEXT        NOT NULL,
    started_at         TIMESTAMPTZ NOT NULL,
    detected_at        TIMESTAMPTZ NOT NULL,
    last_signal_at     TIMESTAMPTZ NOT NULL,
    resolved_at        TIMESTAMPTZ
);
CREATE INDEX incidents_status_idx ON incidents (status, detected_at DESC);
CREATE INDEX incidents_detected_idx ON incidents (detected_at DESC);

CREATE TABLE incident_signals (
    id           BIGSERIAL   PRIMARY KEY,
    incident_id  BIGINT      NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    rule         TEXT        NOT NULL,
    service      TEXT        NOT NULL,
    peer         TEXT        NOT NULL DEFAULT '',
    severity     TEXT        NOT NULL,
    value        DOUBLE PRECISION NOT NULL,
    baseline     DOUBLE PRECISION NOT NULL,
    threshold    DOUBLE PRECISION NOT NULL,
    peak_value   DOUBLE PRECISION NOT NULL,
    unit         TEXT        NOT NULL,
    message      TEXT        NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,
    active       BOOLEAN     NOT NULL DEFAULT TRUE,
    exemplars    TEXT[]      NOT NULL DEFAULT '{}',
    UNIQUE (incident_id, rule, service, peer)
);

CREATE TABLE incident_notes (
    id           BIGSERIAL   PRIMARY KEY,
    incident_id  BIGINT      NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    author       TEXT        NOT NULL,
    body         TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX incident_notes_incident_idx ON incident_notes (incident_id, created_at);

CREATE TABLE experiments (
    id           BIGSERIAL   PRIMARY KEY,
    scenario     TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    params       JSONB       NOT NULL DEFAULT '{}',
    targets      TEXT[]      NOT NULL DEFAULT '{}',
    seed         BIGINT      NOT NULL,
    status       TEXT        NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL,
    planned_end  TIMESTAMPTZ NOT NULL,
    ended_at     TIMESTAMPTZ,
    stop_reason  TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX experiments_started_idx ON experiments (started_at DESC);

-- Immutable capture of the telemetry around an incident.
CREATE TABLE datasets (
    id              BIGSERIAL   PRIMARY KEY,
    incident_id     BIGINT      NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    version         INT         NOT NULL,
    captured_at     TIMESTAMPTZ NOT NULL,
    baseline_start  TIMESTAMPTZ NOT NULL,
    incident_start  TIMESTAMPTZ NOT NULL,
    window_end      TIMESTAMPTZ NOT NULL,
    trace_count     INT         NOT NULL DEFAULT 0,
    span_count      INT         NOT NULL DEFAULT 0,
    checksum        TEXT        NOT NULL DEFAULT '',
    meta            JSONB       NOT NULL DEFAULT '{}',
    service_series  JSONB       NOT NULL DEFAULT '[]',
    edge_series     JSONB       NOT NULL DEFAULT '[]',
    UNIQUE (incident_id, version)
);

CREATE TABLE dataset_spans (
    dataset_id      BIGINT      NOT NULL REFERENCES datasets (id) ON DELETE CASCADE,
    phase           TEXT        NOT NULL,
    trace_id        TEXT        NOT NULL,
    span_id         TEXT        NOT NULL,
    parent_span_id  TEXT        NOT NULL,
    service         TEXT        NOT NULL,
    name            TEXT        NOT NULL,
    kind            SMALLINT    NOT NULL,
    start_time      TIMESTAMPTZ NOT NULL,
    duration_ns     BIGINT      NOT NULL,
    status_code     SMALLINT    NOT NULL,
    status_message  TEXT        NOT NULL,
    attributes      JSONB       NOT NULL,
    peer_service    TEXT        NOT NULL,
    resend_count    INT         NOT NULL,
    error_type      TEXT        NOT NULL,
    PRIMARY KEY (dataset_id, trace_id, span_id)
);

CREATE TABLE hypotheses (
    id                      BIGSERIAL PRIMARY KEY,
    dataset_id              BIGINT    NOT NULL REFERENCES datasets (id) ON DELETE CASCADE,
    incident_id             BIGINT    NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    rank                    INT       NOT NULL,
    kind                    TEXT      NOT NULL,
    service                 TEXT      NOT NULL,
    peer                    TEXT      NOT NULL DEFAULT '',
    title                   TEXT      NOT NULL,
    statement               TEXT      NOT NULL,
    score                   INT       NOT NULL,
    max_score               INT       NOT NULL,
    evidence                JSONB     NOT NULL DEFAULT '[]',
    suggested_intervention  JSONB,
    matches_known_fault     BOOLEAN   NOT NULL DEFAULT FALSE
);
CREATE INDEX hypotheses_dataset_idx ON hypotheses (dataset_id, rank);

CREATE TABLE replay_runs (
    id                BIGSERIAL   PRIMARY KEY,
    incident_id       BIGINT      NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    dataset_id        BIGINT      NOT NULL REFERENCES datasets (id) ON DELETE CASCADE,
    name              TEXT        NOT NULL,
    interventions     JSONB       NOT NULL,
    seed              BIGINT      NOT NULL,
    engine_version    TEXT        NOT NULL,
    dataset_checksum  TEXT        NOT NULL,
    result            JSONB       NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    duration_ms       BIGINT      NOT NULL DEFAULT 0
);
CREATE INDEX replay_runs_incident_idx ON replay_runs (incident_id, created_at DESC);

CREATE TABLE events (
    id             BIGSERIAL   PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL,
    kind           TEXT        NOT NULL,
    severity       TEXT        NOT NULL DEFAULT '',
    title          TEXT        NOT NULL,
    detail         JSONB       NOT NULL DEFAULT '{}',
    incident_id    BIGINT      REFERENCES incidents (id) ON DELETE CASCADE,
    service        TEXT        NOT NULL DEFAULT '',
    trace_id       TEXT        NOT NULL DEFAULT '',
    experiment_id  BIGINT      REFERENCES experiments (id) ON DELETE SET NULL,
    replay_id      BIGINT      REFERENCES replay_runs (id) ON DELETE SET NULL
);
CREATE INDEX events_ts_idx ON events (ts DESC, id DESC);
CREATE INDEX events_incident_idx ON events (incident_id, ts);

CREATE TABLE service_health (
    service    TEXT        PRIMARY KEY,
    status     TEXT        NOT NULL,
    since      TIMESTAMPTZ NOT NULL,
    last_seen  TIMESTAMPTZ NOT NULL
);

CREATE TABLE settings (
    key         TEXT        PRIMARY KEY,
    value       JSONB       NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
