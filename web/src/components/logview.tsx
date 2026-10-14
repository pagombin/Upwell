import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api } from "../lib/api";
import { time } from "../lib/format";
import { useStream } from "../lib/hooks";
import type { LogRecord } from "../lib/types";
import { Drawer, ErrorState, Skeleton } from "./ui";

const ROW = 22;
const MAX = 20000;
const PAGE = 2000;

export interface LogFilter { migration?: string; database?: string; component?: string; level?: string; op?: string; q?: string; attempt?: string }

function qs(f: LogFilter, extra: Record<string, string | number> = {}) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries({ ...f, ...extra })) if (v !== undefined && v !== "") p.set(k, String(v));
  return p.toString();
}

// merge adds lines to cur in sequence order, without duplicates: lines from
// the history, the live tail and a backfill can overlap or arrive out of order.
function merge(cur: LogRecord[], add: LogRecord[]): LogRecord[] {
  if (!add.length) return cur;
  const seen = new Set<number>();
  for (const r of cur) if (r.seq) seen.add(r.seq);
  const fresh = add.filter((r) => !r.seq || !seen.has(r.seq));
  if (!fresh.length) return cur;
  const lastSeq = cur.length ? cur[cur.length - 1].seq || 0 : 0;
  let next: LogRecord[];
  if (fresh.every((r) => !r.seq || r.seq > lastSeq)) next = cur.concat(fresh);
  else next = cur.concat(fresh).sort((a, b) => (a.seq || Number.MAX_SAFE_INTEGER) - (b.seq || Number.MAX_SAFE_INTEGER));
  return next.length > MAX ? next.slice(next.length - MAX) : next;
}

function sameDay(a: number, b: number) {
  return new Date(a).toDateString() === new Date(b).toDateString();
}

// stamp is the time column: the time of day, with the date for a line from another day.
function stamp(ts: number, today: number) {
  const t = time(ts, false).replace(/ \S+$/, "");
  if (sameDay(ts, today)) return t;
  return `${new Date(ts).toLocaleDateString("en-GB", { day: "2-digit", month: "short" })} ${t}`;
}

function Highlight({ text, q }: { text: string; q?: string }) {
  if (!q) return <>{text}</>;
  const lower = text.toLowerCase(), needle = q.toLowerCase();
  const out: React.ReactNode[] = [];
  let i = 0;
  for (let j = lower.indexOf(needle); j >= 0 && out.length < 40; j = lower.indexOf(needle, i)) {
    if (j > i) out.push(text.slice(i, j));
    out.push(<mark key={j}>{text.slice(j, j + q.length)}</mark>);
    i = j + q.length;
  }
  out.push(text.slice(i));
  return <>{out}</>;
}

