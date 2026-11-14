import { FormEvent, useState } from 'react';
import { api, Incident, Intervention, ReplayResult, ReplayRun, useApi } from '../api';
import { fmtMs, fmtPct } from '../lib';
import { query } from '../router';
import { Async, Badge, Empty, ErrorBox, LineChart, PageHead } from '../ui';

const TYPES: Record<string, string> = { scale_latency: 'Scale latency', remove_errors: 'Remove errors', retry_policy: 'Set retry attempts', set_timeout: 'Set timeout' };

export function ReplayPage() {
  const route = location.hash.slice(1);
  const q = query(route);
  const [incident, setIncident] = useState(q.get('incident') ?? '');
  const list = useApi<{ incidents: Incident[] }>('/incidents?limit=100');
  return (
    <>
      <PageHead title="Counterfactual replay" sub="Re-run a captured incident with one change to see what would likely have happened">
        <select aria-label="Incident" value={incident} onChange={e => setIncident(e.target.value)}>
          <option value="">Select an incident</option>{list.data?.incidents.map(i => <option key={i.id} value={i.id}>INC-{String(i.id).padStart(4, '0')} {i.title}</option>)}
        </select>
      </PageHead>
      <div className="notice sim">Results are <b>simulated</b>: a model replays recorded traces under your intervention. They are a what-if estimate, not an observed outcome.</div>
      {incident ? <Runner key={incident} id={incident} focus={q.get('run')} /> : <Empty title="Choose an incident" />}
    </>
  );
}

