# Reliability test

A single reproducible run, separate from the 27-check acceptance script. These are the numbers measured; nothing is
extrapolated. Raw data: [`docs/reliability/20261114T125214Z/`](reliability/20261114T125214Z/).

## Reproduce

```sh
export TF_API_TOKEN=demo-api-token-123 TF_LAB_TOKEN=demo-lab-token-456
docker compose -f deploy/docker-compose.yml up --build -d
TOKEN=demo-api-token-123 RATE=500 DURATION=360 scripts/reliability.sh   # writes docs/reliability/<timestamp>/
```

## Workload

- `scripts/loadtest` sends **500 spans/s** of OTLP protobuf (50-span requests, ~10 requests/s, one trace per span,
  service `loadtest-<epoch>`) for **360 s**, behaving like an exporter: it retries failed requests up to 8 times with
  exponential back-off (100 ms to 5 s). Every span is unique, so stored count can be reconciled exactly.
- At the same time the bundled sample shop kept running (about 30 spans/s) and the load tester made one **API read per
  second** (`overview`, `services`, `incidents`, `dependencies`, rotating) with the API token.
- Chaos schedule (seconds from start): `+90 s` `docker restart` traceforge; `+162 s` `docker restart` postgres;
  `+235 s` start the lab scenario *Service errors: payment* (40 % errors for 60 s) while load continued.
- Environment: macOS arm64 host, Colima VM with 4 CPUs and 5.8 GiB RAM, Docker 29.5.2, load generator and stack on the
  same machine (loopback), commit `d2d8514`. CPU and memory are `docker stats` samples every ~4 s (62 samples).

## Results

| Measure | Result |
|---|---|
| Sustained ingestion | 180,000 spans in 360 s = **500 spans/s** achieved |
| Ingest request latency (client-side) | 3,603 requests: p50 1.7 ms, p95 2.5 ms, p99 3.0 ms, max 4.5 ms |
| Ingest request errors | 3 of 3,603 (0.08 %), all `connection refused` during the traceforge restart; each succeeded on retry |
| Throttling / drops | 0 HTTP 429, 0 spans dropped, 0 spans rejected |
| API read latency (358 reads under load) | p50 5.8 ms, p95 9.1 ms, p99 9.8 ms, max 11.2 ms |
| API read errors | 1 of 358 (0.28 %): an HTTP 503 around the postgres restart; the other 357 were 200 |
| **Delivery accounting** | attempted 180,000 = acknowledged 180,000 = **stored in Postgres 180,000** (0 lost, 0 duplicated) |
| traceforge process | CPU mean 3.1 %, max 6.1 % of one core; memory 8-12 MiB |
| postgres | CPU mean 6.4 %, max 48.6 % (during recovery/restart); memory 39-172 MiB |
| Ingest queue | maximum observed depth 150 spans (capacity 50,000) |
| Database connections | 4-6 |
| Database size at end | 271 MB (this run plus earlier runs on the same volume) |

## Behaviour during failures

- **traceforge restart** (graceful `docker restart`): the exporter saw 3 refused connections and delivered the same
  batches on retry. Pipeline counters reset on restart, which is why `spans_received` after it (152,249) is smaller
  than 180,000.
- **postgres restart**: the pipeline logged 2 write errors (`terminating connection due to administrator command`),
  retried with back-off, and wrote the batches; nothing was dropped. One API read returned 503 while the database was down.
- **injected fault**: the payment-errors scenario produced a `checkout → payment` dependency incident, error-rate
  signals for payment and checkout, and a retry anomaly; the signals went inactive after the fault cleared and all
  sample services returned to `healthy`. At the moment the summary was captured (about 90 s after the fault ended) the incident
  was still `open`, which is inside the 60 s quiet period plus the 60 s detection window. A later check is recorded in
  [PROJECT_STATUS.md](../PROJECT_STATUS.md).
- Incident bookkeeping: incident 3 is the injected fault. Its recorded start (12:55:20) is earlier than the injection
  (12:56:09) while its signals first fired at 12:56:44, so the start is the investigation's onset estimate, which I did not
  analyse further. Incident 2 (start 12:50:40) predates this run: it came from rebuilding the traceforge container shortly
  before; it had resolved by 12:55:04.

## What this does not show

One run on one laptop-class VM; no repeat to measure variance. Restarts were graceful (`docker restart`), not `kill -9`,
power loss or a full disk. Postgres outages longer than the pipeline's retry budget (5 retries, about 6 s of back-off per
batch) **will drop spans**; they are counted in `spans_dropped` but not recovered. 500 spans/s is not a capacity
limit: CPU headroom was large and the limit was not searched for. Duration (6 minutes) says nothing about memory
growth over days, retention behaviour or table bloat. Client-side latency excludes network. The no-loss result depends on
the exporter retrying, as OpenTelemetry exporters normally do.
