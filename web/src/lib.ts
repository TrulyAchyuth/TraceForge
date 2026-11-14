import type { GraphEdge, GraphNode, MemoryGraphData, Span, Edge } from './api';

export const fmtMs = (v: number | undefined): string =>
  v === undefined || Number.isNaN(v) ? '–' : v >= 1000 ? `${(v / 1000).toFixed(2)} s` : v >= 100 ? `${v.toFixed(0)} ms` : v >= 10 ? `${v.toFixed(1)} ms` : `${v.toFixed(2)} ms`;
export const fmtPct = (v: number | undefined, digits = 1): string => (v === undefined ? '–' : `${(v * 100).toFixed(digits)}%`);
export const fmtNum = (v: number): string => (v >= 1e6 ? `${(v / 1e6).toFixed(1)}M` : v >= 1e4 ? `${(v / 1e3).toFixed(1)}k` : String(Math.round(v * 10) / 10));
export const fmtTime = (t?: string): string => (t ? new Date(t).toLocaleTimeString([], { hour12: false }) : '–');
export const fmtDateTime = (t?: string): string => (t ? new Date(t).toLocaleString([], { hour12: false }) : '–');
export function fmtDuration(sec: number): string {
  if (sec < 90) return `${Math.round(sec)}s`;
  if (sec < 5400) return `${Math.round(sec / 60)}m`;
  return `${(sec / 3600).toFixed(1)}h`;
}
export function ago(t: string | undefined, now = Date.now()): string {
  if (!t) return '–';
  const s = Math.max(0, (now - new Date(t).getTime()) / 1000);
  return s < 5 ? 'just now' : `${fmtDuration(s)} ago`;
}

// ---- trace tree ----

export interface TreeRow { span: Span; depth: number; offsetMs: number; durationMs: number }

/** Orders spans depth-first (children by start time). Spans whose parent is missing become roots. */
export function buildTree(spans: Span[]): { rows: TreeRow[]; totalMs: number } {
  if (spans.length === 0) return { rows: [], totalMs: 0 };
  const ids = new Set(spans.map(s => s.span_id));
  const kids = new Map<string, Span[]>();
  const roots: Span[] = [];
  for (const s of spans) {
    if (s.parent_span_id && ids.has(s.parent_span_id)) {
      const l = kids.get(s.parent_span_id) ?? [];
      l.push(s); kids.set(s.parent_span_id, l);
    } else roots.push(s);
  }
  const byStart = (a: Span, b: Span) => new Date(a.start_time).getTime() - new Date(b.start_time).getTime();
  const t0 = Math.min(...spans.map(s => new Date(s.start_time).getTime()));
  const rows: TreeRow[] = [];
  let end = 0;
  const walk = (s: Span, depth: number) => {
    const offsetMs = new Date(s.start_time).getTime() - t0;
    const durationMs = s.duration_ns / 1e6;
    end = Math.max(end, offsetMs + durationMs);
    rows.push({ span: s, depth, offsetMs, durationMs });
    (kids.get(s.span_id) ?? []).sort(byStart).forEach(c => walk(c, depth + 1));
  };
  roots.sort(byStart).forEach(r => walk(r, 0));
  return { rows, totalMs: Math.max(end, 0.001) };
}

// ---- dependency map layout ----

export interface PlacedService { name: string; x: number; y: number; layer: number }

/** Layers services left to right by their shortest call distance from the entry services. */
export function layerServices(names: string[], edges: Edge[], entry: string[], width: number, height: number): PlacedService[] {
  const layer = new Map<string, number>();
  const queue = entry.filter(n => names.includes(n));
  queue.forEach(n => layer.set(n, 0));
  for (let i = 0; i < queue.length; i++) {
    for (const e of edges) {
      if (e.caller === queue[i] && !layer.has(e.callee)) { layer.set(e.callee, layer.get(queue[i])! + 1); queue.push(e.callee); }
    }
  }
  const unreached = Math.max(-1, ...layer.values()) + 1; // services nothing calls share one last column
  for (const n of names) if (!layer.has(n)) layer.set(n, unreached);
  const cols = new Map<number, string[]>();
  for (const n of [...names].sort()) { const l = layer.get(n)!; cols.set(l, [...(cols.get(l) ?? []), n]); }
  const nLayers = Math.max(...cols.keys(), 0) + 1;
  const out: PlacedService[] = [];
  for (const [l, list] of cols) {
    list.forEach((name, i) => out.push({ name, layer: l, x: nLayers === 1 ? width / 2 : 70 + (l * (width - 140)) / (nLayers - 1), y: ((i + 1) * height) / (list.length + 1) }));
  }
  return out;
}

// ---- incident memory graph layout ----

export const NODE_COLUMNS = ['signature', 'path', 'incident', 'service', 'intervention', 'outcome'] as const;
export const NODE_LABEL: Record<string, string> = { incident: 'Incidents', service: 'Services', signature: 'Failure signatures', path: 'Dependency paths', intervention: 'Interventions', outcome: 'Outcomes' };

export interface PlacedNode extends GraphNode { x: number; y: number }
export interface GraphView { nodes: PlacedNode[]; edges: GraphEdge[]; width: number; height: number }

export interface GraphFilter { types: Set<string>; hideSimulated: boolean; query: string }

/**
 * Places nodes in one column per type, ordered by how many incidents they connect (then label) so that
 * the layout is stable between renders. Filtering removes nodes and every edge touching them.
 */
export function layoutGraph(g: MemoryGraphData, f: GraphFilter, width = 1180, rowH = 38): GraphView {
  const q = f.query.trim().toLowerCase();
  const keep = new Set(g.nodes.filter(n => f.types.has(n.type) && !(f.hideSimulated && n.simulated) && (!q || n.type === 'incident' || n.label.toLowerCase().includes(q))).map(n => n.id));
  let edges = g.edges.filter(e => keep.has(e.source) && keep.has(e.target));
  if (q) { // keep only incidents that still touch a matching node
    const touched = new Set(edges.map(e => e.source));
    edges = edges.filter(e => touched.has(e.source));
    const used = new Set(edges.flatMap(e => [e.source, e.target]));
    for (const id of [...keep]) if (!used.has(id)) keep.delete(id);
  }
  const degree = new Map<string, number>();
  edges.forEach(e => { degree.set(e.source, (degree.get(e.source) ?? 0) + 1); degree.set(e.target, (degree.get(e.target) ?? 0) + 1); });
  const cols = NODE_COLUMNS.filter(c => g.nodes.some(n => keep.has(n.id) && n.type === c));
  const nodes: PlacedNode[] = [];
  let maxRows = 1;
  cols.forEach((c, ci) => {
    const list = g.nodes.filter(n => keep.has(n.id) && n.type === c)
      .sort((a, b) => (degree.get(b.id) ?? 0) - (degree.get(a.id) ?? 0) || a.label.localeCompare(b.label));
    maxRows = Math.max(maxRows, list.length);
    list.forEach((n, i) => nodes.push({ ...n, x: cols.length === 1 ? width / 2 : 100 + (ci * (width - 200)) / (cols.length - 1), y: 44 + i * rowH }));
  });
  return { nodes, edges, width, height: 44 + maxRows * rowH + 20 };
}

export function connected(g: GraphView | MemoryGraphData, id: string): { nodes: Set<string>; edges: GraphEdge[] } {
  const edges = g.edges.filter(e => e.source === id || e.target === id);
  const nodes = new Set<string>([id]);
  edges.forEach(e => { nodes.add(e.source); nodes.add(e.target); });
  return { nodes, edges };
}
