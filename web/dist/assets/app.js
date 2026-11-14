'use strict';
const $app = document.getElementById('app');
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const ms = v => v == null ? '–' : (v >= 1000 ? (v / 1000).toFixed(2) + ' s' : v.toFixed(v < 10 ? 1 : 0) + ' ms');
const pct = v => (v * 100).toFixed(1) + '%';
const when = t => t ? new Date(t).toLocaleTimeString() : '–';
const tag = s => `<span class="tag ${esc(s)}">${esc(s)}</span>`;

function token() { try { return sessionStorage.getItem('tf_token') || ''; } catch { return ''; } }
async function api(path, opt = {}) {
  const h = { 'Content-Type': 'application/json' };
  if (token()) h.Authorization = 'Bearer ' + token();
  const r = await fetch('/api/v1' + path, { ...opt, headers: h });
  if (r.status === 401) {
    const t = prompt('API token required:');
    if (t) { try { sessionStorage.setItem('tf_token', t); } catch {} return api(path, opt); }
  }
  const ct = r.headers.get('content-type') || '';
  const body = ct.includes('json') ? await r.json() : await r.text();
  if (!r.ok) throw new Error(body?.error?.message || r.statusText);
  return body;
}
const post = (p, b, m = 'POST') => api(p, { method: m, body: JSON.stringify(b || {}) });
const table = (head, rows) => `<table><thead><tr>${head.map(h => `<th>${h}</th>`).join('')}</tr></thead><tbody>${rows.map(r => `<tr>${r.map(c => `<td>${c}</td>`).join('')}</tr>`).join('') || `<tr><td class="mut" colspan="${head.length}">Nothing yet.</td></tr>`}</tbody></table>`;
const incRows = list => list.map(i => [`<a href="#/incidents/${i.id}">${esc(i.key)}</a>`, esc(i.title), tag(i.severity), tag(i.status), when(i.started_at)]);
const incHead = ['ID', 'Title', 'Severity', 'Status', 'Started'];

const views = {
  async '/'() {
    const o = await api('/overview');
    const hc = o.health_counts || {};
    return `<h1>Overview</h1><p>${Object.entries(hc).map(([k, v]) => `${tag(k)} ${v}`).join(' ') || 'No services reporting.'}
      <span class="mut"> · ${o.active_incidents} active incidents · ${(o.ingest?.spans_per_second_1m ?? 0).toFixed(1)} spans/s</span></p>
      <h2>Recent incidents</h2>${table(incHead, incRows(o.recent_incidents || []))}`;
  },
  async '/services'() {
    const d = await api('/services');
    return `<h1>Services</h1>` + table(['Service', 'Status', 'Req/s', 'Error rate', 'p50', 'p95', 'p99'],
      d.services.map(s => [esc(s.name), tag(s.status), s.rate_per_sec, pct(s.error_rate), ms(s.p50_ms), ms(s.p95_ms), ms(s.p99_ms)]));
  },
  async '/dependencies'() {
    const d = await api('/dependencies');
    return `<h1>Dependencies</h1>` + table(['Caller', 'Callee', 'Calls', 'Error rate', 'Retry ratio', 'p95', 'Change'],
      d.edges.map(e => [esc(e.caller), esc(e.callee), e.calls, pct(e.error_rate), pct(e.retry_ratio), ms(e.p95_ms), esc(e.change)]));
  },
  async '/incidents'() {
    const d = await api('/incidents');
    return `<h1>Incidents</h1>` + table(incHead, incRows(d.incidents));
  },
  async '/traces'() {
    const d = await api('/traces?limit=50');
    return `<h1>Traces</h1>` + table(['Trace', 'Root', 'Duration', 'Spans', 'Errors', 'Start'],
      d.traces.map(t => [`<a href="#/traces/${esc(t.trace_id)}">${esc(t.trace_id.slice(0, 12))}</a>`, esc(t.root_service + ' · ' + t.root_name), ms(t.duration_ns / 1e6), t.span_count, t.error_count, when(t.start_time)]));
  },
  async '/lab'() {
    const d = await api('/lab');
    const run = d.experiments || [];
    return `<h1>Failure lab</h1>${d.enabled ? '' : '<p class="mut">No lab targets configured (TF_LAB_TARGETS).</p>'}
      ${d.scenarios.map(s => `<div class="card"><strong>${esc(s.name)}</strong><div class="mut">${esc(s.description)}</div><div>Expected: ${esc(s.expected)}</div>
        ${s.custom ? '' : `<button data-start="${esc(s.id)}" data-dur="${s.default_duration_seconds}">Run ${s.default_duration_seconds}s</button>`}</div>`).join('')}
      <h2>Experiments</h2>` + table(['ID', 'Scenario', 'Status', 'Started', ''],
      run.map(e => [e.id, esc(e.name), tag(e.status), when(e.started_at), e.status === 'running' ? `<button data-stop="${e.id}">Stop</button>` : '']))
      + `<p><button data-reset>Reset lab</button></p>`;
  },
};