function Runner({ id, focus }: { id: string; focus: string | null }) {
  const inv = useApi<{ hypotheses: { service: string }[]; replay_options: { services: string[]; edges: { caller: string; callee: string }[] } | null }>(`/incidents/${id}/investigation`);
  const runs = useApi<{ replays: ReplayRun[] }>(`/incidents/${id}/replays`);
  const [iv, setIv] = useState<Intervention>({ type: 'scale_latency', mode: 'baseline' });
  const [seed, setSeed] = useState(7); const [err, setErr] = useState(''); const [busy, setBusy] = useState(false);
  const services = inv.data?.replay_options?.services ?? [];
  const suggested = inv.data?.hypotheses?.[0]?.service; // start from the most likely origin
  const current = iv.service ?? (suggested && services.includes(suggested) ? suggested : services[0]);
  const set = (p: Partial<Intervention>) => setIv({ ...iv, ...p });
  const run = (e: FormEvent) => {
    e.preventDefault(); setBusy(true); setErr('');
    const clean = { ...iv, service: current };
    api(`/incidents/${id}/replays`, { method: 'POST', body: { name: `${TYPES[iv.type]} on ${clean.service}`, seed, latency_objective_ms: 0, interventions: [clean] } })
      .then(runs.reload).catch(x => setErr(x.message)).finally(() => setBusy(false));
  };
  return (
    <>
      <form className="panel pad section" onSubmit={run}>
        <div className="row" style={{ flexWrap: 'wrap', alignItems: 'end' }}>
          <label className="f">Intervention<select value={iv.type} onChange={e => setIv({ type: e.target.value, mode: e.target.value === 'scale_latency' ? 'baseline' : undefined })}>{Object.entries(TYPES).map(([k, v]) => <option key={k} value={k}>{v}</option>)}</select></label>
          <label className="f">Service<select value={current ?? ''} onChange={e => set({ service: e.target.value })}>{services.map(s => <option key={s}>{s}</option>)}</select></label>
          {iv.type === 'scale_latency' && <label className="f">Mode<select value={iv.mode} onChange={e => set({ mode: e.target.value })}><option value="baseline">Restore to baseline</option><option value="factor">Multiply by factor</option></select></label>}
          {iv.type === 'scale_latency' && iv.mode === 'factor' && <label className="f">Factor<input type="number" step="0.1" min="0" max="20" value={iv.factor ?? 1} onChange={e => set({ factor: +e.target.value })} /></label>}
          {iv.type === 'retry_policy' && <><label className="f">Caller<input value={iv.caller ?? ''} onChange={e => set({ caller: e.target.value })} /></label><label className="f">Max attempts<input type="number" min="1" max="10" value={iv.max_attempts ?? 1} onChange={e => set({ max_attempts: +e.target.value })} /></label></>}
          {iv.type === 'set_timeout' && <><label className="f">Caller<input value={iv.caller ?? ''} onChange={e => set({ caller: e.target.value })} /></label><label className="f">Timeout ms<input type="number" min="1" value={iv.timeout_ms ?? 500} onChange={e => set({ timeout_ms: +e.target.value })} /></label></>}
          <label className="f">Seed<input type="number" value={seed} onChange={e => setSeed(+e.target.value)} /></label>
          <button className="btn primary" disabled={busy || services.length === 0}>{busy ? 'Running…' : 'Run replay'}</button>
        </div>
        {err && <ErrorBox message={err} />}
        {!inv.loading && services.length === 0 && <div className="notice bad">No captured dataset yet. Open the incident's Investigation tab to capture one.</div>}
      </form>
      <Async state={runs}>{d => d.replays.length === 0 ? <Empty title="No replays yet" /> : <>{d.replays.map((r, i) => <Result key={r.id} run={r} open={focus ? String(r.id) === focus : i === 0} />)}</>}</Async>
    </>
  );
}

const side = (title: string, s: ReplayResult['summary']['original'], tag: string) => (
  <div className="side simulated"><div className="tag">{tag}</div><h3>{title}</h3>
    <div className="metric"><span>Error rate</span><b>{fmtPct(s.error_rate)}</b></div><div className="metric"><span>p50</span><b>{fmtMs(s.p50_ms)}</b></div>
    <div className="metric"><span>p95</span><b>{fmtMs(s.p95_ms)}</b></div><div className="metric"><span>p99</span><b>{fmtMs(s.p99_ms)}</b></div></div>
);

/** The list endpoint omits results, so each run loads its own. */
function Result({ run: summary, open }: { run: ReplayRun; open: boolean }) {
  const full = useApi<{ replay: ReplayRun }>(`/replays/${summary.id}`);
  const run = full.data?.replay ?? summary;
  const r = run.result;
  if (!r) return <div className="panel pad section"><b>{summary.name}</b> {full.error ? <ErrorBox message={full.error} retry={full.reload} /> : <span className="mut">Loading result…</span>}</div>;
  const t = (b: string) => new Date(b).getTime();
  return (
    <details className="panel pad section" open={open}>
      <summary><b>{run.name}</b> <Badge kind="sim">simulated</Badge> <span className="mut">seed {run.seed} · dataset v{run.dataset_version}</span></summary>
      <div className="cmp section">{side('Recorded (modelled)', r.summary.original, 'Original')}{side('With intervention', r.summary.counterfactual, 'Counterfactual')}</div>
      <p className="mut">Replayed {r.diagnostics.traces_replayed} of {r.diagnostics.traces_in_dataset} captured traces; the intervention changed {r.diagnostics.traces_changed}. {r.diagnostics.traces_skipped_incomplete > 0 && `${r.diagnostics.traces_skipped_incomplete} incomplete traces were skipped. `}{r.diagnostics.imputed_durations > 0 && `${r.diagnostics.imputed_durations} unobserved durations were imputed from the baseline.`}</p>
      {r.series.length > 1 && <LineChart label="p95 over time, original vs counterfactual" format={fmtMs} series={[
        { name: 'original', color: 'var(--bad)', points: r.series.map(p => ({ t: t(p.bucket), v: p.original_p95_ms })) },
        { name: 'counterfactual', color: 'var(--ok)', dashed: true, points: r.series.map(p => ({ t: t(p.bucket), v: p.counterfactual_p95_ms })) }]} />}
      <h3>Findings</h3><ul>{r.findings.filter(f => f.changed).map((f, i) => <li key={i}>{f.scope} {f.metric}: {f.original.toFixed(2)} → {f.counterfactual.toFixed(2)} {f.unit}</li>)}</ul>
      <details><summary className="mut">Assumptions and limits</summary>
        <ul>{r.assumptions.map((x, i) => <li key={'a' + i}>{x}</li>)}{r.limitations.map((x, i) => <li key={'l' + i}>{x}</li>)}</ul>
        {r.unsupported_conclusions.length > 0 && <><b>Not supported by this replay</b><ul>{r.unsupported_conclusions.map((x, i) => <li key={i}>{x}</li>)}</ul></>}</details>
    </details>
  );
}
