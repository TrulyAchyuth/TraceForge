# Testing

| Layer | Command | What it covers |
|---|---|---|
| Go unit/integration | `go vet ./... && go test ./...` | 12 packages with tests; see below |
| Go + Postgres | `TF_TEST_DATABASE_URL=postgres://postgres:t@localhost:55432/t?sslmode=disable go test ./internal/memory` | memory snapshot/similarity/seeding against a real database (skipped without the variable). Start one with `docker run --rm -d -e POSTGRES_PASSWORD=t -e POSTGRES_DB=t -p 55432:5432 postgres:16-alpine` |
| Frontend unit | `cd web && npm test` | formatting, trace tree, dependency layout, memory-graph layout/filter (`src/lib.test.ts`) |
| Typecheck + build | `cd web && npm run build` | `tsc --noEmit`, then Vite production build into `web/dist` |
| Browser (Playwright, Chromium) | `cd web && npx playwright install chromium && npm run e2e` | 12 tests against a live stack, see below |
| API acceptance | `TOKEN=... scripts/e2e.sh` | 27 checks on a fresh token-protected stack, about 6 minutes |
| Reliability | `TOKEN=... scripts/reliability.sh` | see [RELIABILITY.md](RELIABILITY.md) |

Browser and acceptance tests need a stack: `TF_API_TOKEN=demo-api-token-123 TF_LAB_TOKEN=demo-lab-token-456 docker compose -f deploy/docker-compose.yml up --build -d`.
The Playwright suite opens `#/incidents/1`, so run it on a fresh stack after `scripts/e2e.sh` (which creates incident 1) or after your first lab run.

## What the Go tests assert (behaviour, not coverage)

- **detect**: each rule fires and stays quiet on the right side of its thresholds; the absolute-delta floor vetoes a
  large ratio on a tiny latency; the MAD test vetoes a noisy baseline; unavailability subsumes degradation unless the callee
  still serves traffic; severity ordering and determinism; config validation bounds.
- **ingest**: auth (missing, wrong, no-scheme token), content type/encoding, garbage bodies, gzip bomb and oversize
  bodies (413), partial success counts, 429 with `Retry-After` and no queue-depth leak, batch size limits, drain on
  shutdown, retry after a transient write failure, drop-and-count after a permanent one.
- **otlp**: protobuf and OTLP/JSON (hex IDs) decode to the same spans; each rejection reason; bad spans do not block good
  ones; limits truncate oversized names and attributes.
- **investigate**: payment ranked first on the slowdown fixture; ranks sorted, scores bounded; every piece of evidence
  has a class and text; **injected-fault records never change ranking**; determinism; empty and baseline-only data do not
  invent a cause.
- **replay** (existing): deterministic results, interventions, validation errors, identity on no-op.
- **memory** (existing + Postgres): fingerprint is order-independent, weighted overlap, ranking and exclusion of weak
  matches, sparse history, simulated labelling, graph has no dangling edges.
- **report**: Markdown/HTML/JSON build; HTML escapes `<script>` in titles; JSON is stable; a corrupt stored replay is an error;
  counterfactuals are labelled simulated.
- **api**: token required on every state-changing and admin route and on unknown API paths; bearer scheme mandatory;
  cross-origin writes rejected; static handler does not leak files via `..`; CSP and other security headers present.
- **tree**: orphans, two roots, cycles and self-parents are marked incomplete; input order does not matter; retry groups.

Not covered by Go tests: the `store` SQL layer beyond the memory integration test, the detection `Engine` loop (exercised
by the acceptance run), `lab` HTTP calls to live targets, and `cmd/*` wiring.

## Browser tests (`web/e2e/ui.spec.ts`)

Token gate (prompt, wrong token, right token, sign out); every sidebar page loads, and the navigation, incident and replay workflows run with **zero console errors, page errors and failed
network requests** (the replay test tolerates only its deliberate HTTP 400); services filter and detail; incident detail tabs, note creation, hypotheses (payment
first), similar incidents; report downloads (md/html/json) with the token; replay run (defaults to the top-ranked
origin, labelled simulated, server-side validation error shown); memory graph (load demo data, node select/highlight,
type filter, hide simulated, compare records, same-record warning); lab targets; timeline filter; settings validation
and restore defaults; empty, error (mocked 500) and loading states; 390 px mobile layout with no horizontal overflow.

Not covered: starting a lab experiment from the browser (done by `scripts/e2e.sh` through the API), Firefox/WebKit,
keyboard-only walkthroughs, screen readers.
