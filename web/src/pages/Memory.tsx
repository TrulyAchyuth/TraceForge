import { useMemo, useState } from 'react';
import { api, MemoryGraphData, MemoryRecord, useApi } from '../api';
import { connected, GraphView, layoutGraph, NODE_COLUMNS, NODE_LABEL } from '../lib';
import { Async, Badge, Chips, Empty, ErrorBox, PageHead, SimBadge, Tabs } from '../ui';

type Tab = 'graph' | 'records' | 'compare';
interface Cmp { a: MemoryRecord; b: MemoryRecord; overlap: number; shared_features: string[]; only_in_a: string[]; only_in_b: string[]; differences: string[] }

export function MemoryPage({ route: _route }: { route: string }) {
  const [tab, setTab] = useState<Tab>('graph');
  const [err, setErr] = useState('');
  const recs = useApi<{ records: MemoryRecord[] }>('/memory/records');
  const seed = (m: 'POST' | 'DELETE') => api('/memory/seed', { method: m }).then(() => location.reload()).catch(e => setErr(e.message));
  return (
    <>
      <PageHead title="Incident memory" sub="Past incidents, what they looked like and what was tried">
        <button className="btn sm" onClick={() => seed('POST')}>Load demo records</button>
        <button className="btn sm" onClick={() => seed('DELETE')}>Remove demo records</button>
      </PageHead>
      {err && <ErrorBox message={err} />}
      <div className="notice sim">Demo records are hand-written examples labelled <b>simulated</b>. They are never mixed with records derived from your telemetry without a label.</div>
      <Tabs value={tab} onChange={setTab} tabs={[{ id: 'graph', label: 'Graph' }, { id: 'records', label: 'Records' }, { id: 'compare', label: 'Compare' }]} />
      {tab === 'graph' && <Graph />}
      {tab !== 'graph' && <Async state={recs}>{d => tab === 'records' ? <Records list={d.records} /> : <Compare list={d.records} />}</Async>}
    </>
  );
}

function Graph() {
  const st = useApi<{ graph: MemoryGraphData }>('/memory/graph');
  const [types, setTypes] = useState(new Set<string>(NODE_COLUMNS));
  const [hideSim, setHideSim] = useState(false); const [q, setQ] = useState(''); const [sel, setSel] = useState<string | null>(null);
  const view: GraphView | null = useMemo(() => st.data ? layoutGraph(st.data.graph, { types, hideSimulated: hideSim, query: q }) : null, [st.data, types, hideSim, q]);
  return (
    <Async state={st}>{() => !view || view.nodes.length === 0 ? <Empty title="The graph is empty">Resolve an incident or load demo records.</Empty> : (() => {
      const hot = sel ? connected(view, sel) : null;
      const byId = new Map(view.nodes.map(n => [n.id, n]));
      const node = sel ? byId.get(sel) : undefined;
      return (
        <>
          <div className="row section" style={{ flexWrap: 'wrap' }}>
            <input type="search" placeholder="Filter by service or signature" aria-label="Filter graph" value={q} onChange={e => setQ(e.target.value)} />
            {NODE_COLUMNS.map(c => <label key={c} className="row"><input type="checkbox" checked={types.has(c)} onChange={() => { const n = new Set(types); n.has(c) ? n.delete(c) : n.add(c); setTypes(n); }} />{NODE_LABEL[c]}</label>)}
            <label className="row"><input type="checkbox" checked={hideSim} onChange={e => setHideSim(e.target.checked)} />Hide simulated</label>
          </div>
          <div className="graph-box"><svg viewBox={`0 0 ${view.width} ${view.height}`} width="100%" style={{ minWidth: 760 }} role="img" aria-label="Incident memory graph">
            {NODE_COLUMNS.map(c => { const n = view.nodes.find(x => x.type === c); return n ? <text key={c} className="gcol" x={n.x} y={18} textAnchor="middle">{NODE_LABEL[c].toUpperCase()}</text> : null; })}
            {view.edges.map((e, i) => { const a = byId.get(e.source)!, b = byId.get(e.target)!; const on = hot?.edges.includes(e);
              return <path key={i} className={`gedge ${e.effect ?? ''} ${on ? 'hot' : hot ? 'dim' : ''}`} d={`M${a.x},${a.y}C${(a.x + b.x) / 2},${a.y} ${(a.x + b.x) / 2},${b.y} ${b.x},${b.y}`} />; })}
            {view.nodes.map(n => <g key={n.id} className={`gnode t-${n.type} ${n.simulated ? 'sim' : ''} ${sel === n.id ? 'sel' : ''} ${hot && !hot.nodes.has(n.id) ? 'dim' : ''}`} tabIndex={0} role="button" aria-label={`${n.type} ${n.label}`}
              onClick={() => setSel(sel === n.id ? null : n.id)} onKeyDown={e => { if (e.key === 'Enter') setSel(sel === n.id ? null : n.id); }}>
              <title>{n.label}</title><rect x={n.x - 86} y={n.y - 13} width={172} height={26} rx={6} /><text x={n.x} y={n.y + 4} textAnchor="middle">{n.label.length > 26 ? n.label.slice(0, 25) + '…' : n.label}</text></g>)}
          </svg></div>
          {node && <div className="panel pad section"><b>{node.label}</b> <Badge>{node.type}</Badge> {node.simulated && <SimBadge />}{node.detail && <p className="mut">{node.detail}</p>}
            {hot!.edges.filter(e => e.note).map((e, i) => <p key={i} className="mut">{e.kind}: {e.note}</p>)}</div>}
        </>);
    })()}</Async>
  );
}

