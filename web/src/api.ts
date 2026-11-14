import { useCallback, useEffect, useRef, useState } from 'react';

export class ApiError extends Error {
  constructor(public status: number, message: string) { super(message); }
}

const KEY = 'tf_token';
export const getToken = (): string => { try { return sessionStorage.getItem(KEY) ?? ''; } catch { return ''; } };
export const setToken = (t: string) => { try { t ? sessionStorage.setItem(KEY, t) : sessionStorage.removeItem(KEY); } catch { /* storage unavailable */ } };

type Listener = () => void;
const authListeners = new Set<Listener>();
export const onAuthRequired = (fn: Listener) => { authListeners.add(fn); return () => { authListeners.delete(fn); }; };

function headers(json: boolean): HeadersInit {
  const h: Record<string, string> = {};
  if (json) h['Content-Type'] = 'application/json';
  const t = getToken();
  if (t) h.Authorization = `Bearer ${t}`;
  return h;
}

export async function api<T>(path: string, init: { method?: string; body?: unknown; signal?: AbortSignal } = {}): Promise<T> {
  const r = await fetch('/api/v1' + path, {
    method: init.method ?? 'GET', signal: init.signal, headers: headers(init.body !== undefined),
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  });
  if (r.status === 401) { authListeners.forEach(f => f()); throw new ApiError(401, 'A valid API token is required.'); }
  const text = await r.text();
  let data: unknown = text;
  try { data = text ? JSON.parse(text) : null; } catch { /* plain text */ }
  if (!r.ok) throw new ApiError(r.status, (data as { error?: { message?: string } })?.error?.message ?? r.statusText);
  return data as T;
}

/** Downloads an authenticated resource and hands it to the browser as a file. */
export async function download(path: string, filename: string): Promise<void> {
  const r = await fetch('/api/v1' + path, { headers: headers(false) });
  if (r.status === 401) { authListeners.forEach(f => f()); throw new ApiError(401, 'A valid API token is required.'); }
  if (!r.ok) throw new ApiError(r.status, `download failed (${r.status})`);
  const url = URL.createObjectURL(await r.blob());
  const a = document.createElement('a');
  a.href = url; a.download = filename; document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

export interface State<T> { data?: T; error?: string; loading: boolean; reload: () => void }

/** Fetches `path` (null = skip) and refreshes every `every` ms while the tab is visible. */
export function useApi<T>(path: string | null, every = 0): State<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(path !== null);
  const [tick, setTick] = useState(0);
  const first = useRef(true);
  useEffect(() => {
    if (path === null) { setLoading(false); return; }
    const ctl = new AbortController();
    if (first.current || every === 0) setLoading(true);
    api<T>(path, { signal: ctl.signal })
      .then(d => { setData(d); setError(undefined); })
      .catch(e => { if (e.name !== 'AbortError') setError(e.message); })
      .finally(() => { if (!ctl.signal.aborted) { setLoading(false); first.current = false; } });
    return () => ctl.abort();
  }, [path, tick, every]);
  useEffect(() => {
    if (!every || path === null) return;
    const id = setInterval(() => { if (!document.hidden) setTick(t => t + 1); }, every);
    return () => clearInterval(id);
  }, [every, path]);
  useEffect(() => { first.current = true; setData(undefined); }, [path]);
  return { data, error, loading, reload: useCallback(() => setTick(t => t + 1), []) };
}

// ---- response types (only the fields the interface uses) ----

