import { FormEvent, useEffect, useMemo, useState } from 'react';
import { api, ServiceRow, Span, TraceDetail, TraceSummary, useApi } from '../api';
import { buildTree, fmtDateTime, fmtMs, fmtTime } from '../lib';
import { Async, Badge, Empty, ErrorBox, PageHead, RangePicker, useRange } from '../ui';

const HEX32 = /^[0-9a-f]{32}$/;

export function Traces() {
  const [range, setRange] = useRange();
  const [service, setService] = useState('');
  const [status, setStatus] = useState('');
  const [minMs, setMinMs] = useState('');
  const [id, setId] = useState('');
  const [idErr, setIdErr] = useState('');
  const [rows, setRows] = useState<TraceSummary[]>([]);
  const [next, setNext] = useState<string | null>(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const svc = useApi<{ services: ServiceRow[] }>(`/services?range=${range}`);
  const minOk = minMs === '' || (Number(minMs) >= 0 && Number.isFinite(Number(minMs)));

  const qs = useMemo(() => {
    const p = new URLSearchParams({ range, limit: '50' });
    if (service) p.set('service', service);
    if (status) p.set('status', status);
    if (minMs !== '' && minOk) p.set('min_duration_ms', minMs);
    return p.toString();
  }, [range, service, status, minMs, minOk]);

  useEffect(() => {
    let live = true;
    setBusy(true);
    api<{ traces: TraceSummary[]; next_cursor: string | null }>(`/traces?${qs}`)
      .then(r => { if (live) { setRows(r.traces); setNext(r.next_cursor); setErr(''); } })
      .catch(e => live && setErr(e.message))
      .finally(() => live && setBusy(false));
    return () => { live = false; };
  }, [qs]);

  const more = async () => {
    if (!next) return;
    setBusy(true);
    try { const r = await api<{ traces: TraceSummary[]; next_cursor: string | null }>(`/traces?${qs}&cursor=${encodeURIComponent(next)}`); setRows(x => [...x, ...r.traces]); setNext(r.next_cursor); }
    catch (e) { setErr((e as Error).message); } finally { setBusy(false); }
  };
  const open = (e: FormEvent) => {
    e.preventDefault();
    const v = id.trim().toLowerCase();
    if (!HEX32.test(v)) { setIdErr('A trace ID is 32 hexadecimal characters.'); return; }
    setIdErr(''); location.hash = `/traces/${v}`;
  };
  return (
    <>
      <PageHead title="Traces" sub="Recent requests, newest first"><RangePicker value={range} onChange={setRange} /></PageHead>
      <div className="row section" style={{ alignItems: 'flex-start' }}>
        <select aria-label="Service" value={service} onChange={e => setService(e.target.value)}><option value="">All services</option>{svc.data?.services.map(s => <option key={s.name}>{s.name}</option>)}</select>
        <select aria-label="Status" value={status} onChange={e => setStatus(e.target.value)}><option value="">Any status</option><option value="error">Errors only</option><option value="ok">OK only</option></select>
        <label className="row mut">Slower than <input type="number" min="0" aria-label="Minimum duration in milliseconds" aria-invalid={!minOk} style={{ width: 90 }} value={minMs} onChange={e => setMinMs(e.target.value)} /> ms</label>
        <form className="row" onSubmit={open}>
          <input type="text" className="mono" placeholder="Open by trace ID" aria-label="Trace ID" aria-invalid={!!idErr} value={id} onChange={e => setId(e.target.value)} style={{ width: 270 }} />
          <button className="btn">Open</button>{idErr && <span className="err-text">{idErr}</span>}
        </form>
      </div>
      {err && <ErrorBox message={err} />}
      <div className="panel table-wrap"><table>
        <thead><tr><th>Trace</th><th>Root operation</th><th className="r">Duration</th><th className="r">Spans</th><th>Services</th><th>Status</th><th>Started</th></tr></thead>
        <tbody>{rows.length === 0 ? <tr><td colSpan={7}>{busy ? <div className="empty"><span className="spinner" /></div> : <Empty title="No traces match">Widen the time range or relax the filters.</Empty>}</td></tr> : rows.map(t => (
          <tr key={t.trace_id} className="click" onClick={() => { location.hash = `/traces/${t.trace_id}`; }}>
            <td><a className="mono" href={`#/traces/${t.trace_id}`}>{t.trace_id.slice(0, 12)}</a></td><td>{t.root_service} <span className="mut">· {t.root_name}</span></td>
            <td className="r num">{fmtMs(t.duration_ns / 1e6)}</td><td className="r num">{t.span_count}</td><td className="mut">{t.services.join(', ')}</td>
            <td>{t.error_count > 0 ? <Badge kind="critical">{t.error_count} error{t.error_count > 1 ? 's' : ''}</Badge> : <Badge kind="ok">ok</Badge>}</td><td className="mut">{fmtTime(t.start_time)}</td></tr>))}</tbody></table></div>
      {next && <div className="row" style={{ justifyContent: 'center', marginTop: 12 }}><button className="btn" onClick={more} disabled={busy}>{busy ? 'Loading…' : 'Load more'}</button></div>}
    </>
  );
}

const colours = ['#7c9cff', '#4dd0e1', '#c792ea', '#3ecf8e', '#f0b429', '#ff9e64'];

export function TraceView({ id }: { id: string }) {
  const st = useApi<TraceDetail>(HEX32.test(id) ? `/traces/${id}` : null);
  const [sel, setSel] = useState<Span | null>(null);
  const [hideShort, setHideShort] = useState(false);
  if (!HEX32.test(id)) return <ErrorBox message="That is not a valid trace ID." />;
  return (
    <>
      <PageHead title="Trace" sub={<span className="mono">{id}</span>}><a className="btn" href="#/traces">Back to traces</a></PageHead>
      <Async state={st}>{d => {
        const { rows, totalMs } = buildTree(d.spans);
        const svcs = [...new Set(d.spans.map(s => s.service))].sort();
        const shown = hideShort ? rows.filter(r => r.durationMs / totalMs > 0.01 || r.span.status_code === 2) : rows;
        return (
          <>
            <div className="panel pad section row" style={{ gap: 28 }}>
              <div className="stat"><span className="v">{fmtMs(d.duration_ns / 1e6)}</span><span className="l">duration</span></div>
              <div className="stat"><span className="v">{d.span_count}</span><span className="l">spans</span></div>
              <div className="stat"><span className="v">{d.services.length}</span><span className="l">services</span></div>
              <div className="stat"><span className="v" style={{ color: d.error_count ? 'var(--bad)' : undefined }}>{d.error_count}</span><span className="l">errors</span></div>
              <div className="spacer" />
              <label className="row mut"><input type="checkbox" checked={hideShort} onChange={e => setHideShort(e.target.checked)} /> Hide spans under 1% of the trace</label>
            </div>
            {d.truncated && <div className="notice">This trace was truncated: some spans are not shown.</div>}
            <div className="split">
              <div className="panel table-wrap"><table>
                <thead><tr><th>Span</th><th style={{ width: '45%' }}>Timeline (0 – {fmtMs(totalMs)})</th><th className="r">Duration</th></tr></thead>
                <tbody>{shown.map(r => (
                  <tr key={r.span.span_id} className={`click ${sel?.span_id === r.span.span_id ? 'sel' : ''}`} onClick={() => setSel(r.span)} tabIndex={0} onKeyDown={e => e.key === 'Enter' && setSel(r.span)}>
                    <td><div className="tree-cell" style={{ paddingLeft: r.depth * 16 }}><span className="dot" style={{ background: colours[svcs.indexOf(r.span.service) % colours.length] }} /><b style={{ fontWeight: 500 }}>{r.span.service}</b><span className="mut">{r.span.name}</span>{r.span.status_code === 2 && <Badge kind="critical">error</Badge>}{!!r.span.resend_count && <Badge kind="warn">retry</Badge>}</div></td>
                    <td><div className="wf"><div className={`bar ${r.span.status_code === 2 ? 'err' : ''}`} style={{ left: `${(r.offsetMs / totalMs) * 100}%`, width: `${Math.max((r.durationMs / totalMs) * 100, 0.4)}%`, background: r.span.status_code === 2 ? undefined : colours[svcs.indexOf(r.span.service) % colours.length] }} /></div></td>
                    <td className="r num">{fmtMs(r.durationMs)}</td></tr>))}</tbody></table></div>
              <aside className="panel pad">{sel ? (<>
                <h3>{sel.name}</h3>
                <dl className="kv"><dt>Service</dt><dd>{sel.service}</dd><dt>Span ID</dt><dd className="mono">{sel.span_id}</dd><dt>Start</dt><dd>{fmtDateTime(sel.start_time)}</dd><dt>Duration</dt><dd>{fmtMs(sel.duration_ns / 1e6)}</dd><dt>Status</dt><dd>{sel.status_code === 2 ? `error ${sel.status_message ?? ''}` : 'ok'}</dd>{sel.peer_service && <><dt>Calls</dt><dd>{sel.peer_service}</dd></>}{!!sel.resend_count && <><dt>Retry</dt><dd>attempt after {sel.resend_count} earlier</dd></>}</dl>
                {sel.attributes && Object.keys(sel.attributes).length > 0 && <><h2 style={{ marginTop: 14 }}>Attributes</h2><dl className="kv">{Object.entries(sel.attributes).map(([k, v]) => <><dt key={k} className="mono">{k}</dt><dd className="mono">{String(v)}</dd></>)}</dl></>}
              </>) : <p className="mut">Select a span to see its attributes.</p>}</aside>
            </div>
          </>
        );
      }}</Async>
    </>
  );
}
