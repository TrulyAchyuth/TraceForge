#!/usr/bin/env bash
# End-to-end acceptance run against a live stack (docker compose up).
# Usage: BASE=http://localhost:8080 TOKEN=... scripts/e2e.sh
# Needs curl and jq. Takes about six minutes: it injects a real fault and waits for recovery.
set -uo pipefail
BASE=${BASE:-http://localhost:8080}; TOKEN=${TOKEN:-}
H=(-H 'content-type: application/json'); [ -n "$TOKEN" ] && H+=(-H "Authorization: Bearer $TOKEN")
api() { curl -s "${H[@]}" "$@"; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
pass=0; fail=0
check() { if [ "$2" = "1" ] || [ "$2" = "true" ]; then echo "PASS  $1"; pass=$((pass+1)); else echo "FAIL  $1"; fail=$((fail+1)); fi; }
ok() { [ "$1" = "$2" ] && echo 1 || echo 0; }

echo "== security"
check "API without token is rejected (only when a token is set)" "$( [ -z "$TOKEN" ] && echo 1 || ok "$(code "$BASE/api/v1/overview")" 401 )"
check "wrong token is rejected" "$( [ -z "$TOKEN" ] && echo 1 || ok "$(code -H 'Authorization: Bearer wrong' "$BASE/api/v1/overview")" 401 )"
check "lab start without token is rejected" "$( [ -z "$TOKEN" ] && echo 1 || ok "$(code -XPOST -H 'content-type: application/json' -d '{"scenario":"payment-errors","duration_seconds":30}' "$BASE/api/v1/lab/experiments")" 401 )"
check "cross-origin write is rejected" "$(ok "$(code -XPOST "${H[@]}" -H 'Origin: http://evil.example' "$BASE/api/v1/lab/reset")" 403)"
check "healthz is public" "$(ok "$(code "$BASE/healthz")" 200)"

echo "== telemetry"
for i in $(seq 1 30); do n=$(api "$BASE/api/v1/overview" | jq '.ingest.spans_written // 0'); [ "${n:-0}" -gt 200 ] && break; sleep 2; done
check "spans are being ingested ($n written)" "$([ "${n:-0}" -gt 200 ] && echo 1 || echo 0)"
check "dependency graph has edges" "$([ "$(api "$BASE/api/v1/dependencies" | jq '.edges|length')" -ge 3 ] && echo 1 || echo 0)"
tid=$(api "$BASE/api/v1/traces?limit=1" | jq -r '.traces[0].trace_id')
check "a trace can be fetched" "$([ "$(api "$BASE/api/v1/traces/$tid" | jq '.spans|length')" -ge 2 ] && echo 1 || echo 0)"

echo "== failure injection"
api -XPOST "$BASE/api/v1/memory/seed" >/dev/null
exp=$(api -XPOST -d '{"scenario":"payment-latency","duration_seconds":90,"seed":1}' "$BASE/api/v1/lab/experiments" | jq -r '.experiment.id')
check "experiment started (#$exp)" "$([ "$exp" != null ] && [ -n "$exp" ] && echo 1 || echo 0)"
inc=""
for i in $(seq 1 40); do inc=$(api "$BASE/api/v1/incidents" | jq -r '[.incidents[]|select(.status!="resolved")][0].id // empty'); [ -n "$inc" ] && break; sleep 5; done
check "incident detected (#$inc)" "$([ -n "$inc" ] && echo 1 || echo 0)"
[ -z "$inc" ] && { echo "cannot continue"; exit 1; }

echo "== investigation"
for i in $(seq 1 30); do top=$(api "$BASE/api/v1/incidents/$inc/investigation" | jq -r '.hypotheses[0].service // empty'); [ -n "$top" ] && break; sleep 5; done
check "top hypothesis names payment ($top)" "$(ok "$top" payment)"
check "dataset integrity holds" "$(ok "$(api "$BASE/api/v1/incidents/$inc/investigation" | jq -r .dataset_integrity_ok)" true)"
sim=$(api "$BASE/api/v1/incidents/$inc/similar")
check "incident memory returns simulated matches, labelled" "$([ "$(echo "$sim" | jq '[.matches[]|select(.record.simulated)]|length')" -ge 1 ] && echo 1 || echo 0)"
check "matches explain shared features" "$([ "$(echo "$sim" | jq '[.matches[]|select((.shared_features|length)>0)]|length')" = "$(echo "$sim" | jq '.matches|length')" ] && echo 1 || echo 0)"

echo "== counterfactual replay"
body='{"name":"Restore payment latency","seed":7,"latency_objective_ms":0,"interventions":[{"type":"scale_latency","service":"payment","mode":"baseline"}]}'
r1=$(api -XPOST -d "$body" "$BASE/api/v1/incidents/$inc/replays"); r2=$(api -XPOST -d "$body" "$BASE/api/v1/incidents/$inc/replays")
s1=$(echo "$r1" | jq -c '.replay.result|{summary,operations,services,edges,findings}'); s2=$(echo "$r2" | jq -c '.replay.result|{summary,operations,services,edges,findings}')
check "replay ran and reports simulated evidence" "$(ok "$(echo "$r1" | jq -r .replay.result.evidence_class)" simulated)"
check "identical inputs and seed give identical results" "$([ -n "$s1" ] && [ "$s1" = "$s2" ] && echo 1 || echo 0)"
check "replay reduced simulated error rate or p95" "$(echo "$r1" | jq '(.replay.result.summary.counterfactual.error_rate < .replay.result.summary.original.error_rate) or (.replay.result.summary.counterfactual.p95_ms < .replay.result.summary.original.p95_ms)')"
check "invalid intervention is rejected with 400" "$(ok "$(code -XPOST "${H[@]}" -d '{"name":"x","seed":1,"interventions":[{"type":"nope"}]}' "$BASE/api/v1/incidents/$inc/replays")" 400)"
check "dataset still intact after replays" "$(ok "$(api "$BASE/api/v1/incidents/$inc/investigation" | jq -r .dataset_integrity_ok)" true)"

echo "== reports"
for f in md html json; do
  out=$(mktemp); c=$(curl -s -o "$out" -w '%{http_code}' "${H[@]}" "$BASE/api/v1/incidents/$inc/report?format=$f"); sz=$(wc -c <"$out" | tr -d ' '); rm -f "$out"
  check "report $f downloads ($c, $sz bytes)" "$([ "$c" = 200 ] && [ "$sz" -gt 1000 ] && echo 1 || echo 0)"
done
check "report mentions incident memory" "$(curl -s "${H[@]}" "$BASE/api/v1/incidents/$inc/report?format=md" | grep -q 'Similar past incidents' && echo 1 || echo 0)"

echo "== recovery"
for i in $(seq 1 50); do st=$(api "$BASE/api/v1/incidents/$inc" | jq -r .incident.status); [ "$st" = resolved ] && break; sleep 6; done
check "incident resolved after the fault ended ($st)" "$(ok "$st" resolved)"
check "all services healthy" "$(ok "$(api "$BASE/api/v1/services" | jq '[.services[]|select(.status!="healthy")]|length')" 0)"
for i in $(seq 1 20); do mem=$(api "$BASE/api/v1/memory/records" | jq "[.records[]|select(.incident_id==$inc)]|length"); [ "$mem" = 1 ] && break; sleep 5; done
check "resolved incident was saved to incident memory" "$(ok "$mem" 1)"
check "memory graph has nodes and edges" "$(api "$BASE/api/v1/memory/graph" | jq '(.graph.nodes|length)>0 and (.graph.edges|length)>0')"

echo; echo "passed $pass, failed $fail"; [ "$fail" = 0 ]
