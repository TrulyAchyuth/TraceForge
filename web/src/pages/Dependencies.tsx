import { useMemo, useState } from 'react';
import { Edge, Overview, ServiceRow, useApi } from '../api';
import { fmtMs, fmtPct, layerServices } from '../lib';
import { Async, Badge, Empty, PageHead, RangePicker, useRange } from '../ui';

const W = 900, H = 380;

export function Dependencies() {
  const [range, setRange] = useRange();
  const dep = useApi<{ edges: Edge[]; compared_with_previous_window: boolean }>(`/dependencies?range=${range}`, 10_000);
  const svc = useApi<{ services: ServiceRow[] }>(`/services?range=${range}`, 10_000);
  const ov = useApi<Overview>('/overview?range=5m');
  const [sel, setSel] = useState<string | null>(null);
  const placed = useMemo(() => {
    if (!svc.data || !dep.data) return [];
    return layerServices(svc.data.services.map(s => s.name), dep.data.edges, ov.data?.entry_services ?? [], W, H);
  }, [svc.data, dep.data, ov.data]);
  const pos = new Map(placed.map(p => [p.name, p]));
  const status = new Map((svc.data?.services ?? []).map(s => [s.name, s.status]));
  const colour = (e: Edge) => (e.error_rate >= 0.05 ? 'var(--bad)' : e.retry_ratio >= 0.2 ? 'var(--warn)' : 'var(--dim)');
  const edgeKey = (e: Edge) => `${e.caller}>${e.callee}`;
  const chosen = dep.data?.edges.find(e => edgeKey(e) === sel);
  return (
    <>
      <PageHead title="Dependencies" sub="Calls between services, measured on client spans"><RangePicker value={range} onChange={setRange} /></PageHead>
      <Async state={dep}>{d => d.edges.length === 0 ? <div className="panel"><Empty title="No dependencies observed">Calls between services appear once traces with client and server spans arrive.</Empty></div> : (
        <div className="split">
          <div>
            <div className="panel pad" style={{ overflow: 'auto' }}>
              <svg viewBox={`0 0 ${W} ${H}`} width="100%" role="img" aria-label="Service dependency map" style={{ minWidth: 560 }}>
                <defs>{['dim', 'bad', 'warn'].map(c => <marker key={c} id={`ar-${c}`} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto"><path d="M0 0 8 4 0 8z" fill={`var(--${c})`} /></marker>)}</defs>
                {d.edges.map(e => {
                  const a = pos.get(e.caller), b = pos.get(e.callee);
                  if (!a || !b) return null;
                  const k = edgeKey(e), c = colour(e), id = c.includes('bad') ? 'bad' : c.includes('warn') ? 'warn' : 'dim';
                  const dx = b.x - a.x, dy = b.y - a.y, len = Math.hypot(dx, dy) || 1;
                  const sx = a.x + (dx / len) * 46, sy = a.y + (dy / len) * 16, ex = b.x - (dx / len) * 50, ey = b.y - (dy / len) * 18;
                  return <g key={k} onClick={() => setSel(k)} style={{ cursor: 'pointer' }} tabIndex={0} role="button" aria-label={`${e.caller} calls ${e.callee}`} onKeyDown={ev => ev.key === 'Enter' && setSel(k)}>
                    <path d={`M${sx},${sy} Q${(sx + ex) / 2},${(sy + ey) / 2 - 14} ${ex},${ey}`} fill="none" stroke={c} strokeWidth={Math.min(1.2 + Math.log10(e.calls + 1), 4.5)} opacity={sel && sel !== k ? 0.3 : 1} markerEnd={`url(#ar-${id})`} />
                    <path d={`M${sx},${sy} Q${(sx + ex) / 2},${(sy + ey) / 2 - 14} ${ex},${ey}`} fill="none" stroke="transparent" strokeWidth="14" />
                    <text x={(sx + ex) / 2} y={(sy + ey) / 2 - 16} textAnchor="middle" fontSize="10.5" fill="var(--mut)">{fmtMs(e.p95_ms)}</text>
                  </g>;
                })}
                {placed.map(p => (
                  <g key={p.name} transform={`translate(${p.x},${p.y})`} style={{ cursor: 'pointer' }} tabIndex={0} role="link" aria-label={`Open ${p.name}`} onClick={() => { location.hash = `/services/${p.name}`; }} onKeyDown={ev => ev.key === 'Enter' && (location.hash = `/services/${p.name}`)}>
                    <rect x="-44" y="-15" width="88" height="30" rx="7" fill="var(--raised)" stroke="var(--line2)" />
                    <circle cx="-31" cy="0" r="4" fill={status.get(p.name) === 'healthy' ? 'var(--ok)' : status.get(p.name) === 'critical' ? 'var(--bad)' : 'var(--warn)'} />
                    <text x="-22" y="4" fontSize="12" fill="var(--fg)">{p.name.length > 10 ? p.name.slice(0, 9) + '…' : p.name}</text>
                  </g>
                ))}
              </svg>
              <div className="legend"><span><i style={{ background: 'var(--dim)' }} />normal</span><span><i style={{ background: 'var(--warn)' }} />≥20% retries</span><span><i style={{ background: 'var(--bad)' }} />≥5% errors</span><span>Line width: call volume · label: p95</span></div>
            </div>
          </div>
          <aside className="panel pad" aria-live="polite">
            {chosen ? (<>
              <h3>{chosen.caller} → {chosen.callee}</h3>
              <dl className="kv"><dt>Calls</dt><dd>{chosen.calls} ({chosen.rate_per_sec}/s)</dd><dt>Errors</dt><dd>{chosen.errors} ({fmtPct(chosen.error_rate)})</dd><dt>Retries</dt><dd>{chosen.retries} ({fmtPct(chosen.retry_ratio)})</dd><dt>p50 / p95</dt><dd>{fmtMs(chosen.p50_ms)} / {fmtMs(chosen.p95_ms)}</dd>{chosen.change && <><dt>vs previous</dt><dd><Badge kind="warn">{chosen.change}</Badge></dd></>}</dl>
            </>) : <p className="mut">Select an edge to see call volume, errors, retries and latency.</p>}
          </aside>
        </div>
      )}</Async>
    </>
  );
}
