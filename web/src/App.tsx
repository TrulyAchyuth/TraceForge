import { FormEvent, ReactNode, useEffect, useState } from 'react';
import { getToken, onAuthRequired, setToken, useApi, Overview as OverviewData } from './api';
import { match, useRoute } from './router';
import { Icon } from './ui';
import { Overview } from './pages/Overview';
import { Services, ServiceDetail } from './pages/Services';
import { Dependencies } from './pages/Dependencies';
import { Traces, TraceView } from './pages/Traces';
import { Incidents, IncidentDetail } from './pages/Incidents';
import { ReplayPage } from './pages/Replay';
import { MemoryPage } from './pages/Memory';
import { Lab } from './pages/Lab';
import { TimelinePage } from './pages/Timeline';
import { Reports } from './pages/Reports';
import { Settings } from './pages/Settings';

const NAV: ({ to: string; label: string; icon: string } | 'sep')[] = [
  { to: '/', label: 'Overview', icon: 'overview' },
  { to: '/services', label: 'Services', icon: 'services' },
  { to: '/dependencies', label: 'Dependencies', icon: 'map' },
  { to: '/traces', label: 'Traces', icon: 'traces' },
  'sep',
  { to: '/incidents', label: 'Incidents', icon: 'incidents' },
  { to: '/replay', label: 'Replay', icon: 'replay' },
  { to: '/memory', label: 'Memory graph', icon: 'memory' },
  { to: '/timeline', label: 'Timeline', icon: 'timeline' },
  { to: '/reports', label: 'Reports', icon: 'reports' },
  'sep',
  { to: '/lab', label: 'Failure lab', icon: 'lab' },
  { to: '/settings', label: 'Settings', icon: 'settings' },
];

function crumbs(route: string): { label: string; to?: string }[] {
  const [path] = route.split('?');
  const seg = path.split('/').filter(Boolean);
  if (seg.length === 0) return [{ label: 'Overview' }];
  const root = NAV.find(n => n !== 'sep' && n.to === '/' + seg[0]);
  const out: { label: string; to?: string }[] = [{ label: root && root !== 'sep' ? root.label : seg[0], to: seg.length > 1 ? '/' + seg[0] : undefined }];
  if (seg.length > 1) out.push({ label: seg[1].length > 14 ? seg[1].slice(0, 12) + '…' : seg[1] });
  return out;
}

function TokenGate({ onDone }: { onDone: () => void }) {
  const [t, setT] = useState('');
  const submit = (e: FormEvent) => { e.preventDefault(); setToken(t.trim()); onDone(); };
  return (
    <form className="panel gate" onSubmit={submit}>
      <h1 style={{ marginBottom: 6 }}>TraceForge</h1>
      <p className="mut">This server requires an API token. It is kept in this tab's session storage only and is sent as a bearer token.</p>
      <label className="f">API token<input type="password" autoFocus value={t} onChange={e => setT(e.target.value)} autoComplete="off" /></label>
      <div className="row" style={{ marginTop: 12 }}><button className="btn primary" disabled={!t.trim()}>Continue</button></div>
    </form>
  );
}

function Route({ route }: { route: string }) {
  let m: Record<string, string> | null;
  if (match(route, '/')) return <Overview />;
  if (match(route, '/services')) return <Services />;
  if ((m = match(route, '/services/:name'))) return <ServiceDetail name={m.name} />;
  if (match(route, '/dependencies')) return <Dependencies />;
  if (match(route, '/traces')) return <Traces />;
  if ((m = match(route, '/traces/:id'))) return <TraceView id={m.id} />;
  if (match(route, '/incidents')) return <Incidents />;
  if ((m = match(route, '/incidents/:id'))) return <IncidentDetail id={m.id} route={route} />;
  if (match(route, '/replay')) return <ReplayPage />;
  if (match(route, '/memory')) return <MemoryPage route={route} />;
  if (match(route, '/timeline')) return <TimelinePage />;
  if (match(route, '/reports')) return <Reports />;
  if (match(route, '/lab')) return <Lab />;
  if (match(route, '/settings')) return <Settings />;
  return <div className="empty"><strong>Page not found</strong><a href="#/" className="btn" style={{ marginTop: 8 }}>Go to overview</a></div>;
}

export function App() {
  const route = useRoute();
  const [open, setOpen] = useState(false);
  const [needToken, setNeedToken] = useState(false);
  const [epoch, setEpoch] = useState(0);
  useEffect(() => onAuthRequired(() => setNeedToken(true)), []);
  useEffect(() => setOpen(false), [route]);
  const ov = useApi<OverviewData>(needToken ? null : '/overview', 10_000);

  if (needToken) return <TokenGate onDone={() => { setNeedToken(false); setEpoch(e => e + 1); }} />;
  const active = ov.data?.active_incidents ?? 0;
  const bc = crumbs(route);
  const nav = (n: (typeof NAV)[number], i: number): ReactNode => n === 'sep' ? <div key={i} className="nav-sep" /> : (
    <a key={n.to} href={'#' + n.to} className={`nav ${route === n.to || (n.to !== '/' && route.startsWith(n.to + '/')) || (n.to !== '/' && route.startsWith(n.to + '?')) ? 'on' : ''}`}>
      <Icon name={n.icon} />{n.label}
      {n.to === '/incidents' && active > 0 && <span className="badge critical">{active}</span>}
    </a>
  );
  return (
    <div className="shell">
      <div className={`scrim ${open ? 'open' : ''}`} onClick={() => setOpen(false)} />
      <nav className={`sidebar ${open ? 'open' : ''}`} aria-label="Main">
        <div className="brand"><Icon name="traces" size={18} />TraceForge</div>
        {NAV.map(nav)}
        <div className="foot">v0.2 · local demo</div>
      </nav>
      <div className="main">
        <header className="topbar">
          <button className="btn ghost menu-btn" aria-label="Open navigation" onClick={() => setOpen(o => !o)}><Icon name="menu" /></button>
          <div className="crumbs" aria-label="Breadcrumb">
            {bc.map((c, i) => <span key={i} className="row" style={{ gap: 6 }}>{i > 0 && <Icon name="chevron" size={12} />}{c.to ? <a href={'#' + c.to}>{c.label}</a> : <span className="cur">{c.label}</span>}</span>)}
          </div>
          <div className="spacer" />
          {ov.data && <span className="row mut" title="Spans received per second, last minute"><span className={`dot ${ov.data.ingest.spans_per_second_1m > 0 ? 'healthy pulse' : ''}`} />{ov.data.ingest.spans_per_second_1m.toFixed(1)} spans/s</span>}
          {getToken() && <button className="btn sm ghost" onClick={() => { setToken(''); setNeedToken(true); }}>Sign out</button>}
        </header>
        <main className="content" key={epoch}>
          <Route route={route} />
        </main>
      </div>
    </div>
  );
}

