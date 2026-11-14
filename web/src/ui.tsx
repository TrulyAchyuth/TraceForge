import { ReactNode, useEffect, useMemo, useRef, useState } from 'react';

export const Icon = ({ name, size = 16 }: { name: string; size?: number }) => {
  const d: Record<string, string> = {
    overview: 'M3 3h7v7H3zM14 3h7v4h-7zM14 11h7v10h-7zM3 14h7v7H3z',
    services: 'M12 2 3 7v10l9 5 9-5V7zM3 7l9 5 9-5M12 12v10',
    map: 'M5 6a2 2 0 1 0 0 .01M19 6a2 2 0 1 0 0 .01M12 19a2 2 0 1 0 0 .01M6.7 7.5 11 17M17.3 7.5 13 17M7 6h10',
    traces: 'M3 6h12M6 12h14M9 18h9',
    incidents: 'M12 3 2 20h20zM12 10v4M12 17v.01',
    memory: 'M6 6a2.5 2.5 0 1 0 0 .01M18 8a2.5 2.5 0 1 0 0 .01M10 18a2.5 2.5 0 1 0 0 .01M8.3 7.2l7.4.6M7.3 8.3l2 7.5M16.8 10.2l-5 6',
    lab: 'M9 3h6M10 3v6L4.5 19a1.5 1.5 0 0 0 1.3 2h12.4a1.5 1.5 0 0 0 1.3-2L14 9V3M7.5 15h9',
    timeline: 'M12 4v16M8 8h-3M16 12h3M8 16H5',
    reports: 'M7 3h8l4 4v14H7zM15 3v4h4M10 12h6M10 16h6',
    settings: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19 12a7 7 0 0 0-.1-1.2l2-1.6-2-3.4-2.4 1a7 7 0 0 0-2-1.2L14 3h-4l-.5 2.6a7 7 0 0 0-2 1.2l-2.4-1-2 3.4 2 1.6A7 7 0 0 0 5 12c0 .4 0 .8.1 1.2l-2 1.6 2 3.4 2.4-1a7 7 0 0 0 2 1.2L10 21h4l.5-2.6a7 7 0 0 0 2-1.2l2.4 1 2-3.4-2-1.6c.1-.4.1-.8.1-1.2z',
    menu: 'M4 6h16M4 12h16M4 18h16',
    refresh: 'M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7',
    download: 'M12 4v11M7 11l5 5 5-5M5 20h14',
    chevron: 'M9 6l6 6-6 6',
    play: 'M7 4v16l13-8z', stop: 'M6 6h12v12H6z', check: 'M5 12l5 5 9-10', replay: 'M4 12a8 8 0 1 1 3 6.2M4 19v-5h5',
  };
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d={d[name] ?? ''} />
    </svg>
  );
};

export const Badge = ({ kind, children }: { kind?: string; children: ReactNode }) => <span className={`badge ${kind ?? ''}`}>{children}</span>;
export const SimBadge = () => <span className="badge sim" title="Hand-written demonstration record, not derived from telemetry">simulated example</span>;
export const StatusDot = ({ status, pulse }: { status: string; pulse?: boolean }) => <span className={`dot ${status} ${pulse ? 'pulse' : ''}`} role="img" aria-label={status} />;

export const Loading = ({ rows = 3 }: { rows?: number }) => (
  <div className="panel pad" aria-busy="true" aria-label="Loading">
    {Array.from({ length: rows }, (_, i) => <div key={i} className="skeleton" style={{ width: `${90 - i * 18}%`, margin: '10px 0' }} />)}
  </div>
);
export const Empty = ({ title, children }: { title: string; children?: ReactNode }) => <div className="empty"><strong>{title}</strong>{children}</div>;
export const ErrorBox = ({ message, retry }: { message: string; retry?: () => void }) => (
  <div className="notice bad" role="alert">{message} {retry && <button className="btn sm" onClick={retry}>Retry</button>}</div>
);

export function Async<T>({ state, children, rows }: { state: { data?: T; error?: string; loading: boolean; reload: () => void }; children: (d: T) => ReactNode; rows?: number }) {
  if (state.data !== undefined) return <>{state.error && <ErrorBox message={`Refresh failed: ${state.error}`} retry={state.reload} />}{children(state.data)}</>;
  if (state.error) return <ErrorBox message={state.error} retry={state.reload} />;
  return <Loading rows={rows} />;
}

export const PageHead = ({ title, sub, children }: { title: string; sub?: ReactNode; children?: ReactNode }) => (
  <div className="page-head"><div><h1>{title}</h1>{sub && <div className="sub">{sub}</div>}</div><div className="spacer" />{children}</div>
);

export function Tabs<T extends string>({ tabs, value, onChange }: { tabs: { id: T; label: string }[]; value: T; onChange: (t: T) => void }) {
  return (
    <div className="tabs" role="tablist">
      {tabs.map(t => <button key={t.id} role="tab" aria-selected={t.id === value} className={`tab ${t.id === value ? 'on' : ''}`} onClick={() => onChange(t.id)}>{t.label}</button>)}
    </div>
  );
}

export const Chips = ({ items, kind }: { items: string[]; kind?: 'shared' | 'diff' }) => <>{items.map(i => <span key={i} className={`chip ${kind ?? ''}`}>{i}</span>)}</>;

