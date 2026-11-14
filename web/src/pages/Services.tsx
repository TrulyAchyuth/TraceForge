import { useMemo, useState } from 'react';
import { Edge, Incident, Point, ServiceRow, useApi } from '../api';
import { ago, fmtMs, fmtPct } from '../lib';
import { Async, Badge, Empty, LineChart, PageHead, RangePicker, Sparkline, StatusDot, useRange } from '../ui';

export function Services() {
  const [range, setRange] = useRange();
  const [q, setQ] = useState('');
  const [status, setStatus] = useState('');
  const st = useApi<{ services: ServiceRow[] }>(`/services?range=${range}`, 10_000);
  return (
    <>
      <PageHead title="Services" sub="Every service seen in traces"><RangePicker value={range} onChange={setRange} /></PageHead>
      <div className="row section">
        <input type="search" placeholder="Filter by name" aria-label="Filter services by name" value={q} onChange={e => setQ(e.target.value)} />
        <select aria-label="Filter by status" value={status} onChange={e => setStatus(e.target.value)}>
          <option value="">All statuses</option><option>healthy</option><option>degraded</option><option>critical</option>
        </select>
      </div>
      <Async state={st}>{d => {
        const rows = d.services.filter(s => s.name.includes(q.trim().toLowerCase()) && (!status || s.status === status));
        return <div className="panel table-wrap"><table>
          <thead><tr><th>Service</th><th>Status</th><th className="r">Req/s</th><th className="r">Error rate</th><th className="r">p50</th><th className="r">p95</th><th className="r">p99</th><th>Trend</th></tr></thead>
          <tbody>{rows.length === 0 ? <tr><td colSpan={8}><Empty title="No matching services">Adjust the filters.</Empty></td></tr> : rows.map(s => (
            <tr key={s.name} className="click" onClick={() => { location.hash = `/services/${s.name}`; }}>
              <td><a href={`#/services/${s.name}`}>{s.name}</a></td><td><span className="row"><StatusDot status={s.status} />{s.status}</span></td>
              <td className="r num">{s.rate_per_sec.toFixed(2)}</td><td className="r num">{fmtPct(s.error_rate)}</td>
              <td className="r num">{fmtMs(s.p50_ms)}</td><td className="r num">{fmtMs(s.p95_ms)}</td><td className="r num">{fmtMs(s.p99_ms)}</td>
              <td><Sparkline values={(s.series ?? []).map(p => p.p95_ms)} color="var(--warn)" /></td></tr>))}</tbody></table></div>;
      }}</Async>
    </>
  );
}

interface Detail { service: ServiceRow & { series: Point[] }; incoming: Edge[]; outgoing: Edge[]; incidents: Incident[] }

export function ServiceDetail({ name }: { name: string }) {
  const [range, setRange] = useRange();
  const st = useApi<Detail>(`/services/${encodeURIComponent(name)}?range=${range}`, 10_000);
  const t = (p: Point) => new Date(p.t).getTime();
  const edges = (list: Edge[], other: 'caller' | 'callee') => list.length === 0 ? <Empty title="None">No calls in this range.</Empty> : (
    <table><thead><tr><th>{other === 'caller' ? 'Caller' : 'Callee'}</th><th className="r">Calls</th><th className="r">Errors</th><th className="r">Retries</th><th className="r">p95</th></tr></thead>
      <tbody>{list.map(e => <tr key={e.caller + e.callee}><td><a href={`#/services/${e[other]}`}>{e[other]}</a></td><td className="r num">{e.calls}</td><td className="r num">{fmtPct(e.error_rate)}</td><td className="r num">{fmtPct(e.retry_ratio)}</td><td className="r num">{fmtMs(e.p95_ms)}</td></tr>)}</tbody></table>
  );
  const body = useMemo(() => st.data, [st.data]);
  return (
    <>
      <PageHead title={name} sub={body ? <span className="row"><StatusDot status={body.service.status} />{body.service.status}{body.service.last_seen && <> · last seen {ago(body.service.last_seen)}</>}</span> : undefined}>
        <RangePicker value={range} onChange={setRange} />
      </PageHead>
      <Async state={st}>{d => (
        <>
          <div className="grid g3 section">
            <div className="panel pad"><h2>Request rate</h2><LineChart label="Request rate" format={v => `${v.toFixed(1)}/s`} series={[{ name: 'requests/s', color: 'var(--accent)', points: d.service.series.map(p => ({ t: t(p), v: p.rate_per_sec })) }]} /></div>
            <div className="panel pad"><h2>Error rate</h2><LineChart label="Error rate" format={v => fmtPct(v, 0)} series={[{ name: 'errors', color: 'var(--bad)', points: d.service.series.map(p => ({ t: t(p), v: p.error_rate })) }]} /></div>
            <div className="panel pad"><h2>Latency</h2><LineChart label="Latency" format={fmtMs} series={[
              { name: 'p95', color: 'var(--warn)', points: d.service.series.map(p => ({ t: t(p), v: p.p95_ms })) },
              { name: 'p50', color: 'var(--info)', points: d.service.series.map(p => ({ t: t(p), v: p.p50_ms })) }]} /></div>
          </div>
          <div className="grid g2 section">
            <section><h2>Called by</h2><div className="panel table-wrap">{edges(d.incoming, 'caller')}</div></section>
            <section><h2>Calls</h2><div className="panel table-wrap">{edges(d.outgoing, 'callee')}</div></section>
          </div>
          <section><h2>Incidents involving {name}</h2><div className="panel">{d.incidents.length === 0 ? <Empty title="None in this range" /> : d.incidents.map(i => (
            <a key={i.id} className="item" href={`#/incidents/${i.id}`}><Badge kind={i.severity}>{i.severity}</Badge><div className="grow title">{i.title}</div><Badge kind={i.status}>{i.status}</Badge></a>))}</div></section>
        </>
      )}</Async>
    </>
  );
}
