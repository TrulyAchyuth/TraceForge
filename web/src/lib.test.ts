import { describe, expect, it } from 'vitest';
import type { Edge, MemoryGraphData, Span } from './api';
import { buildTree, connected, fmtMs, fmtPct, layerServices, layoutGraph, NODE_COLUMNS } from './lib';

const span = (id: string, parent: string, start: number, durMs: number): Span => ({
  trace_id: 't', span_id: id, parent_span_id: parent, service: 's', name: id, kind: 1,
  start_time: new Date(1_700_000_000_000 + start).toISOString(), duration_ns: durMs * 1e6, status_code: 0,
});

describe('formatting', () => {
  it('scales units', () => {
    expect(fmtMs(2500)).toBe('2.50 s');
    expect(fmtMs(12.34)).toBe('12.3 ms');
    expect(fmtMs(undefined)).toBe('–');
    expect(fmtPct(0.2158)).toBe('21.6%');
  });
});

describe('buildTree', () => {
  it('nests children under parents in start order and treats orphans as roots', () => {
    const { rows, totalMs } = buildTree([span('c2', 'a', 30, 5), span('a', '', 0, 50), span('c1', 'a', 10, 5), span('o', 'missing', 20, 5)]);
    expect(rows.map(r => `${r.span.span_id}:${r.depth}`)).toEqual(['a:0', 'c1:1', 'c2:1', 'o:0']);
    expect(totalMs).toBe(50);
  });
  it('handles an empty trace', () => expect(buildTree([]).rows).toEqual([]));
});

describe('layerServices', () => {
  const e = (caller: string, callee: string): Edge => ({ caller, callee, calls: 1, errors: 0, retries: 0, error_rate: 0, retry_ratio: 0, rate_per_sec: 0, p50_ms: 0, p95_ms: 0, change: '' });
  it('puts callees to the right of their callers and places every service', () => {
    const p = layerServices(['gateway', 'checkout', 'payment', 'lonely'], [e('gateway', 'checkout'), e('checkout', 'payment')], ['gateway'], 800, 400);
    const x = (n: string) => p.find(s => s.name === n)!.x;
    expect(p).toHaveLength(4);
    expect(x('gateway')).toBeLessThan(x('checkout'));
    expect(x('checkout')).toBeLessThan(x('payment'));
    expect(new Set(p.map(s => `${s.x},${s.y}`)).size).toBe(4);
  });
});

const graph: MemoryGraphData = {
  nodes: [
    { id: 'inc:A', type: 'incident', label: 'A' }, { id: 'inc:B', type: 'incident', label: 'B', simulated: true },
    { id: 'svc:payment', type: 'service', label: 'payment' }, { id: 'sig:x', type: 'signature', label: 'error_rate', simulated: true },
  ],
  edges: [
    { source: 'inc:A', target: 'svc:payment', kind: 'primary' }, { source: 'inc:B', target: 'svc:payment', kind: 'affected' },
    { source: 'inc:B', target: 'sig:x', kind: 'signature' },
  ],
};
const all = { types: new Set<string>(NODE_COLUMNS), hideSimulated: false, query: '' };

describe('layoutGraph', () => {
  it('is deterministic and uses one column per present type', () => {
    const a = layoutGraph(graph, all), b = layoutGraph(graph, all);
    expect(a).toEqual(b);
    expect(new Set(a.nodes.map(n => n.x)).size).toBe(3);
  });
  it('removes simulated nodes and their edges', () => {
    const v = layoutGraph(graph, { ...all, hideSimulated: true });
    expect(v.nodes.map(n => n.id).sort()).toEqual(['inc:A', 'svc:payment']);
    expect(v.edges).toHaveLength(1);
  });
  it('filters by type and never leaves dangling edges', () => {
    const v = layoutGraph(graph, { ...all, types: new Set(['incident', 'service']) });
    const ids = new Set(v.nodes.map(n => n.id));
    expect(v.edges.every(e => ids.has(e.source) && ids.has(e.target))).toBe(true);
  });
  it('search keeps only incidents that touch a matching node', () => {
    const v = layoutGraph(graph, { ...all, query: 'error_rate' });
    expect(v.nodes.map(n => n.id).sort()).toEqual(['inc:B', 'sig:x']);
  });
  it('handles an empty graph', () => expect(layoutGraph({ nodes: [], edges: [] }, all).nodes).toEqual([]));
});

describe('connected', () => {
  it('returns the neighbourhood of a node', () => {
    const c = connected(graph, 'svc:payment');
    expect([...c.nodes].sort()).toEqual(['inc:A', 'inc:B', 'svc:payment']);
    expect(c.edges).toHaveLength(2);
  });
});
