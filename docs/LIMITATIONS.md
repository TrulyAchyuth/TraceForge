# Limitations

- **Replay is a model, not a prediction.** It re-times recorded traces under a stated intervention using sampled
  data, with imputed durations where an attempt was never observed. It cannot say what *would* have happened in
  production; the report lists what each result does not support.
- **Hypothesis scores are a rubric, not probabilities.** In the verified payment-latency run the second hypothesis
  ("gateway: request volume surged", 7/10) was a plausible false lead.
- **Incident Memory similarity is feature overlap** with fixed weights, over a small history. Demo records are
  hand-written. "What was tried before" is evidence from another incident.
- **Telemetry scope**: traces only (no metrics or logs). Sampling in the client is not corrected for.
- **Detection** uses windows and baselines from the same time series; a baseline that already contains an outage
  hides it. Thresholds are tunable in Settings, defaults are not tuned for any real system.
- **Single node**: one process, one Postgres, no HA. Spans that cannot be written after retries are dropped and counted
  (`spans_dropped`); see [RELIABILITY.md](RELIABILITY.md) for what was measured.
- **Security**: demonstration grade only; see [SECURITY.md](SECURITY.md).
- **Browser support** is tested only in headless Chromium.
