# Demo

Everything below runs locally. The tokens are public demonstration values (see [SECURITY.md](SECURITY.md)).

```sh
export TF_API_TOKEN=demo-api-token-123 TF_LAB_TOKEN=demo-lab-token-456
docker compose -f deploy/docker-compose.yml up --build -d     # first build takes a few minutes
open http://localhost:8080                                    # enter the API token when asked
```

1. **Overview**: within ~30 s the sample shop's traffic appears (about 30 spans/s).
2. **Failure lab → Dependency slowdown: payment → Start** (90 s). Optionally first **Memory graph → Load demo records**.
3. After 30-60 s an incident appears under **Incidents**. Open it: **Investigation** ranks *payment: its own processing
   became slower* first, with observed / correlation / inference evidence and an integrity-verified dataset.
4. **Similar incidents** shows overlapping past incidents, shared features and what was tried (demo records are badged *simulated example*).
5. **Replay**: the service defaults to the top-ranked origin; run *Scale latency → Restore to baseline*. The result is
   labelled simulated: error rate and p95 before/after, a time series, assumptions and unsupported conclusions.
6. **Reports**: download Markdown / HTML / JSON. **Memory graph**: after the incident resolves it becomes a node.

Automated versions: `scripts/e2e.sh` (API acceptance, ~6 min) and `cd web && npm run e2e` (browser tests).
Stop with `docker compose -f deploy/docker-compose.yml down -v`.

Colima / Docker Desktop credential helper problems: use a `DOCKER_CONFIG` directory containing `{}`
and set `DOCKER_HOST` to the engine socket (for Colima `unix://$HOME/.colima/default/docker.sock`).
