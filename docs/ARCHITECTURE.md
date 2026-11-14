# Architecture

```
 sample services ──OTLP/HTTP──▶ :4318  ingest pipeline ──batches──▶ PostgreSQL
 (or your exporter)                    (validate, bound,             spans, traces, rollups,
                                        backpressure)                incidents, datasets, replays, memory
                                                                      ▲
 detection engine (every 10 s) ── rollups ─▶ signals ─▶ incidents ────┤
 investigation (captures a dataset per incident, ranks hypotheses) ───┤
 replay engine (pure function of a stored dataset + scenario) ────────┤
 incident memory (fingerprints, similarity, graph) ───────────────────┘
                                       REST API + React UI on :8080
```

| Package | Responsibility |
|---|---|
| `internal/otlp` | Decode OTLP protobuf/JSON, validate IDs and timestamps, apply size limits, count rejections by reason |
| `internal/ingest` | `POST /v1/traces`: auth, gzip bomb protection, bounded queue (HTTP 429 when full), batched writes with retry |
| `internal/store` | PostgreSQL access, migrations, 10-second service/edge rollups, dataset capture and checksums |
| `internal/detect` | Six rules (error rate, latency outlier, traffic change, dependency degradation, unavailability, retry anomaly) as a pure function of baseline and window; the engine turns signals into incidents and auto-resolves |
| `internal/investigate` | Captures an immutable dataset (baseline + incident traces), computes profiles, ranks hypotheses with evidence classed as observed / correlation / inference |
| `internal/replay` | Deterministic counterfactual engine over a dataset: scale latency, remove errors, retry policy, timeout |
| `internal/memory` | Incident Memory: fingerprint, weighted overlap similarity, graph (see [MEMORY_GRAPH.md](MEMORY_GRAPH.md)) |
| `internal/report` | Markdown, HTML (escaped) and JSON incident reports |
| `internal/lab` | Fault injection into the bundled sample services through an allow-list |
| `internal/api` | REST handlers, token auth, CSRF origin check, security headers, static UI |
| `web/` | React + TypeScript SPA (Vite), hash-routed, built to `web/dist` and served by the Go binary |
| `cmd/sampleapp` | Six-role sample shop (gateway, checkout, payment, inventory, notification, loadgen) emitting real OTLP |

Design rules that matter:

- **Evidence is separated by class.** Every statement is `observed` (measured in telemetry), `correlation`, `inference`,
  or `simulated` (replay output). Simulated results are labelled everywhere they appear.
- **Datasets are immutable.** An investigation captures spans once and stores a checksum; replays and reports refuse a
  dataset whose checksum no longer matches.
- **Replay is deterministic.** The same dataset, scenario and seed produce byte-identical results.
- **Ranking is a heuristic.** Hypothesis scores are an additive rubric out of 10, not probabilities. Injected-fault
  records are displayed beside hypotheses but never used to score them.
