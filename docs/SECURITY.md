# Security

**TraceForge is a local demonstration and is not production-ready.** It has not been penetration-tested, has no user
accounts or roles, and TLS is not terminated by the server.

## Demonstration credentials

The tokens used in the docs, tests and screenshots are public examples:

| Purpose | Value | Variable |
|---|---|---|
| API | `demo-api-token-123` | `TF_API_TOKEN` |
| Lab (server to sample services) | `demo-lab-token-456` | `TF_LAB_TOKEN` |

**They are demonstration credentials only.** Anyone who has read this repository knows them. Generate your own
(`openssl rand -hex 24`) for anything beyond a throwaway local run. `docker-compose.yml` reads them from the
environment with empty defaults (token-less, open), so no token is committed. `.env` is git-ignored.

## What is protected

- **API**: when `TF_API_TOKEN` is set every `/api/*` route (including unknown ones) requires `Authorization: Bearer <token>`.
  Comparison is constant-time and the `Bearer` scheme is mandatory. `/healthz` and `/readyz` are public for probes.
  With no token set, the API and lab are open to anyone who can reach the port; set it.
- **Writes from browsers**: state-changing requests carrying a foreign `Origin` are rejected with 403 (CSRF defence
  that also protects token-less demos).
- **OTLP ingest** (`:4318`): optional separate `TF_INGEST_TOKEN`; bodies are size-limited before and after gzip
  decompression; field counts and lengths are capped; timestamps and IDs are validated.
- **Failure lab**: the API cannot name arbitrary hosts; it can only fault services in the `TF_LAB_TARGETS` allow-list.
  The sample services' `/admin/fault` endpoints require `TF_LAB_TOKEN` when it is set.
- **UI**: the token is kept in `sessionStorage` (cleared when the tab closes) and sent as a bearer header; strict CSP
  (`default-src 'self'`, no inline script), `X-Frame-Options: DENY`, `nosniff`, `no-referrer`. HTML reports escape all
  telemetry-derived text.
- **SQL**: built from fixed fragments with bound parameters.

## Isolation of the local demo

Compose publishes only `8080` (UI/API) and `4318` (OTLP). Postgres and the sample services are reachable only on the
compose network. Postgres uses the throwaway password `traceforge`. Bind to localhost or firewall the ports on shared machines.

## Known gaps

No TLS, rate limiting, audit log, token rotation, per-user authorisation or multi-tenancy. The lab follows HTTP
redirects from its allow-listed targets. Retention is by time only. See [LIMITATIONS.md](LIMITATIONS.md).
