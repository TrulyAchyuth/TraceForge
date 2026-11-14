# Incident Memory and the memory graph

Every incident that reaches `resolved` or `closed` is saved as a **record**: its signals, event sequence, dependency
edges, ranked hypotheses, injected faults, operator notes, recovery, and the interventions that were tried (counterfactual
replays, labelled *simulated*; operator notes, labelled *operator*).

## Fingerprint and similarity (method `mem-1`)

A record is reduced to a set of features, each in one of five classes with a fixed weight (not fitted to data):

| Class | Example | Weight |
|---|---|---|
| edge | `edge:checkout->payment` | 3 |
| origin | `origin:latency_origin@payment` (top hypothesis; omitted when undetermined) | 3 |
| rule | `rule:dependency_degradation` | 2 |
| primary | `primary:checkout` | 2 |
| service | `service:gateway` | 1 |

Similarity is the **weighted Jaccard overlap** of two feature sets. A candidate is only listed when it shares at least
one core feature (edge, origin or rule), at most 8 matches are returned, ranked by overlap. Each match lists shared
features, features only in the current incident, features only in the past one, and plain-language differences.
Overlap is not a probability and a match is not a diagnosis; "what was tried before" was tried on *that* incident's data.

## Simulated demonstration records

`POST /api/v1/memory/seed` (button **Load demo records**) inserts four hand-written records (`SIM-0001`..`SIM-0004`) so
the feature can be seen on a fresh install. They carry `simulated: true`, are drawn with dashed borders and a
"simulated example" badge, are counted separately (`simulated_count`), and are removed by `DELETE /api/v1/memory/seed`.
They are not derived from telemetry.

## Graph

`GET /api/v1/memory/graph` merges records into nodes of six types in fixed columns: failure signatures, dependency
paths, incidents, services, interventions, outcomes. Edges connect an incident to the things it exhibited, affected,
tried and ended with; intervention edges carry an effect (improved / mixed / worsened) decided by the replay's own
tolerances. The UI lets you filter by node type or text, hide simulated nodes, and click a node to highlight its
neighbourhood. There are no dangling edges (unit-tested).

## Endpoints

`GET /incidents/{id}/similar`, `GET /memory/records`, `GET /memory/graph`, `GET /memory/compare?a=&b=`,
`POST|DELETE /memory/seed`. The incident report includes a "Similar past incidents" section.
