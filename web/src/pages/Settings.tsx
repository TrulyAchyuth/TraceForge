import { useState } from 'react';
import { api, useApi } from '../api';
import { fmtDateTime } from '../lib';
import { Async, Badge, ErrorBox, PageHead } from '../ui';

type Cfg = Record<string, number>;
interface Sys { version: string; started_at: string; auth: { api_token_required: boolean; ingest_token_required: boolean }; retention_hours: number; replay_engine: string; data_source: string; stored: Record<string, number>; detection: { last_evaluation?: string; last_error?: string } }

export function Settings() {
  const sys = useApi<Sys>('/system', 15_000);
  const det = useApi<{ config: Cfg; defaults: Cfg; rules: Record<string, string> }>('/settings/detection');
  const [draft, setDraft] = useState<Cfg | null>(null); const [msg, setMsg] = useState(''); const [err, setErr] = useState('');
  const save = (c: Cfg) => api('/settings/detection', { method: 'PUT', body: c }).then(() => { setMsg('Saved.'); setErr(''); setDraft(null); det.reload(); }).catch(e => { setErr(e.message); setMsg(''); });
  return (
    <>
      <PageHead title="Settings" sub="System status and detection thresholds" />
      <Async state={sys}>{s => <dl className="panel pad kv section">
        <dt>Version</dt><dd>{s.version} · replay engine {s.replay_engine}</dd><dt>Started</dt><dd>{fmtDateTime(s.started_at)}</dd>
        <dt>API token</dt><dd><Badge kind={s.auth.api_token_required ? 'ok' : 'warn'}>{s.auth.api_token_required ? 'required' : 'not set (open)'}</Badge></dd>
        <dt>Ingest token</dt><dd><Badge kind={s.auth.ingest_token_required ? 'ok' : 'warn'}>{s.auth.ingest_token_required ? 'required' : 'not set'}</Badge></dd>
        <dt>Retention</dt><dd>{s.retention_hours} h</dd><dt>Last detection run</dt><dd>{fmtDateTime(s.detection.last_evaluation)}{s.detection.last_error && <span className="mut"> · {s.detection.last_error}</span>}</dd>
        <dt>Data source</dt><dd className="mut">{s.data_source}</dd></dl>}</Async>
      <h2>Detection thresholds</h2>
      {msg && <div className="notice info">{msg}</div>}{err && <ErrorBox message={err} />}
      <Async state={det}>{d => { const c = draft ?? d.config; return (
        <form className="panel pad" onSubmit={e => { e.preventDefault(); save(c); }}>
          <div className="grid g3">{Object.keys(d.defaults).map(k => <label key={k} className="f">{k}<input type="number" step="any" value={c[k]} onChange={e => setDraft({ ...c, [k]: +e.target.value })} /></label>)}</div>
          <div className="row" style={{ marginTop: 12 }}><button className="btn primary" disabled={!draft}>Save</button><button type="button" className="btn" onClick={() => save(d.defaults)}>Restore defaults</button></div>
        </form>); }}</Async>
    </>
  );
}
