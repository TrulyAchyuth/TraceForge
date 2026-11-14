import { FormEvent, useState } from 'react';
import { api, download, Incident, Investigation, Match, Similarity, useApi } from '../api';
import { ago, fmtDateTime, fmtMs, fmtPct } from '../lib';
import { query } from '../router';
import { Async, Badge, Chips, Empty, ErrorBox, PageHead, SimBadge, Tabs } from '../ui';

const STATUSES = ['open', 'acknowledged', 'investigating', 'resolved', 'closed'];

export function Incidents() {
  const [status, setStatus] = useState('');
  const st = useApi<{ incidents: Incident[]; total: number }>(`/incidents?limit=100${status ? `&status=${status}` : ''}`, 10_000);
  return (
    <>
      <PageHead title="Incidents" sub="Detected from live telemetry">
        <select aria-label="Filter by status" value={status} onChange={e => setStatus(e.target.value)}>
          <option value="">All</option><option value="active">Active</option>{STATUSES.map(s => <option key={s}>{s}</option>)}
        </select>
      </PageHead>
      <Async state={st}>{d => d.incidents.length === 0 ? <Empty title="No incidents">Start an experiment in the failure lab to produce one.</Empty> : (
        <div className="panel">{d.incidents.map(i => (
          <a key={i.id} className="item" href={`#/incidents/${i.id}`}>
            <Badge kind={i.severity}>{i.severity}</Badge>
            <div className="grow"><div className="title">{i.title}</div><div className="mut">INC-{String(i.id).padStart(4, '0')} · {i.primary_service} · started {ago(i.started_at)}</div></div>
            <Badge kind={i.status}>{i.status}</Badge>
          </a>))}</div>
      )}</Async>
    </>
  );
}

type Tab = 'summary' | 'investigation' | 'similar' | 'replays' | 'notes';

export function IncidentDetail({ id, route }: { id: string; route: string }) {
  const [tab, setTab] = useState<Tab>((query(route).get('tab') as Tab) || 'summary');
  const [err, setErr] = useState('');
  const inc = useApi<{ incident: Incident; rules: Record<string, string> }>(`/incidents/${id}`, 10_000);
  const setStatus = (status: string) => api(`/incidents/${id}`, { method: 'PATCH', body: { status } }).then(inc.reload).catch(e => setErr(e.message));
  const dl = (f: string) => download(`/incidents/${id}/report?format=${f}`, `incident-${id}.${f}`).catch(e => setErr(e.message));
  return (
    <>
      <Async state={inc}>{({ incident: i, rules }) => (
        <>
          <PageHead title={i.title} sub={<span className="row"><Badge kind={i.severity}>{i.severity}</Badge><Badge kind={i.status}>{i.status}</Badge>INC-{String(i.id).padStart(4, '0')} · detected {ago(i.detected_at)}</span>}>
            <select aria-label="Set status" value={i.status} onChange={e => setStatus(e.target.value)}>{STATUSES.map(s => <option key={s}>{s}</option>)}</select>
            {['md', 'html', 'json'].map(f => <button key={f} className="btn sm" onClick={() => dl(f)}>Report .{f}</button>)}
          </PageHead>
          {err && <ErrorBox message={err} />}
          <Tabs value={tab} onChange={setTab} tabs={[{ id: 'summary', label: 'Summary' }, { id: 'investigation', label: 'Investigation' }, { id: 'similar', label: 'Similar incidents' }, { id: 'replays', label: 'Replays' }, { id: 'notes', label: 'Notes' }]} />
          {tab === 'summary' && (
            <div className="grid g2">
              <section><h2>Details</h2><dl className="panel pad kv">
                <dt>Primary service</dt><dd><a href={`#/services/${i.primary_service}`}>{i.primary_service}</a></dd>
                <dt>Affected</dt><dd>{i.affected_services.join(', ')}</dd><dt>Rule</dt><dd>{i.trigger_rule}</dd>
                <dt>Started</dt><dd>{fmtDateTime(i.started_at)}</dd><dt>Resolved</dt><dd>{fmtDateTime(i.resolved_at)}</dd></dl></section>
              <section><h2>Signals</h2><div className="panel">{(i.signals ?? []).map(s => (
                <div key={s.id} className="item"><Badge kind={s.severity}>{s.severity}</Badge><div className="grow">{s.message}<div className="mut" title={rules[s.rule]}>{s.rule} · {s.service}{s.peer ? ` → ${s.peer}` : ''}</div></div></div>))}</div></section>
            </div>)}
          {tab === 'investigation' && <InvestigationTab id={id} />}
          {tab === 'similar' && <SimilarTab id={id} />}
          {tab === 'replays' && <ReplaysTab id={id} />}
          {tab === 'notes' && <NotesTab id={id} notes={i.notes ?? []} reload={inc.reload} />}
        </>)}</Async>
    </>
  );
}

