import { useEffect, useState } from 'react';

/** Hash routing keeps the Go file server trivial: every URL is "/" plus a fragment. */
export function useRoute(): string {
  const [h, setH] = useState(() => location.hash.slice(1) || '/');
  useEffect(() => {
    const f = () => { setH(location.hash.slice(1) || '/'); window.scrollTo(0, 0); };
    addEventListener('hashchange', f);
    return () => removeEventListener('hashchange', f);
  }, []);
  return h;
}
export const go = (path: string) => { location.hash = path; };

export function match(route: string, pattern: string): Record<string, string> | null {
  const [path] = route.split('?');
  const a = path.split('/').filter(Boolean), b = pattern.split('/').filter(Boolean);
  if (a.length !== b.length) return null;
  const out: Record<string, string> = {};
  for (let i = 0; i < b.length; i++) {
    if (b[i].startsWith(':')) out[b[i].slice(1)] = decodeURIComponent(a[i]);
    else if (a[i] !== b[i]) return null;
  }
  return out;
}

export function query(route: string): URLSearchParams {
  return new URLSearchParams(route.split('?')[1] ?? '');
}
