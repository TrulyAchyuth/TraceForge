import { useState } from 'react';
import { api, Experiment, LabScenario, useApi } from '../api';
import { ago } from '../lib';
import { Async, Badge, Empty, ErrorBox, PageHead } from '../ui';

interface LabState { enabled: boolean; scenarios: LabScenario[]; experiments: Experiment[]; targets: { name: string; reachable: boolean; active: boolean; error?: string }[]; min_duration_seconds: number; max_duration_seconds: number }

export function Lab() {
  const st = useApi<LabState>('/lab', 5_000);
  const [scenario, setScenario] = useState(''); const [dur, setDur] = useState(90); const [seed, setSeed] = useState(1);
  const [err, setErr] = useState(''); const [busy, setBusy] = useState(false);
  const act = (p: Promise<unknown>) => { setBusy(true); setErr(''); p.then(st.reload).catch(e => setErr(e.message)).finally(() => setBusy(false)); };
  return (
    <>
      <PageHead title="Failure lab" sub="Inject a real fault into the sample application; TraceForge must detect it from telemetry" />
      {err && <ErrorBox message={err} />}
      <Async state={st}>{d => {
        const running = d.experiments.find(e => e.status === 'running');
        const sc = d.scenarios.find(s => s.id === (scenario || d.scenarios[0]?.id));
        return !d.enabled ? <Empty title="Lab is disabled">Set TF_LAB_TARGETS to enable it.</Empty> : (
          <>
            {running && <div className="notice info row">Running: <b>{running.name}</b> on {running.targets.join(', ')}<div className="spacer" /><button className="btn sm" disabled={busy} onClick={() => act(api(`/lab/experiments/${running.id}/stop`, { method: 'POST' }))}>Stop</button></div>}
            <div className="panel pad section">
              <label className="f">Scenario<select value={sc?.id} onChange={e => { setScenario(e.target.value); const s = d.scenarios.find(x => x.id === e.target.value); if (s) setDur(s.default_duration_seconds); }}>{d.scenarios.filter(s => !s.custom).map(s => <option key={s.id} value={s.id}>{s.name}</option>)}</select></label>
              {sc && <p className="mut">{sc.description} <br />Expected: {sc.expected}</p>}
              <div className="row" style={{ alignItems: 'end' }}>
                <label className="f">Duration (s)<input type="number" min={d.min_duration_seconds} max={d.max_duration_seconds} value={dur} onChange={e => setDur(+e.target.value)} /></label>
                <label className="f">Seed<input type="number" value={seed} onChange={e => setSeed(+e.target.value)} /></label>
                <button className="btn primary" disabled={busy || !!running || !sc} onClick={() => act(api('/lab/experiments', { method: 'POST', body: { scenario: sc!.id, duration_seconds: dur, seed } }))}>Start experiment</button>
                <button className="btn" disabled={busy} onClick={() => act(api('/lab/reset', { method: 'POST' }))}>Reset all faults</button>
              </div>
            </div>
            <h2>Targets</h2><div className="panel">{d.targets.map(t => <div key={t.name} className="item"><div className="grow">{t.name}</div><Badge kind={t.reachable ? 'ok' : 'bad'}>{t.reachable ? 'reachable' : 'unreachable'}</Badge>{t.active && <Badge kind="warn">fault active</Badge>}</div>)}</div>
            <h2 style={{ marginTop: 20 }}>History</h2><div className="panel">{d.experiments.length === 0 ? <Empty title="No experiments yet" /> : d.experiments.map(e => <div key={e.id} className="item"><div className="grow">{e.name}<div className="mut">seed {e.seed} · {ago(e.started_at)}</div></div><Badge kind={e.status}>{e.status}</Badge></div>)}</div>
          </>);
      }}</Async>
    </>
  );
}
