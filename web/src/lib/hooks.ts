import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError } from "./api";

export interface Loaded<T> { data: T | undefined; error: ApiError | undefined; loading: boolean; refresh: () => void; updatedAt: number }

// useApi fetches path and refreshes it on demand or every intervalMs.
export function useApi<T>(path: string | null, intervalMs = 0): Loaded<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<ApiError>();
  const [loading, setLoading] = useState(!!path);
  const [updatedAt, setUpdatedAt] = useState(0);
  const [tick, setTick] = useState(0);
  const refresh = useCallback(() => setTick((t) => t + 1), []);
  useEffect(() => {
    if (!path) return;
    let alive = true;
    api.get<T>(path).then(
      (d) => { if (alive) { setData(d); setError(undefined); setLoading(false); setUpdatedAt(Date.now()); } },
      (e) => { if (alive) { setError(e instanceof ApiError ? e : new ApiError(0, "error", String(e))); setLoading(false); } },
    );
    return () => { alive = false; };
  }, [path, tick]);
  useEffect(() => {
    if (!intervalMs || !path) return;
    const id = setInterval(refresh, intervalMs);
    return () => clearInterval(id);
  }, [intervalMs, path, refresh]);
  return { data, error, loading, refresh, updatedAt };
}

export interface StreamEvent { seq: number; topic: string; type: string; migration?: string; data: any; ts: number }

// useStream subscribes to an SSE endpoint and calls onEvent for each event.
export function useStream(path: string | null, onEvent: (e: StreamEvent) => void): { connected: boolean; lastEventAt: number } {
  const [connected, setConnected] = useState(false);
  const [lastEventAt, setLast] = useState(0);
  const cb = useRef(onEvent);
  cb.current = onEvent;
  useEffect(() => {
    if (!path || typeof EventSource === "undefined") return;
    const es = new EventSource(path, { withCredentials: true });
    const handler = (ev: MessageEvent) => {
      try {
        const e = JSON.parse(ev.data) as StreamEvent;
        setLast(Date.now());
        cb.current(e);
      } catch { /* ignore malformed */ }
    };
    es.onopen = () => { setConnected(true); setLast(Date.now()); };
    es.onerror = () => setConnected(false);
    for (const t of ["event", "state", "progress", "metrics", "operation", "check", "log", "alert"]) es.addEventListener(t, handler as EventListener);
    return () => es.close();
  }, [path]);
  return { connected, lastEventAt };
}

// useNow re-renders every intervalMs with the current time.
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}

// useDebounced returns value after it has been stable for ms.
export function useDebounced<T>(value: T, ms = 250): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setV(value), ms);
    return () => clearTimeout(id);
  }, [value, ms]);
  return v;
}