function InvestigationTab({ id }: { id: string }) {
  const st = useApi<Investigation>(`/incidents/${id}/investigation`);
  const [busy, setBusy] = useState(false); const [err, setErr] = useState('');
  const capture = () => { setBusy(true); api(`/incidents/${id}/investigation`, { method: 'POST' }).then(st.reload).catch(e => setErr(e.message)).finally(() => setBusy(false)); };
  if (st.error && !st.data) return <><ErrorBox message={st.error} /><button className="btn primary" disabled={busy} onClick={capture}>Capture dataset and investigate</button></>;
  return <Async state={st}>{d => (
    <>
      {err && <ErrorBox message={err} />}
      <div className="row section">
        <span className="mut">{d.dataset ? `Dataset v${d.dataset.version}: ${d.dataset.trace_count} traces, ${d.dataset.span_count} spans` : 'No dataset captured'}</span>
        <Badge kind={d.dataset_integrity_ok ? 'ok' : 'bad'}>{d.dataset_integrity_ok ? 'integrity verified' : 'integrity check failed'}</Badge>
        <div className="spacer" /><button className="btn sm" disabled={busy} onClick={capture}>Re-capture</button>
      </div>
      <h2>Ranked hypotheses</h2>
      <div className="panel">{d.hypotheses.length === 0 ? <Empty title="No hypotheses" /> : d.hypotheses.map(h => (
        <details key={h.id} className="item" style={{ display: 'block' }} open={h.rank === 1}>
          <summary><b>#{h.rank}</b> {h.title} <Badge>{h.score}/{h.max_score}</Badge> {h.matches_known_fault && <Badge kind="warn">matches injected fault</Badge>}</summary>
          <p className="mut">{h.statement}</p>
          <ul>{h.evidence.map((e, k) => <li key={k}><Badge kind={e.polarity === 'against' ? 'bad' : 'ok'}>{e.polarity}</Badge> <span className="mut">[{e.class}]</span> {e.text} ({e.points >= 0 ? '+' : ''}{e.points})</li>)}</ul>
        </details>))}</div>
      {d.analysis && <><h2 style={{ marginTop: 20 }}>Baseline vs incident</h2><div className="panel table-wrap"><table>
        <thead><tr><th>Service</th><th className="r">p95 before</th><th className="r">p95 during</th><th className="r">Errors before</th><th className="r">Errors during</th></tr></thead>
        <tbody>{d.analysis.services.map(s => <tr key={s.service}><td>{s.service}</td><td className="r num">{fmtMs(s.baseline_p95_ms)}</td><td className="r num">{fmtMs(s.incident_p95_ms)}</td><td className="r num">{fmtPct(s.baseline_error_rate)}</td><td className="r num">{fmtPct(s.incident_error_rate)}</td></tr>)}</tbody></table></div></>}
      <h2 style={{ marginTop: 20 }}>Related traces</h2>
      <div className="panel">{d.related_traces.map(t => <a key={t.trace_id} className="item" href={`#/traces/${t.trace_id}`}><Badge kind={t.error ? 'bad' : ''}>{t.error ? 'error' : 'slow'}</Badge><div className="grow">{t.operation}<div className="mut">{t.reason}</div></div><span className="num">{fmtMs(t.duration_ms)}</span></a>)}</div>
      <details style={{ marginTop: 20 }}><summary className="mut">Methodology</summary><ul>{d.methodology.map((m, i) => <li key={i}>{m}</li>)}</ul></details>
    </>)}</Async>;
}