export interface Point { t: string; requests: number; errors: number; error_rate: number; rate_per_sec: number; p50_ms: number; p95_ms: number; p99_ms: number }
export interface ServiceRow {
  name: string; status: string; status_since?: string; last_seen?: string; requests: number; errors: number;
  error_rate: number; rate_per_sec: number; p50_ms: number; p95_ms: number; p99_ms: number; series?: Point[];
}
export interface Incident {
  id: number; key: string; title: string; severity: string; status: string; primary_service: string;
  affected_services: string[]; trigger_rule: string; started_at: string; detected_at: string; last_signal_at: string;
  resolved_at?: string; signals?: Signal[]; notes?: { id: number; author: string; body: string; created_at: string }[];
}
export interface Signal {
  id: number; rule: string; service: string; peer?: string; severity: string; value: number; baseline: number; threshold: number;
  peak_value: number; unit: string; message: string; first_seen: string; last_seen: string; active: boolean; exemplar_trace_ids: string[];
}
export interface Overview {
  active_incidents: number; entry_services: string[]; health_counts: Record<string, number>; system_health: string;
  range: { from: string; to: string; step_seconds: number }; recent_incidents: Incident[]; services: ServiceRow[];
  series: Point[]; totals: { requests: number; errors: number; error_rate: number; rate_per_sec: number; p50_ms: number; p95_ms: number; p99_ms: number };
  ingest: { spans_per_second_1m: number; spans_written: number; queue_depth: number; spans_dropped: number; write_errors: number; last_ingest_at?: string };
}
export interface Edge { caller: string; callee: string; calls: number; errors: number; retries: number; error_rate: number; retry_ratio: number; rate_per_sec: number; p50_ms: number; p95_ms: number; change: string }
export interface TraceSummary { trace_id: string; start_time: string; duration_ns: number; root_service: string; root_name: string; span_count: number; error_count: number; services: string[] }
export interface Span {
  trace_id: string; span_id: string; parent_span_id: string; service: string; name: string; kind: number; start_time: string;
  duration_ns: number; status_code: number; status_message?: string; attributes?: Record<string, unknown>; peer_service?: string; resend_count?: number; error_type?: string;
}
export interface TraceDetail { trace_id: string; spans: Span[]; span_count: number; error_count: number; services: string[]; duration_ns: number; truncated: boolean }
export interface Evidence { check: string; polarity: string; class: string; points: number; text: string; trace_ids?: string[] }
export interface Hypothesis { id: number; rank: number; kind: string; service: string; peer?: string; title: string; statement: string; score: number; max_score: number; evidence: Evidence[]; matches_known_fault?: boolean }
export interface TimelineEvent { id: number; time: string; kind: string; severity?: string; title: string; detail?: unknown; incident_id?: number; service?: string; trace_id?: string }
export interface ReplayOptions { services: string[]; edges: { caller: string; callee: string; observed_max_attempts: number; inferred_timeout_ms: number }[] }
export interface Investigation {
  incident: Incident; dataset: { id: number; version: number; trace_count: number; span_count: number; checksum: string; captured_at: string } | null;
  hypotheses: Hypothesis[]; known_faults: { experiment_id: number; name: string; targets: string[]; started_at: string; ended_at?: string }[];
  related_traces: { trace_id: string; operation: string; duration_ms: number; error: boolean; reason: string }[];
  timeline: TimelineEvent[]; replays: ReplayRun[]; replay_options: ReplayOptions | null; methodology: string[]; dataset_integrity_ok: boolean;
  analysis?: { services: ServiceProfile[] };
}
export interface ServiceProfile { service: string; baseline_p95_ms: number; incident_p95_ms: number; baseline_error_rate: number; incident_error_rate: number; baseline_requests: number; incident_requests: number; onset?: string }
export interface ReplayRun { id: number; incident_id: number; name: string; seed: number; dataset_version: number; created_at: string; interventions: Intervention[]; result?: ReplayResult }
export interface Intervention { type: string; service?: string; mode?: string; factor?: number; caller?: string; callee?: string; max_attempts?: number; timeout_ms?: number }
export interface Stat { count: number; errors: number; error_rate: number; mean_ms: number; p50_ms: number; p95_ms: number; p99_ms: number }
export interface Cmp { name: string; original: Stat; counterfactual: Stat }
export interface ReplayResult {
  evidence_class: string; engine_version: string; scenario: { name: string; interventions: Intervention[]; seed: number };
  summary: { original: Stat; counterfactual: Stat; original_retries: number; counterfactual_retries: number; objective_ms: number; original_breach_rate: number; counterfactual_breach_rate: number; sampling_fraction_percent: number };
  services: Cmp[]; operations: Cmp[]; series: { bucket: string; original_error_rate: number; counterfactual_error_rate: number; original_p95_ms: number; counterfactual_p95_ms: number }[];
  findings: { scope: string; metric: string; unit: string; original: number; counterfactual: number; changed: boolean }[];
  assumptions: string[]; limitations: string[]; unsupported_conclusions: string[]; diagnostics: { traces_in_dataset: number; traces_replayed: number; traces_skipped_incomplete: number; traces_changed: number; censored_attempts: number; imputed_durations: number };
}
export interface MemoryRecord {
  id: number; key: string; incident_id?: number; simulated: boolean; title: string; severity: string; trigger_rule: string; started_at: string;
  primary_service: string; affected_services: string[]; signals: { rule: string; service: string; peer?: string; severity: string }[];
  dependency_edges: string[]; event_sequence: string[]; hypotheses: { rank: number; kind: string; service: string; title: string; score: number; max_score: number; supporting: string[]; contradicting: string[] }[];
  known_faults: string[]; interventions: MemIntervention[]; operator_notes: string[]; recovery: { status: string; duration_seconds: number; cause: string };
  limitations: string[]; fingerprint: string[]; example_trace_ids: string[];
}
export interface MemIntervention { source: string; class: string; name: string; detail: string; effect: string; summary: string }
export interface Match {
  record: MemoryRecord; overlap: number; shared_features: string[]; only_in_current: string[]; only_in_historical: string[]; differences: string[]; interventions: MemIntervention[];
}
export interface Similarity {
  method_version: string; weights: Record<string, number>; fingerprint: string[]; historical_count: number; simulated_count: number; matches: Match[];
  recurring_patterns: { feature: string; incidents: string[] }[]; commonly_affected_services: { feature: string; incidents: string[] }[]; notes: string[];
}
export interface GraphNode { id: string; type: string; label: string; simulated?: boolean; severity?: string; detail?: string }
export interface GraphEdge { source: string; target: string; kind: string; effect?: string; note?: string }
export interface MemoryGraphData { nodes: GraphNode[]; edges: GraphEdge[] }
export interface LabScenario { id: string; name: string; description: string; expected: string; faults: Record<string, { latency_ms?: number; error_rate?: number; unavailable?: boolean; rate_multiplier?: number }>; default_duration_seconds: number; custom: boolean }
export interface Experiment { id: number; scenario: string; name: string; targets: string[]; status: string; started_at: string; ended_at?: string; seed: number }
