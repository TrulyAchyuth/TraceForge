# TraceForge status

Last updated: 2026-11-14 (UI rebuilt in React; acceptance run in progress)

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

## Resumed work (after interruption)
- Incident Memory (records, fingerprint, similarity, graph, API, report section) committed earlier.
- Web UI rebuilt in React + TypeScript (`web/src`, built to `web/dist` by `npm run build`): the seven pages that were
  missing (incidents, replay, memory graph, lab, timeline, reports, settings) are now written; `tsc` and vitest pass.
  Token gate keeps the API token in sessionStorage only.
- `scripts/e2e.sh` is the acceptance script (security checks, injection, investigation, memory, replay determinism, reports, recovery).
  Run against a token-protected compose stack: `TF_API_TOKEN=... TF_LAB_TOKEN=... docker-compose -f deploy/docker-compose.yml up --build -d`
  then `TOKEN=... scripts/e2e.sh`. On this machine docker needs `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock` and a
  credential-helper-free `DOCKER_CONFIG`.
