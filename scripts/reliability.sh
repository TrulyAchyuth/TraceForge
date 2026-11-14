#!/usr/bin/env bash
# Reliability run, separate from scripts/e2e.sh. Needs a running compose stack, docker, curl, jq, go.
#   TOKEN=<api token> [RATE=500] [DURATION=360] scripts/reliability.sh
# Timeline (seconds from start, scaled by DURATION=360): steady ingest, restart traceforge at 25%, restart postgres at 45%,
# inject a payment fault at 65%, steady until the end. Writes docs/reliability/<stamp>/.
set -uo pipefail
cd "$(dirname "$0")/.."
BASE=${BASE:-http://localhost:8080}; TOKEN=${TOKEN:-}; RATE=${RATE:-500}; D=${DURATION:-360}
PROJECT=${PROJECT:-deploy}
out=docs/reliability/$(date -u +%Y%m%dT%H%M%SZ); mkdir -p "$out"
svc="loadtest-$(date +%s)"
H=(-H 'content-type: application/json'); [ -n "$TOKEN" ] && H+=(-H "Authorization: Bearer $TOKEN")
api() { curl -s "${H[@]}" "$@"; }
psqlc() { docker exec "${PROJECT}-postgres-1" psql -U postgres -d traceforge -Atc "$1"; }
log() { echo "$(date -u +%H:%M:%S) $*" | tee -a "$out/events.log"; }

{ echo "date_utc=$(date -u +%FT%TZ)"; echo "host=$(uname -srm)"; echo "docker=$(docker version --format '{{.Server.Version}}')"
  echo "rate_spans_per_s=$RATE duration_s=$D service=$svc"; echo "git=$(git rev-parse --short HEAD)"; docker info --format 'cpus={{.NCPU}} mem_bytes={{.MemTotal}}'; } > "$out/environment.txt"

before=$(psqlc "SELECT count(*) FROM spans WHERE service='$svc'")
log "start: baseline spans for $svc = $before"

echo "t_s,container,cpu_pct,mem_mib" > "$out/resources.csv"
echo "t_s,queue_depth,spans_written,spans_dropped,write_errors,throttled,db_conns" > "$out/ingest.csv"
t0=$(date +%s)
( while [ $(( $(date +%s) - t0 )) -lt $((D + 20)) ]; do
    t=$(( $(date +%s) - t0 ))
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' "${PROJECT}-traceforge-1" "${PROJECT}-postgres-1" 2>/dev/null |
      awk -F, -v t="$t" '{gsub("%","",$2); split($3,m," / "); v=m[1]; u=v; gsub(/[0-9.]/,"",u); gsub(/[A-Za-z]/,"",v); if(u=="GiB")v*=1024; if(u=="KiB")v/=1024; print t","$1","$2","v}' >> "$out/resources.csv"
    s=$(api "$BASE/api/v1/ingest/stats" | jq -r '.pipeline|[.queue_depth,.spans_written,.spans_dropped,.write_errors,.requests_throttled]|@csv' 2>/dev/null)
    c=$(psqlc "SELECT count(*) FROM pg_stat_activity WHERE datname='traceforge'" 2>/dev/null)
    echo "$t,${s:-,,,,},${c:-}" >> "$out/ingest.csv"
    sleep 4
  done ) &
sampler=$!

( sleep $((D*25/100)); log "RESTART traceforge container"; docker restart "${PROJECT}-traceforge-1" >/dev/null; log "traceforge restarted"
  sleep $((D*20/100)); log "RESTART postgres container"; docker restart "${PROJECT}-postgres-1" >/dev/null; log "postgres restarted"
  sleep $((D*20/100)); log "INJECT payment-errors fault (60s)"
  curl -s -XPOST "${H[@]}" -d '{"scenario":"payment-errors","duration_seconds":60,"seed":3}' "$BASE/api/v1/lab/experiments" | tee -a "$out/events.log" | head -c 200; echo ) &
chaos=$!

log "loadtest running for ${D}s"
go run ./scripts/loadtest -rate "$RATE" -duration "${D}s" -service "$svc" -token "$TOKEN" -api "$BASE" > "$out/loadtest.json" 2> "$out/loadtest.err"
wait $chaos; log "load finished; waiting 20s for queues to drain"; sleep 20; kill $sampler 2>/dev/null; wait $sampler 2>/dev/null

after=$(psqlc "SELECT count(*) FROM spans WHERE service='$svc'")
acked=$(jq .spans_acked "$out/loadtest.json"); att=$(jq .spans_attempted "$out/loadtest.json")
stored=$((after - before))
final_ing=$(api "$BASE/api/v1/ingest/stats" | jq -c .pipeline)
{ echo "== reconciliation"; echo "spans attempted by exporter : $att"; echo "spans acknowledged (HTTP 200): $acked"; echo "spans stored in postgres    : $stored"
  echo "acknowledged but not stored : $((acked - stored))"; echo "never acknowledged          : $((att - acked))"
  echo "== pipeline counters since last traceforge restart"; echo "$final_ing"
  echo "== incidents"; api "$BASE/api/v1/incidents" | jq -c '[.incidents[]|{id,title,status,started_at}]'
  echo "== services"; api "$BASE/api/v1/services" | jq -c '[.services[]|{name,status}]'
  echo "== db"; psqlc "SELECT pg_size_pretty(pg_database_size('traceforge'))"; } | tee "$out/summary.txt"
cat "$out/loadtest.json" | jq . | tee -a "$out/summary.txt" >/dev/null
echo "results in $out"
