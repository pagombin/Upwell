import { createContext, ReactNode, useCallback, useContext, useEffect, useState } from "react";
import { duration, time } from "../lib/format";
import { useApi, useNow } from "../lib/hooks";
import type { Operation } from "../lib/types";
import { LogViewer } from "./logview";
import { Dialog, ErrorState, Pill, ProgressBar, Skeleton } from "./ui";

// Operation console pattern: any long operation opens a panel with numbered
// steps, live status and a log pane filtered to that operation. Panels can be
// minimized to a tray and reopened.

interface OpsCtx { open: (id: string, title: string) => void; tray: { id: string; title: string }[] }
const Ctx = createContext<OpsCtx>({ open: () => {}, tray: [] });
export function useOps() { return useContext(Ctx); }

export function OpsProvider({ children }: { children: ReactNode }) {
  const [shown, setShown] = useState<{ id: string; title: string } | null>(null);
  const [tray, setTray] = useState<{ id: string; title: string }[]>([]);
  const open = useCallback((id: string, title: string) => {
    setTray((t) => t.filter((x) => x.id !== id));
    setShown({ id, title });
  }, []);
  const minimize = () => {
    if (shown) setTray((t) => [...t.filter((x) => x.id !== shown.id), shown].slice(-5));
    setShown(null);
  };
  return (
    <Ctx.Provider value={{ open, tray }}>
      {children}
      {tray.length > 0 && (
        <div className="tray" aria-label="Minimized operations">
          {tray.map((t) => <TrayButton key={t.id} id={t.id} title={t.title} onOpen={() => open(t.id, t.title)} onDismiss={() => setTray((x) => x.filter((y) => y.id !== t.id))} />)}
        </div>
      )}
      {shown && <OperationPanel id={shown.id} title={shown.title} onClose={() => setShown(null)} onMinimize={minimize} />}
    </Ctx.Provider>
  );
}

function TrayButton({ id, title, onOpen, onDismiss }: { id: string; title: string; onOpen: () => void; onDismiss: () => void }) {
  const { data } = useApi<Operation>(`/api/v1/operations/${id}`, 3000);
  const running = !data || data.state === "running";
  return (
    <span className="row" style={{ gap: 2, flexWrap: "nowrap" }}>
      <button onClick={onOpen}>{running ? <span className="spin" /> : <Pill state={data?.state === "done" ? "done" : "failed"} text={data?.state === "done" ? "Done" : "Failed"} />}<span className="trunc" style={{ maxWidth: 220 }}>{title}</span></button>
      {!running && <button className="ghost icon" aria-label={`Dismiss ${title}`} onClick={onDismiss}>✕</button>}
    </span>
  );
}

export function OpSteps({ op, now }: { op: Operation; now: number }) {
  return (
    <ol className="opsteps">
      {op.steps.map((s, i) => (
        <li key={s.key} className={s.state}>
          <span className="num" aria-hidden="true">{s.state === "done" ? "✓" : s.state === "failed" ? "!" : i + 1}</span>
          <div className="stack" style={{ gap: 2 }}>
            <span>{s.title}</span>
            {s.detail && <span className="detail">{s.detail}</span>}
            {s.progress !== undefined && s.state === "running" && <ProgressBar value={s.progress} label={`${s.title} progress`} />}
          </div>
          <span className="small muted" style={{ whiteSpace: "nowrap" }}>
            {s.state === "running" ? <span className="row" style={{ gap: 6 }}><span className="spin" />{duration(now - (s.started_at || now))}</span>
              : s.started_at && s.ended_at ? duration(s.ended_at - s.started_at) : s.state === "waiting" ? "Waiting" : s.state === "skipped" ? "Skipped" : ""}
          </span>
        </li>
      ))}
    </ol>
  );
}

export function OperationPanel({ id, title, onClose, onMinimize }: { id: string; title: string; onClose: () => void; onMinimize: () => void }) {
  const { data, error, refresh } = useApi<Operation>(`/api/v1/operations/${id}`);
  const now = useNow(1000);
  const running = !data || data.state === "running";
  useEffect(() => {
    if (!running) return;
    const t = setInterval(refresh, 1000);
    return () => clearInterval(t);
  }, [running, refresh]);
  const [level, setLevel] = useState("info");
  return (
    <Dialog title={title} onClose={onClose} wide footer={<>
      <button onClick={onMinimize}>Minimize to tray</button>
      <button className="primary" onClick={onClose}>Close</button>
    </>}>
      {error ? <ErrorState error={error} retry={refresh} /> : !data ? <Skeleton lines={5} /> : (
        <>
          <div className="row">
            <Pill state={data.state === "running" ? "running" : data.state === "done" ? "done" : data.state === "interrupted" ? "interrupted" : "failed"}
              text={data.state === "running" ? "Running" : data.state === "done" ? "Done" : data.state === "interrupted" ? "Interrupted" : "Failed"} />
            <span className="muted small">Started {time(data.started_at)}{data.started_by ? ` by ${data.started_by}` : ""}</span>
            <span className="muted small">Elapsed {duration((data.ended_at || now) - data.started_at)}</span>
            <span className="mono faint small" title="Operation ID">{data.id}</span>
          </div>
          {data.error && <div className="callout crit"><div className="body"><strong>The operation failed</strong><span className="wrapany">{data.error}</span></div></div>}
          <OpSteps op={data} now={now} />
          <div className="row">
            <h3 style={{ flex: 1 }}>Operation log</h3>
            <label className="row small" style={{ gap: 6 }}>Verbosity
              <select value={level} onChange={(e) => setLevel(e.target.value)} aria-label="Log verbosity">
                <option value="debug">Debug</option><option value="info">Info</option><option value="warn">Warnings</option><option value="error">Errors</option>
              </select>
            </label>
          </div>
          <LogViewer filter={{ op: id, level }} height={220} label="Operation log" />
        </>
      )}
    </Dialog>
  );
}