// ---- charts ----

export interface ChartSeries { name: string; color: string; points: { t: number; v: number }[]; dashed?: boolean }

export function LineChart({ series, height = 150, format = (v: number) => String(Math.round(v * 100) / 100), min0 = true, label }: {
  series: ChartSeries[]; height?: number; format?: (v: number) => string; min0?: boolean; label: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [w, setW] = useState(600);
  const [hover, setHover] = useState<number | null>(null);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver(e => setW(Math.max(240, e[0].contentRect.width)));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  const pad = { l: 44, r: 8, t: 8, b: 20 };
  const all = series.flatMap(s => s.points);
  const geo = useMemo(() => {
    if (all.length === 0) return null;
    const t0 = Math.min(...all.map(p => p.t)), t1 = Math.max(...all.map(p => p.t));
    let lo = min0 ? 0 : Math.min(...all.map(p => p.v)), hi = Math.max(...all.map(p => p.v));
    if (hi <= lo) hi = lo + 1;
    return { t0, t1: t1 === t0 ? t0 + 1 : t1, lo, hi };
  }, [all, min0]);
  if (!geo) return <div className="empty" ref={ref}>No data in this range.</div>;
  const x = (t: number) => pad.l + ((t - geo.t0) / (geo.t1 - geo.t0)) * (w - pad.l - pad.r);
  const y = (v: number) => pad.t + (1 - (v - geo.lo) / (geo.hi - geo.lo)) * (height - pad.t - pad.b);
  const ticks = [geo.lo, (geo.lo + geo.hi) / 2, geo.hi];
  const ref0 = series[0].points;
  const hp = hover !== null ? ref0[hover] : null;
  return (
    <div ref={ref} style={{ position: 'relative' }}>
      <svg className="chart" width={w} height={height} role="img" aria-label={label}
        onMouseMove={e => {
          const r = e.currentTarget.getBoundingClientRect();
          const t = geo.t0 + ((e.clientX - r.left - pad.l) / (w - pad.l - pad.r)) * (geo.t1 - geo.t0);
          let best = 0;
          ref0.forEach((p, i) => { if (Math.abs(p.t - t) < Math.abs(ref0[best].t - t)) best = i; });
          setHover(best);
        }} onMouseLeave={() => setHover(null)}>
        {ticks.map((v, i) => <g key={i}><line className="grid-line" x1={pad.l} x2={w - pad.r} y1={y(v)} y2={y(v)} /><text x={pad.l - 6} y={y(v) + 3} textAnchor="end">{format(v)}</text></g>)}
        <text x={pad.l} y={height - 5}>{new Date(geo.t0).toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit' })}</text>
        <text x={w - pad.r} y={height - 5} textAnchor="end">{new Date(geo.t1).toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit' })}</text>
        {series.map(s => (
          <path key={s.name} d={s.points.map((p, i) => `${i ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join('')}
            fill="none" stroke={s.color} strokeWidth="1.6" strokeDasharray={s.dashed ? '4 3' : undefined} strokeLinejoin="round" />
        ))}
        {hp && <line x1={x(hp.t)} x2={x(hp.t)} y1={pad.t} y2={height - pad.b} stroke="var(--dim)" strokeDasharray="2 3" />}
      </svg>
      {hp && hover !== null && (
        <div className="tip" style={{ left: Math.min(x(hp.t) + 10, w - 150), top: 4 }}>
          <div className="mut">{new Date(hp.t).toLocaleTimeString([], { hour12: false })}</div>
          {series.map(s => <div key={s.name}><span style={{ color: s.color }}>●</span> {s.name} {s.points[hover] ? format(s.points[hover].v) : '–'}</div>)}
        </div>
      )}
    </div>
  );
}

export function Sparkline({ values, color = 'var(--accent)', w = 90, h = 22 }: { values: number[]; color?: string; w?: number; h?: number }) {
  if (values.length < 2) return <svg width={w} height={h} aria-hidden="true" />;
  const hi = Math.max(...values, 1e-9), lo = Math.min(...values, 0);
  const d = values.map((v, i) => `${i ? 'L' : 'M'}${((i / (values.length - 1)) * w).toFixed(1)},${(h - 2 - ((v - lo) / (hi - lo || 1)) * (h - 4)).toFixed(1)}`).join('');
  return <svg width={w} height={h} aria-hidden="true"><path d={d} fill="none" stroke={color} strokeWidth="1.4" /></svg>;
}

// ---- time range (shared by pages that query telemetry) ----

export const RANGES = ['5m', '15m', '30m', '1h', '3h', '6h', '24h'];
export function useRange(): [string, (r: string) => void] {
  const [r, setR] = useState(() => { try { return sessionStorage.getItem('tf_range') ?? '15m'; } catch { return '15m'; } });
  return [r, v => { setR(v); try { sessionStorage.setItem('tf_range', v); } catch { /* ignore */ } }];
}
export const RangePicker = ({ value, onChange }: { value: string; onChange: (r: string) => void }) => (
  <select aria-label="Time range" value={value} onChange={e => onChange(e.target.value)}>{RANGES.map(r => <option key={r}>{r}</option>)}</select>
);
