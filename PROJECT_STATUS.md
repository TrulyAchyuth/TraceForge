# TraceForge status

Last updated: 2026-11-14

## Done and verified
- Go backend (`cmd/`, `internal/`): OTLP ingest, Postgres store, detection, investigation, replay,
  reports, failure lab, REST API. `go build`, `go vet`, `go test ./...` pass.
- End-to-end checked against real Postgres: sample app -> ingest -> payment-latency experiment ->
  incident -> investigation (payment ranked first) -> replay -> report -> status PATCH -> lab reset.
- Web UI (`web/dist`, vanilla JS): overview, services, dependencies, incidents + detail, traces, lab.
  Loaded in a browser under the CSP with no console errors.
- Docker image builds; `deploy/docker-compose.yml` brings up the full stack and the lab works in it.

## Not done / limitations
- Only config and API-auth unit tests were added; most packages have no tests (replay has some).
- UI is minimal; only checked on Chrome-class browser via Playwright, no UI tests.
- Replay UI offers one intervention type (restore latency to baseline).