// LogViewer: virtualized rows, live tail over SSE (gaps from a pause or a
// reconnect are filled from the history), older lines on demand, follow
// mode, error navigation and a detail drawer per line.
export function LogViewer({ filter, height = 420, live = true, label = "Logs" }: { filter: LogFilter; height?: number; live?: boolean; label?: string }) {
  const [rows, setRows] = useState<LogRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error>();
  const [follow, setFollow] = useState(true);
  const [paused, setPaused] = useState(false);
  const [pausedAt, setPausedAt] = useState(0);
  const [scrollTop, setScrollTop] = useState(0);
  const [open, setOpen] = useState<LogRecord | null>(null);
  const [hl, setHl] = useState(-1);
  const [older, setOlder] = useState<"idle" | "loading" | "none">("idle");
  const [gapError, setGapError] = useState("");
  const box = useRef<HTMLDivElement>(null);
  const key = qs(filter);
  const keyRef = useRef(key);
  keyRef.current = key;
  const pending = useRef<LogRecord[]>([]);
  const rowsRef = useRef<LogRecord[]>([]);
  rowsRef.current = rows;
  const ready = useRef(false);
  const today = Date.now();

  // History for this filter. Live lines that arrive meanwhile wait in
  // pending and are merged by sequence, so none is lost or doubled.
  useEffect(() => {
    let alive = true;
    ready.current = false;
    pending.current = [];
    setLoading(true);
    setRows([]);
    setOlder("idle");
    setHl(-1);
    api.get<LogRecord[]>(`/api/v1/logs?${qs(filter, { limit: PAGE })}`).then(
      (r) => {
        if (!alive) return;
        ready.current = true;
        const add = pending.current;
        pending.current = [];
        setRows(merge(r, add));
        if (r.length < PAGE) setOlder("none");
        setLoading(false);
        setError(undefined);
      },
      (e) => { if (alive) { setError(e); setLoading(false); } },
    );
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  // backfill fetches what the live tail missed (paused, or disconnected)
  // after the newest line held.
  const backfill = useCallback(async () => {
    const k = keyRef.current;
    let after = 0;
    for (const r of rowsRef.current) if (r.seq && r.seq > after) after = r.seq;
    if (!after) return;
    try {
      for (let pages = 0; pages < 5; pages++) {
        const got = await api.get<LogRecord[]>(`/api/v1/logs?${qs(filter, { after, limit: 5000 })}`);
        if (keyRef.current !== k) return;
        setRows((cur) => merge(cur, got));
        setGapError("");
        if (got.length < 5000) return;
        after = got[got.length - 1].seq || after;
      }
    } catch (e) {
      setGapError(`Lines written while the live tail was not connected could not be loaded: ${(e as Error).message}`);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  // Text search is applied server-side for history and client-side for the live tail.
  const matches = useCallback((r: LogRecord) => !filter.q || r.msg.toLowerCase().includes(filter.q.toLowerCase()), [filter.q]);
  const { connected } = useStream(live && !paused ? `/api/v1/stream/logs?${qs({ ...filter, q: undefined, attempt: undefined })}` : null, (e) => {
    const r = e.data as LogRecord;
    if (r && typeof r.msg === "string" && matches(r) && (!filter.attempt || String(r.attempt || "") === filter.attempt)) pending.current.push(r);
  });
  // A reconnect (or the end of a pause) leaves a gap: fill it.
  const wasConnected = useRef(false);
  useEffect(() => {
    if (connected && !wasConnected.current && ready.current) void backfill();
    wasConnected.current = connected;
  }, [connected, backfill]);

  // Batch appends to at most four renders a second.
  useEffect(() => {
    const id = setInterval(() => {
      if (!ready.current || !pending.current.length) return;
      const add = pending.current;
      pending.current = [];
      setRows((cur) => merge(cur, add));
    }, 250);
    return () => clearInterval(id);
  }, []);

  useEffect(() => {
    if (follow && box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [rows, follow]);

  const loadOlder = async () => {
    const first = rowsRef.current.find((r) => r.seq)?.seq;
    if (!first) return;
    const k = keyRef.current;
    setOlder("loading");
    try {
      const got = await api.get<LogRecord[]>(`/api/v1/logs?${qs(filter, { before: first, limit: PAGE })}`);
      if (keyRef.current !== k) return;
      const el = box.current;
      const added = got.filter((r) => !r.seq || r.seq < first).length;
      setFollow(false);
      setRows((cur) => merge(got, cur));
      // Keep the lines on screen where they were.
      if (el) requestAnimationFrame(() => { el.scrollTop += added * ROW; });
      setOlder(got.length < PAGE ? "none" : "idle");
    } catch (e) {
      setOlder("idle");
      setGapError(`Older lines could not be loaded: ${(e as Error).message}`);
    }
  };

  const scrollToRow = (n: number) => {
    const el = box.current;
    if (!el) return;
    if (n * ROW < el.scrollTop || (n + 1) * ROW > el.scrollTop + el.clientHeight) el.scrollTop = Math.max(0, n * ROW - el.clientHeight / 2);
  };
  const errorRows = useMemo(() => rows.reduce<number[]>((a, r, i) => (r.level === "error" ? (a.push(i), a) : a), []), [rows]);
  const jumpError = (dir: 1 | -1) => {
    if (!errorRows.length) return;
    const from = hl >= 0 ? hl : dir === 1 ? -1 : rows.length;
    const n = dir === 1 ? errorRows.find((i) => i > from) ?? errorRows[0] : [...errorRows].reverse().find((i) => i < from) ?? errorRows[errorRows.length - 1];
    setFollow(false);
    setHl(n);
    scrollToRow(n);
  };

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
  const togglePause = () => {
    setPaused((p) => {
      if (!p) setPausedAt(Date.now());
      return !p;
    });
  };
  // Download exactly the lines loaded here (the range shown), or the
  // newest 5,000 for this filter from the server.
  const downloadLoaded = () => {
    const blob = new Blob(rows.map((r) => JSON.stringify(r) + "\n"), { type: "application/x-ndjson" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    // Its own name every time: scope, the lines' range and the time of download.
    const stamp = new Date().toISOString().replace(/[-:]/g, "").replace(/\.\d+Z$/, "Z");
    const scope = [filter.migration, filter.database].filter(Boolean).map((x) => String(x).replace(/[^A-Za-z0-9._-]/g, "_")).join("-");
    a.download = `upwell-logs-${scope ? scope + "-" : ""}${rows.length ? rows[0].seq : 0}-${rows.length ? rows[rows.length - 1].seq : 0}-${stamp}.jsonl`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  };

  const first = Math.max(0, Math.floor(scrollTop / ROW) - 10);
  const visible = Math.ceil(height / ROW) + 20;
  const slice = useMemo(() => rows.slice(first, first + visible), [rows, first, visible]);
  const range = rows.length ? `${stamp(rows[0].ts, today)} to ${stamp(rows[rows.length - 1].ts, today)}` : "";

  return (
    <div className="stack" style={{ gap: 8 }}>
      <div className="row small muted" style={{ flexWrap: "wrap" }}>
        <span>{rows.length.toLocaleString("en-US")} lines{range && ` · ${range}`}{rows.length >= MAX ? " (oldest dropped from view)" : ""}</span>
        {live && <span role="status">{paused ? `Live tail paused at ${stamp(pausedAt, today)}; new lines are loaded when you resume` : connected ? "Live" : "Connecting…"}</span>}
        <span style={{ flex: 1 }} />
        {errorRows.length > 0 && <>
          <button className="ghost" onClick={() => jumpError(-1)} aria-label="Previous error line">↑ Error</button>
          <button className="ghost" onClick={() => jumpError(1)} aria-label="Next error line">{errorRows.length.toLocaleString("en-US")} error{errorRows.length === 1 ? "" : "s"} ↓</button>
        </>}
        {live && <button className="ghost" onClick={togglePause}>{paused ? "Resume live tail" : "Pause live tail"}</button>}
        {!follow && <button className="ghost" onClick={() => { setHl(-1); setFollow(true); }}>Jump to latest</button>}
        <button className="ghost" onClick={downloadLoaded} disabled={!rows.length} title="The lines loaded here, as JSON lines">Download shown</button>
        <a className="btn" href={`/api/v1/logs?${qs(filter, { format: "download", limit: 5000 })}`} download title="The newest 5,000 lines for this filter">Download</a>
      </div>
      {gapError && <div className="callout warn" role="alert"><div className="body"><span>{gapError}</span></div></div>}
      {error ? <ErrorState error={error} /> : loading ? <Skeleton lines={6} /> : (
        <div className="logview" ref={box} style={{ height }} onScroll={onScroll} onKeyDown={onKey} role="log" aria-label={`${label}. Use the arrow keys to move between lines and Enter to open one.`} tabIndex={0}>
          {rows.length === 0 ? (
            <div className="empty" style={{ margin: 12 }}><h3>No log lines match</h3><p>Lines appear here as soon as Upwell or the engine writes them. Widen the level or clear the search.</p></div>
          ) : (
            <>
              {older !== "none" && (
                <div className="row" style={{ justifyContent: "center", height: ROW * 2 }}>
                  <button className="ghost small" onClick={loadOlder} disabled={older === "loading"}>{older === "loading" ? "Loading older lines…" : "Load older lines"}</button>
                </div>
              )}
              <div style={{ height: rows.length * ROW, position: "relative" }}>
                {slice.map((r, i) => {
                  const n = first + i;
                  const prev = n > 0 ? rows[n - 1] : undefined;
                  const newDay = prev && !sameDay(prev.ts, r.ts);
                  return (
                    <div key={(r.seq || 0) + ":" + n} id={`logline-${n}`} className={`logrow ${r.level}${n === hl ? " hl" : ""}${newDay ? " newday" : ""}`} style={{ position: "absolute", top: n * ROW, left: 0, right: 0 }}
                      onClick={() => { setHl(n); setOpen(r); }}>
                      <span className="faint" title={time(r.ts)}>{stamp(r.ts, today)}</span>
                      <span className={`lv-${r.level}`}>{r.level}</span>
                      <span className="muted" title={r.component}>{r.component}</span>
                      <span className="muted" title={r.database || ""}>{r.database || "—"}</span>
                      <span title={r.msg}><Highlight text={r.msg} q={filter.q} /></span>
                    </div>
                  );
                })}
              </div>
            </>
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
            {!!open.seq && <><dt>Line</dt><dd className="mono">{open.seq}</dd></>}
          </dl>
          <pre className="cmd">{open.msg}</pre>
          {open.raw && open.raw !== open.msg && <><h3>Raw line</h3><pre className="cmd">{open.raw}</pre></>}
          <button className="ghost" onClick={() => navigator.clipboard?.writeText(open.raw || open.msg)}>Copy line</button>
        </Drawer>
      )}
    </div>
  );
}
