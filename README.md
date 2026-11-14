# TraceForge

TraceForge ingests OpenTelemetry traces (OTLP/HTTP), detects service degradation, builds an
evidence-ranked investigation for each incident, and lets you replay captured traces with a
change applied ("what if payment latency were at baseline?") to estimate the effect. A bundled
sample shop and failure lab let you reproduce incidents on demand.

## Quick start

```sh
docker compose -f deploy/docker-compose.yml up --build
```

Open <http://localhost:8080>, go to **Lab**, run "Dependency slowdown: payment", wait about a
minute, then open the incident under **Incidents**. Point your own OTLP/HTTP exporter at
`http://localhost:4318/v1/traces`.

## Configuration (environment)

| Variable | Default | Purpose |
|---|---|---|
| `TF_DATABASE_URL` | required | PostgreSQL URL |
| `TF_HTTP_ADDR` / `TF_OTLP_ADDR` | `:8080` / `:4318` | API+UI and OTLP listeners |
| `TF_API_TOKEN` | unset | Bearer token required on `/api` (set it outside a demo) |
| `TF_INGEST_TOKEN` | unset | Bearer token required on OTLP ingest |
| `TF_RETENTION_HOURS` | `24` | Telemetry retention |
| `TF_DETECT_INTERVAL_SECONDS` | `10` | Detection cadence |
| `TF_LAB_TARGETS` | unset | `name=url,...` allowlist of sample services the lab may fault |
| `TF_WEB_DIR` | `web/dist` | Static UI directory |

## Development

```sh
go test ./...
go run ./cmd/traceforge     # needs TF_DATABASE_URL
```

The UI is plain HTML/JS in `web/dist` (no build step). The lab fault endpoints on the sample
services are unauthenticated: run the demo only on an isolated network.

Limitations: replay is a model-based estimate over sampled traces, not a prediction; evidence
scores rank hypotheses, they are not probabilities; metrics and logs are not ingested.