function Records({ list }: { list: MemoryRecord[] }) {
  if (list.length === 0) return <Empty title="No records yet" />;
  return <div className="panel">{list.map(r => (
    <details key={r.id} className="item" style={{ display: 'block' }}>
      <summary>{r.title} {r.simulated && <SimBadge />} <Badge kind={r.severity}>{r.severity}</Badge> <span className="mut">{r.primary_service} · {r.recovery.status}</span></summary>
      <p><Chips items={r.fingerprint} /></p>
      {r.hypotheses.slice(0, 2).map(h => <p key={h.rank} className="mut">#{h.rank} {h.title} ({h.score}/{h.max_score})</p>)}
      {r.interventions.map((v, i) => <p key={i}><Badge>{v.class}</Badge> {v.summary || v.name} <span className="mut">→ {v.effect}</span></p>)}
      {r.limitations.length > 0 && <ul className="mut">{r.limitations.map((l, i) => <li key={i}>{l}</li>)}</ul>}
    </details>))}</div>;
}

function Compare({ list }: { list: MemoryRecord[] }) {
  const [a, setA] = useState(''); const [b, setB] = useState('');
  const st = useApi<Cmp>(a && b && a !== b ? `/memory/compare?a=${a}&b=${b}` : null);
  const pick = (v: string, f: (s: string) => void, l: string) => <select aria-label={l} value={v} onChange={e => f(e.target.value)}><option value="">{l}</option>{list.map(r => <option key={r.id} value={r.id}>{r.title}</option>)}</select>;
  return (
    <>
      <div className="row section">{pick(a, setA, 'Record A')}{pick(b, setB, 'Record B')}</div>
      {a && a === b && <div className="notice bad">Choose two different records.</div>}
      {a && b && a !== b && <Async state={st}>{c => (
        <div className="panel pad"><p><Badge>{Math.round(c.overlap * 100)}% overlap</Badge></p>
          <p><Chips items={c.shared_features} kind="shared" /></p><p>Only in A: <Chips items={c.only_in_a} kind="diff" /></p><p>Only in B: <Chips items={c.only_in_b} kind="diff" /></p>
          <ul>{c.differences.map((d, i) => <li key={i}>{d}</li>)}</ul></div>)}</Async>}
    </>
  );
}
