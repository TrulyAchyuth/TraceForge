import { useState } from 'react';
import { TimelineEvent, useApi } from '../api';
import { fmtDateTime } from '../lib';
import { Async, Badge, Empty, PageHead } from '../ui';

export function TimelinePage() {
  const [service, setService] = useState('');
  const st = useApi<{ events: TimelineEvent[] }>(`/timeline?limit=200${service ? `&service=${encodeURIComponent(service)}` : ''}`, 10_000);
  return (
    <>
      <PageHead title="Timeline" sub="Signals, incidents, experiments and replays in order"><input type="search" placeholder="Filter by service" aria-label="Filter by service" value={service} onChange={e => setService(e.target.value.trim())} /></PageHead>
      <Async state={st}>{d => d.events.length === 0 ? <Empty title="No events" /> : <div className="panel">{d.events.map(e => (
        <div key={e.id} className="item"><span className="mut num" style={{ width: 150 }}>{fmtDateTime(e.time)}</span><Badge kind={e.severity}>{e.kind}</Badge>
          <div className="grow">{e.title}{e.service && <span className="mut"> · {e.service}</span>}</div>
          {e.incident_id && <a href={`#/incidents/${e.incident_id}`}>INC-{String(e.incident_id).padStart(4, '0')}</a>}</div>))}</div>}</Async>
    </>
  );
}
