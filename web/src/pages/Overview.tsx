import { Overview as OverviewData, useApi, ServiceRow } from '../api';
import { ago, fmtMs, fmtNum, fmtPct } from '../lib';
import { Async, Badge, Empty, LineChart, PageHead, RangePicker, Sparkline, StatusDot, useRange } from '../ui';

const ts = (p: { t: string }) => new Date(p.t).getTime();

export function Overview() {
  const [range, setRange] = useRange();
  const st = useApi<OverviewData>(`/overview?range=${range}`, 10_000);
  return (
    <>
      <PageHead title="Overview" sub="Live health of everything sending traces"><RangePicker value={range} onChange={setRange} /></PageHead>
      <Async state={st}>{d => {
        const active = d.recent_incidents.filter(i => i.status !== 'resolved' && i.status !== 'closed');
        if (d.services.length === 0) return (
          <div className="panel"><Empty title="No telemetry yet">Send OTLP/HTTP traces to <span className="mono">:4318/v1/traces</span>, or start the bundled sample application.</Empty></div>
        );
        return (
          <>
            <div className="panel pad section">
              <div className="row" style={{ gap: 36 }}>
                <div className="stat"><span className="v">{d.totals.rate_per_sec.toFixed(1)}</span><span className="l">requests / s</span></div>
                <div className="stat"><span className="v">{fmtPct(d.totals.error_rate)}</span><span className="l">error rate</span></div>
                <div className="stat"><span className="v">{fmtMs(d.totals.p95_ms)}</span><span className="l">p95 latency</span></div>
                <div className="stat"><span className="v">{fmtNum(d.totals.requests)}</span><span className="l">entry requests</span></div>
                <div className="stat"><span className="v" style={{ color: active.length ? 'var(--bad)' : 'var(--ok)' }}>{d.active_incidents}</span><span className="l">active incidents</span></div>
              </div>
            </div>
            <div className="grid g3 section">
              <div className="panel pad"><h2>Request rate</h2><LineChart label="Request rate" format={v => `${v.toFixed(1)}/s`} series={[{ name: 'requests/s', color: 'var(--accent)', points: d.series.map(p => ({ t: ts(p), v: p.rate_per_sec })) }]} /></div>
              <div className="panel pad"><h2>Error rate</h2><LineChart label="Error rate" format={v => fmtPct(v, 0)} series={[{ name: 'errors', color: 'var(--bad)', points: d.series.map(p => ({ t: ts(p), v: p.error_rate })) }]} /></div>
              <div className="panel pad"><h2>Latency</h2><LineChart label="Latency" format={fmtMs} series={[
                { name: 'p95', color: 'var(--warn)', points: d.series.map(p => ({ t: ts(p), v: p.p95_ms })) },
                { name: 'p50', color: 'var(--info)', points: d.series.map(p => ({ t: ts(p), v: p.p50_ms })) }]} /></div>
            </div>
            <div className="grid g2">
              <section><h2>Services</h2>
                <div className="panel table-wrap"><table>
                  <thead><tr><th>Service</th><th className="r">Req/s</th><th className="r">Errors</th><th className="r">p95</th><th>Trend</th></tr></thead>
                  <tbody>{d.services.map((s: ServiceRow) => (
                    <tr key={s.name} className="click" onClick={() => { location.hash = `/services/${s.name}`; }}>
                      <td><span className="row"><StatusDot status={s.status} /><a href={`#/services/${s.name}`}>{s.name}</a></span></td>
                      <td className="r num">{s.rate_per_sec.toFixed(1)}</td><td className="r num">{fmtPct(s.error_rate)}</td><td className="r num">{fmtMs(s.p95_ms)}</td>
                      <td><Sparkline values={(s.series ?? []).map(p => p.rate_per_sec)} /></td></tr>
                  ))}</tbody></table></div></section>
              <section><h2>Recent incidents</h2>
                <div className="panel">{d.recent_incidents.length === 0 ? <Empty title="No incidents">Nothing has been detected in this range.</Empty> :
                  d.recent_incidents.slice(0, 6).map(i => (
                    <a key={i.id} className="item" href={`#/incidents/${i.id}`}>
                      <Badge kind={i.severity}>{i.severity}</Badge><div className="grow"><div className="title">{i.title}</div><div className="mut" style={{ fontSize: 12 }}>{i.key} · {ago(i.started_at)}</div></div><Badge kind={i.status}>{i.status}</Badge>
                    </a>))}</div></section>
            </div>
          </>
        );
      }}</Async>
    </>
  );
}
