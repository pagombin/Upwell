import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api } from "../lib/api";
import { time } from "../lib/format";
import { useStream } from "../lib/hooks";
import type { LogRecord } from "../lib/types";
import { Drawer, ErrorState, Skeleton } from "./ui";

const ROW = 22;
const MAX = 20000;

export interface LogFilter { migration?: string; database?: string; component?: string; level?: string; op?: string; q?: string; attempt?: string }

function qs(f: LogFilter, extra: Record<string, string | number> = {}) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries({ ...f, ...extra })) if (v !== undefined && v !== "") p.set(k, String(v));
  return p.toString();
}

// LogViewer: virtualized rows, live tail over SSE, follow mode, a detail drawer per line.
export function LogViewer({ filter, height = 420, live = true, label = "Logs" }: { filter: LogFilter; height?: number; live?: boolean; label?: string }) {
  const [rows, setRows] = useState<LogRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error>();
  const [follow, setFollow] = useState(true);
  const [paused, setPaused] = useState(false);
  const [scrollTop, setScrollTop] = useState(0);
  const [open, setOpen] = useState<LogRecord | null>(null);
  const [hl, setHl] = useState(-1);
  const box = useRef<HTMLDivElement>(null);
  const key = qs(filter);
  const pending = useRef<LogRecord[]>([]);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    api.get<LogRecord[]>(`/api/v1/logs?${qs(filter, { limit: 2000 })}`).then(
      (r) => { if (alive) { setRows(r); setLoading(false); setError(undefined); } },
      (e) => { if (alive) { setError(e); setLoading(false); } },
    );
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  // Text search is applied server-side for history and client-side for the live tail.
  const matches = useCallback((r: LogRecord) => !filter.q || r.msg.toLowerCase().includes(filter.q.toLowerCase()), [filter.q]);
  const { connected } = useStream(live && !paused ? `/api/v1/stream/logs?${qs({ ...filter, q: undefined, attempt: undefined })}` : null, (e) => {
    const r = e.data as LogRecord;
    if (r && typeof r.msg === "string" && matches(r)) pending.current.push(r);
  });
  // Batch appends to at most four renders a second.
  useEffect(() => {
    const id = setInterval(() => {
      if (!pending.current.length) return;
      const add = pending.current;
      pending.current = [];
      setRows((cur) => {
        const last = cur.length ? cur[cur.length - 1].seq || 0 : 0;
        const fresh = add.filter((r) => !r.seq || r.seq > last);
        const next = cur.concat(fresh);
        return next.length > MAX ? next.slice(next.length - MAX) : next;
      });
    }, 250);
    return () => clearInterval(id);
  }, []);

  useEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [rows, follow]);

  const onScroll = () => {
    const el = box.current;
    if (!el) return;
    setScrollTop(el.scrollTop);
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < ROW * 2;
    if (atBottom !== follow) setFollow(atBottom);
  };
  // Keyboard: arrows move the highlighted line, Enter opens it, End follows.
  const onKey = (e: React.KeyboardEvent) => {
    if (!rows.length) return;
    let n = hl;
    if (e.key === "ArrowDown") n = Math.min(rows.length - 1, (hl < 0 ? Math.floor(scrollTop / ROW) : hl) + 1);
    else if (e.key === "ArrowUp") n = Math.max(0, (hl < 0 ? Math.floor(scrollTop / ROW) + Math.floor(height / ROW) : hl) - 1);
    else if (e.key === "Home") n = 0;
    else if (e.key === "End") { setHl(-1); setFollow(true); e.preventDefault(); return; }
    else if (e.key === "Enter" && hl >= 0) { setOpen(rows[hl]); e.preventDefault(); return; }
    else return;
    e.preventDefault();
    setHl(n);
    setFollow(false);
    const el = box.current;
    if (el) {
      if (n * ROW < el.scrollTop) el.scrollTop = n * ROW;
      else if ((n + 1) * ROW > el.scrollTop + el.clientHeight) el.scrollTop = (n + 1) * ROW - el.clientHeight;
    }
  };
  const first = Math.max(0, Math.floor(scrollTop / ROW) - 10);
  const visible = Math.ceil(height / ROW) + 20;
  const slice = useMemo(() => rows.slice(first, first + visible), [rows, first, visible]);

  return (
    <div className="stack" style={{ gap: 8 }}>
      <div className="row small muted">
        <span>{rows.length.toLocaleString("en-US")} lines{rows.length >= MAX ? " (oldest dropped from view)" : ""}</span>
        {live && <span>{paused ? "Live tail paused" : connected ? "Live" : "Connecting…"}</span>}
        <span style={{ flex: 1 }} />
        {live && <button className="ghost" onClick={() => setPaused((p) => !p)}>{paused ? "Resume live tail" : "Pause live tail"}</button>}
        {!follow && <button className="ghost" onClick={() => setFollow(true)}>Jump to latest</button>}
        <a className="btn" href={`/api/v1/logs?${qs(filter, { format: "download", limit: 5000 })}`} download>Download</a>
      </div>
      {error ? <ErrorState error={error} /> : loading ? <Skeleton lines={6} /> : (
        <div className="logview" ref={box} style={{ height }} onScroll={onScroll} onKeyDown={onKey} role="log" aria-label={`${label}. Use the arrow keys to move between lines and Enter to open one.`} tabIndex={0}>
          {rows.length === 0 ? (
            <div className="empty" style={{ margin: 12 }}><h3>No log lines match</h3><p>Lines appear here as soon as Upwell or the engine writes them. Widen the level or clear the search.</p></div>
          ) : (
            <div style={{ height: rows.length * ROW, position: "relative" }}>
              {slice.map((r, i) => (
                <div key={(r.seq || 0) + ":" + (first + i)} id={`logline-${first + i}`} className={`logrow ${r.level}${first + i === hl ? " hl" : ""}`} style={{ position: "absolute", top: (first + i) * ROW, left: 0, right: 0 }}
                  onClick={() => { setHl(first + i); setOpen(r); }}>
                  <span className="faint" title={time(r.ts)}>{time(r.ts, false).replace(/ \S+$/, "")}</span>
                  <span className={`lv-${r.level}`}>{r.level}</span>
                  <span className="muted" title={r.component}>{r.component}</span>
                  <span className="muted" title={r.database || ""}>{r.database || "—"}</span>
                  <span title={r.msg}>{r.msg}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
      {open && (
        <Drawer title="Log line" onClose={() => setOpen(null)}>
          <dl className="kv">
            <dt>Time</dt><dd>{time(open.ts)}</dd>
            <dt>Level</dt><dd>{open.level}</dd>
            <dt>Component</dt><dd>{open.component}</dd>
            {open.database && <><dt>Database</dt><dd className="mono">{open.database}</dd></>}
            {!!open.attempt && <><dt>Attempt</dt><dd>{open.attempt}</dd></>}
            {open.step && <><dt>Engine step</dt><dd>{open.step}</dd></>}
            {open.table && <><dt>Table</dt><dd className="mono">{open.table}</dd></>}
            {open.op && <><dt>Operation</dt><dd className="mono">{open.op}</dd></>}
          </dl>
          <pre className="cmd">{open.msg}</pre>
          {open.raw && open.raw !== open.msg && <><h3>Raw line</h3><pre className="cmd">{open.raw}</pre></>}
        </Drawer>
      )}
    </div>
  );
}