async function incident(id) {
  const d = await api(`/incidents/${id}/investigation`);
  const i = d.incident;
  const ev = h => h.evidence.map(e => `<li>${tag(e.polarity)} ${esc(e.text)} <span class="mut">(${esc(e.class)}, ${e.points >= 0 ? '+' : ''}${e.points})</span></li>`).join('');
  return `<h1>${esc(i.key)} ${tag(i.severity)} ${tag(i.status)}</h1><p>${esc(i.title)}</p>
    <p class="mut">Affected: ${esc((i.affected_services || []).join(', '))} · started ${when(i.started_at)} ·
    Report: <a href="/api/v1/incidents/${i.id}/report?format=md" target="_blank">md</a> · <a href="/api/v1/incidents/${i.id}/report?format=html" target="_blank">html</a> · <a href="/api/v1/incidents/${i.id}/report?format=json" target="_blank">json</a>
    ${i.status !== 'resolved' ? ` · <button data-resolve="${i.id}">Mark resolved</button>` : ''}</p>
    <h2>Signals</h2>${(i.signals || []).map(s => `<div class="card">${tag(s.severity)} ${esc(s.message)}</div>`).join('')}
    <h2>Hypotheses</h2>${(d.hypotheses || []).map(h => `<div class="card"><strong>#${h.rank} ${esc(h.title)}</strong> <span class="mut">evidence ${h.score}/${h.max_score}</span>
      <div>${esc(h.statement)}</div><ul>${ev(h)}</ul></div>`).join('') || '<p class="mut">None yet.</p>'}
    ${(d.known_faults || []).length ? `<h2>Lab faults active in window</h2>` + d.known_faults.map(f => `<div class="card">${esc(f.name)} → ${esc(f.targets.join(', '))}</div>`).join('') : ''}
    <h2>Replay</h2><p class="mut">Re-runs captured traces with a change applied; it estimates, it does not prove.</p>
    ${(d.replay_options?.services || []).map(s => `<button data-replay="${esc(s)}" data-inc="${i.id}">Restore ${esc(s)} latency to baseline</button> `).join('')}
    ${table(['Replay', 'Name', 'Status'], (d.replays || []).map(r => [r.id, esc(r.name || r.scenario?.name || ''), esc(r.status || '')]))}
    <p><button data-rebuild="${i.id}">Rebuild investigation</button></p>
    <h2>Related traces</h2>${table(['Trace', 'Operation', 'Duration', 'Why'], (d.related_traces || []).map(t => [`<a href="#/traces/${esc(t.trace_id)}">${esc(t.trace_id.slice(0, 12))}</a>`, esc(t.operation), ms(t.duration_ms), esc(t.reason)]))}
    <h2>Timeline</h2>${table(['Time', 'Kind', 'Event'], (d.timeline || []).map(t => [when(t.time), esc(t.kind), esc(t.title)]))}
    <h2>Method</h2><ul class="mut">${(d.methodology || []).map(m => `<li>${esc(m)}</li>`).join('')}</ul>`;
}

async function trace(id) {
  const d = await api(`/traces/${id}`);
  const spans = (d.spans || []).slice().sort((a, b) => a.start_time < b.start_time ? -1 : 1);
  const t0 = spans.length ? new Date(spans[0].start_time).getTime() : 0;
  const total = Math.max(...spans.map(s => new Date(s.start_time).getTime() - t0 + s.duration_ns / 1e6), 1);
  return `<h1>Trace ${esc(id.slice(0, 12))}</h1>` + table(['Service', 'Span', 'Duration', 'Timeline'], spans.map(s => {
    const off = (new Date(s.start_time).getTime() - t0) / total * 100, w = Math.max(s.duration_ns / 1e6 / total * 100, .5);
    return [esc(s.service) + (s.status_code === 2 ? ' ' + tag('critical') : ''), esc(s.name), ms(s.duration_ns / 1e6), `<div class="bar" style="margin-left:${off}%;width:${w}%"></div>`];
  }));
}

async function render() {
  const path = location.hash.slice(1) || '/';
  document.querySelectorAll('nav a').forEach(a => a.classList.toggle('on', a.getAttribute('href') === '#' + path));
  try {
    let m;
    if ((m = path.match(/^\/incidents\/(\d+)$/))) $app.innerHTML = await incident(m[1]);
    else if ((m = path.match(/^\/traces\/([0-9a-f]+)$/))) $app.innerHTML = await trace(m[1]);
    else if (views[path]) $app.innerHTML = await views[path]();
    else $app.innerHTML = '<h1>Not found</h1>';
  } catch (e) { $app.innerHTML = `<h1>Error</h1><p class="critical">${esc(e.message)}</p>`; }
}

$app.addEventListener('click', async e => {
  const d = e.target.dataset;
  try {
    if (d.start) await post('/lab/experiments', { scenario: d.start, duration_seconds: +d.dur, seed: 1 });
    else if (d.stop) await post(`/lab/experiments/${d.stop}/stop`);
    else if ('reset' in d) await post('/lab/reset');
    else if (d.resolve) await post(`/incidents/${d.resolve}`, { status: 'resolved' }, 'PATCH');
    else if (d.rebuild) await post(`/incidents/${d.rebuild}/investigation`);
    else if (d.replay) await post(`/incidents/${d.inc}/replays`, { name: `Restore ${d.replay} to baseline`, seed: 1, latency_objective_ms: 0,
      interventions: [{ type: 'scale_latency', service: d.replay, mode: 'baseline' }] });
    else return;
    render();
  } catch (err) { alert(err.message); }
});
addEventListener('hashchange', render);
render();
setInterval(() => { if (!document.hidden && !location.hash.startsWith('#/lab') && !/^#\/incidents\/\d+/.test(location.hash)) render(); }, 10000);