export function MatchCard({ m }: { m: Match }) {
  return (
    <div className="panel pad section">
      <div className="row"><b>{m.record.title}</b>{m.record.simulated && <SimBadge />}<div className="spacer" /><Badge>{Math.round(m.overlap * 100)}% overlap</Badge></div>
      <div className="mut">{m.record.primary_service} · recovery: {m.record.recovery.status}{m.record.recovery.cause ? ` (${m.record.recovery.cause})` : ''}</div>
      <p><Chips items={m.shared_features} kind="shared" /><Chips items={m.only_in_current} kind="diff" /><Chips items={m.only_in_historical} /></p>
      {m.differences.length > 0 && <ul>{m.differences.map((x, i) => <li key={i}>{x}</li>)}</ul>}
      {m.interventions.length > 0 && <><div className="mut">What was tried before</div><ul>{m.interventions.map((v, i) => <li key={i}><Badge>{v.class}</Badge> {v.summary || v.name} <span className="mut">→ {v.effect}</span></li>)}</ul></>}
    </div>
  );
}

function SimilarTab({ id }: { id: string }) {
  const st = useApi<Similarity>(`/incidents/${id}/similar`);
  return <Async state={st}>{d => (
    <>
      <p className="mut">Shared features are green, features only in this incident amber. Matches are ranked by weighted overlap ({d.method_version}); {d.historical_count} real and {d.simulated_count} simulated records searched.</p>
      {d.matches.length === 0 ? <Empty title="No similar incidents yet">Resolved incidents are stored automatically. Demonstration records can be loaded from the Memory graph page.</Empty> : d.matches.map(m => <MatchCard key={m.record.id} m={m} />)}
      {d.notes.map((n, i) => <div key={i} className="notice info">{n}</div>)}
    </>)}</Async>;
}

function ReplaysTab({ id }: { id: string }) {
  const st = useApi<{ replays: { id: number; name: string; created_at: string; seed: number }[] }>(`/incidents/${id}/replays`);
  return <Async state={st}>{d => (
    <>
      <a className="btn primary" href={`#/replay?incident=${id}`}>New counterfactual replay</a>
      <div className="panel section">{d.replays.length === 0 ? <Empty title="No replays yet" /> : d.replays.map(r => <a key={r.id} className="item" href={`#/replay?incident=${id}&run=${r.id}`}><div className="grow">{r.name}</div><span className="mut">seed {r.seed} · {ago(r.created_at)}</span></a>)}</div>
    </>)}</Async>;
}

function NotesTab({ id, notes, reload }: { id: string; notes: { id: number; author: string; body: string; created_at: string }[]; reload: () => void }) {
  const [body, setBody] = useState(''); const [err, setErr] = useState('');
  const submit = (e: FormEvent) => { e.preventDefault(); api(`/incidents/${id}/notes`, { method: 'POST', body: { author: 'operator', body } }).then(() => { setBody(''); reload(); }).catch(x => setErr(x.message)); };
  return (
    <>
      <form onSubmit={submit} className="panel pad"><label className="f">Add a note<textarea rows={3} value={body} maxLength={4000} onChange={e => setBody(e.target.value)} /></label>
        {err && <ErrorBox message={err} />}<div className="row" style={{ marginTop: 8 }}><button className="btn primary" disabled={!body.trim()}>Add note</button></div></form>
      <div className="panel section">{notes.length === 0 ? <Empty title="No notes" /> : notes.map(n => <div key={n.id} className="item"><div className="grow">{n.body}<div className="mut">{n.author} · {fmtDateTime(n.created_at)}</div></div></div>)}</div>
    </>
  );
}
