# TraceForge status

Last updated: 2026-11-14. Local demonstration; **not production-ready** (see docs/SECURITY.md, docs/LIMITATIONS.md).

## Implemented
Go backend (OTLP ingest, Postgres store, detection, investigation, replay, reports, failure lab, Incident Memory + graph,
REST API with optional bearer token) and a React + TypeScript UI (overview, services, dependencies, traces, incidents with
investigation/similar/replays/notes, counterfactual replay, memory graph, timeline, reports, lab, settings, token gate).

## Verification (2026-11-14, fresh token-protected compose stack, demo tokens)
| Check | Result |
|---|---|
| `go vet ./...` | clean |
| `go test ./...` | 12 packages with tests, all pass: 73 test functions/subtests passed, 1 skipped (Postgres-backed memory test needs `TF_TEST_DATABASE_URL`; run separately against a throwaway Postgres: passed) |
| Frontend `npm test` / `npm run build` | 10 vitest tests pass; `tsc --noEmit` + Vite build succeed |
| Playwright (`npm run e2e`, Chromium) | 12 of 12 pass, repeated 3 times for stability earlier, and once more on the final fresh stack |
| `scripts/e2e.sh` acceptance | 27 of 27 pass (fresh stack, final image) |
| Docker image build + compose startup | OK (needs `DOCKER_HOST`/`DOCKER_CONFIG` workaround on this Colima machine, see docs/DEMO.md) |
| Reliability run (`scripts/reliability.sh`, 360 s, 500 spans/s, restarts of traceforge and postgres, injected fault) | 180,000 sent = 180,000 acknowledged = 180,000 stored; ingest p95 2.5 ms; API read p95 9.1 ms; 3 refused connections during restart (retried), 1 API 503 during the postgres restart; see docs/RELIABILITY.md |

## Bugs found and fixed in this pass
- Replay page showed nothing for existing runs (list endpoint omits results): each run now loads its own result.
- Replay page opened the oldest run, and showed `NaN` diagnostics (wrong field names).
- Replay service defaulted to the alphabetically first service; now the top-ranked origin.
- Horizontal overflow on phone width (overview, dependencies); memory graph clipped its right column.
- `Authorization: <token>` without the `Bearer` scheme was accepted by the API and by the sample-app admin endpoint.

## Not done / known limitations
- Browser tests cover Chromium only; starting a lab experiment from the browser is only covered through the API.
- No Go tests for the SQL store beyond the memory integration test, the detection engine loop, lab HTTP, or `cmd/*`.
- Reliability: one run, one machine, graceful restarts only; no kill -9, disk-full or multi-day soak; outages longer than the
  pipeline retry budget drop (and count) spans. After a traceforge image rebuild, incidents can open from the sample
  traffic gap; the incident whose start preceded the fault injection in the reliability run was not analysed further.
- With the 500 spans/s load service present, the investigation of the concurrent incident reported "No origin isolated"
  (the extra service dilutes the dataset); the checker is honest here but this was not investigated further.
- Hypothesis ranking is a heuristic (a false lead ranks second in the payment-latency run); replay is a model.
- No TLS, rate limiting or per-user auth. Demo tokens are public values.
