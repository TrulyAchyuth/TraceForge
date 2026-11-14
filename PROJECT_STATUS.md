# TraceForge status

Last updated: 2026-11-14 (after local review)

## Done and verified
- Go backend (`cmd/`, `internal/`): OTLP ingest, Postgres store, detection, investigation, replay,
  reports, failure lab, REST API. `go build`, `go vet`, `go test ./...` pass.
- End-to-end checked against real Postgres: sample app -> ingest -> payment-latency experiment ->
  incident -> investigation (payment ranked first) -> replay -> report -> status PATCH -> lab reset.
- Web UI (`web/dist`, vanilla JS): overview, services, dependencies, incidents + detail, traces, lab.
  Loaded in a browser under the CSP with no console errors.
- Docker image builds; `deploy/docker-compose.yml` brings up the full stack and the lab works in it.

## Review (local, manual; `/ultrareview` is user-triggered and was not run)
- Fixed: state-changing API calls from a foreign Origin were accepted (CSRF risk when no `TF_API_TOKEN`,
  including lab fault injection). Now rejected with 403; unit-tested.
- Fixed: UI report links returned 401 when `TF_API_TOKEN` was set; they are now fetched with the token.
- Compose run verified end to end: payment-latency experiment -> incident INC-0001 -> investigation ranks
  payment first (9/10) -> fault auto-clears -> incident auto-resolves, all services healthy.
- Reviewed, no change needed: SQL built from fixed fragments with bound args, static path handling,
  constant-time token compare, lab allowlist, html/template for HTML reports.

## Not done / limitations
- Investigation's second hypothesis ("gateway request volume surged") was a plausible-looking false lead in
  the verified run; ranking is a heuristic aid.
- Lab targets follow HTTP redirects; the sample `/admin/fault` endpoints are unauthenticated (demo network only).
- Without `TF_API_TOKEN` the API and lab are open to anyone who can reach port 8080.
- Only config and API-auth unit tests were added; most packages have no tests (replay has some).
- UI is minimal; only checked on Chrome-class browser via Playwright, no UI tests.
- Replay UI offers one intervention type (restore latency to baseline).
