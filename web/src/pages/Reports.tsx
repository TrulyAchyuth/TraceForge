import { useState } from 'react';
import { download, Incident, useApi } from '../api';
import { fmtDateTime } from '../lib';
import { Async, Empty, ErrorBox, PageHead } from '../ui';

export function Reports() {
  const st = useApi<{ incidents: Incident[] }>('/incidents?limit=100');
  const [err, setErr] = useState('');
  return (
    <>
      <PageHead title="Reports" sub="Every incident has a Markdown, HTML and JSON report with evidence, replays and similar past incidents" />
      {err && <ErrorBox message={err} />}
      <Async state={st}>{d => d.incidents.length === 0 ? <Empty title="No incidents to report on" /> : <div className="panel">{d.incidents.map(i => (
        <div key={i.id} className="item"><div className="grow"><a href={`#/incidents/${i.id}`}>{i.title}</a><div className="mut">INC-{String(i.id).padStart(4, '0')} · {fmtDateTime(i.started_at)}</div></div>
          {['md', 'html', 'json'].map(f => <button key={f} className="btn sm" onClick={() => download(`/incidents/${i.id}/report?format=${f}`, `incident-${i.id}.${f}`).catch(e => setErr(e.message))}>.{f}</button>)}</div>))}</div>}</Async>
    </>
  );
}
