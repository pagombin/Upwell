import { createContext, type ReactNode, useCallback, useContext, useEffect, useId, useRef, useState } from "react";
import { ApiError } from "../lib/api";

// ---------- state labels and pills

export const LABELS: Record<string, string> = {
  draft: "Draft", ready: "Ready", queued: "Queued", running: "Running", streaming: "Streaming", cutover: "Cutover", verifying: "Verifying",
  completed: "Completed", needs_attention: "Needs attention", paused: "Paused", aborted: "Aborted", cleaned_up: "Cleaned up",
  pending: "Pending", preparing: "Preparing", base_copy: "Base copy", catch_up: "Catch-up", in_sync: "In sync", draining: "Draining",
  drained: "Drained", verified: "Verified", failed: "Failed", degraded: "Degraded", stopped: "Stopped", restart_required: "Restart required",
};
const KIND: Record<string, string> = {
  draft: "neutral", ready: "info", queued: "neutral", running: "info", streaming: "ok", cutover: "cutover", verifying: "cutover", completed: "ok",
  needs_attention: "crit", paused: "warn", aborted: "warn", cleaned_up: "neutral", pending: "neutral", preparing: "info", base_copy: "info",
  catch_up: "info", in_sync: "ok", draining: "cutover", drained: "cutover", verified: "ok", failed: "crit", degraded: "crit", stopped: "warn",
  restart_required: "crit", ok: "ok", info: "info", warning: "warn", blocker: "crit", critical: "crit", firing: "crit", acknowledged: "warn",
  resolved: "neutral", GO: "ok", "NO-GO": "crit", done: "ok", failed_op: "crit", interrupted: "warn",
};
export function label(s?: string) { return (s && LABELS[s]) || s || "—"; }
export function kindOf(s?: string) { return (s && KIND[s]) || "neutral"; }

export function Pill({ state, text, kind }: { state?: string; text?: string; kind?: string }) {
  return <span className={`pill ${kind || kindOf(state)}`}>{text ?? label(state)}</span>;
}

export function LevelPill({ level, hard }: { level: string; hard?: boolean }) {
  const t = level === "blocker" ? (hard ? "Blocker (hard)" : "Blocker") : level === "ok" ? "OK" : level === "info" ? "Info" : "Warning";
  return <Pill kind={kindOf(level)} text={t} />;
}

// Trunc truncates with an ellipsis and shows the full value on hover.
export function Trunc({ children, title, className }: { children: string; title?: string; className?: string }) {
  return <span className={`trunc ${className || ""}`} title={title ?? children}>{children}</span>;
}

export function ComingSoon({ what }: { what?: string }) {
  return <span className="comingsoon" title={what ? `${what} is not part of this build` : undefined}>Coming soon</span>;
}

// ---------- tiles and layout

export function Tile({ label: l, value, sub, title }: { label: string; value: ReactNode; sub?: ReactNode; title?: string }) {
  return (
    <div className="tile" title={title}>
      <span className="label">{l}</span>
      <span className="value">{value}</span>
      <span className="sub">{sub ?? " "}</span>
    </div>
  );
}

export function Card({ title, actions, children, className, id }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string; id?: string }) {
  return (
    <section className={`card ${className || ""}`} aria-labelledby={title ? id : undefined}>
      {title !== undefined && (
        <div className="cardhead">
          <h2 id={id}>{title}</h2>
          {actions}
        </div>
      )}
      <div className="cardbody">{children}</div>
    </section>
  );
}

export function ProgressBar({ value, kind, label: l }: { value: number; kind?: string; label?: string }) {
  const v = Math.max(0, Math.min(1, isFinite(value) ? value : 0));
  return (
    <div className={`bar ${kind || ""}`} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(v * 100)} aria-label={l || "progress"}>
      <span style={{ width: `${v * 100}%` }} />
    </div>
  );
}

// ---------- states

export function Skeleton({ lines = 3, height = 14 }: { lines?: number; height?: number }) {
  return (
    <div className="stack" aria-busy="true" aria-label="Loading">
      {Array.from({ length: lines }).map((_, i) => <div key={i} className="skel" style={{ height, width: `${90 - i * 12}%` }} />)}
    </div>
  );
}

export function Empty({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {children && <p>{children}</p>}
      {action}
    </div>
  );
}

export function ErrorState({ error, retry }: { error: ApiError | Error | undefined; retry?: () => void }) {
  if (!error) return null;
  const e = error as ApiError;
  return (
    <div className="errorstate" role="alert">
      <h3>{e.status === 0 ? "Upwell is not reachable" : "This could not be loaded"}</h3>
      <p>{e.message}</p>
      {e.remediation && <p className="small">{e.remediation}</p>}
      {retry && <div><button onClick={retry}>Try again</button></div>}
    </div>
  );
}

export function StaleBadge({ updatedAt, now, limitMs = 30000 }: { updatedAt: number; now: number; limitMs?: number }) {
  if (!updatedAt || now - updatedAt < limitMs) return null;
  return <span className="stalebadge" role="status">Last updated {Math.round((now - updatedAt) / 1000)} s ago</span>;
}

// ---------- dialogs

export function Dialog({ title, onClose, children, footer, wide }: { title: string; onClose: () => void; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  const id = useId();
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null;
    const el = ref.current;
    const first = el?.querySelector<HTMLElement>("input, select, textarea, button:not(.ghost)");
    first?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
      if (e.key === "Tab" && el) {
        const items = Array.from(el.querySelectorAll<HTMLElement>("a[href], button:not([disabled]), input, select, textarea, [tabindex]:not([tabindex='-1'])"));
        if (!items.length) return;
        const f = items[0], l = items[items.length - 1];
        if (e.shiftKey && document.activeElement === f) { e.preventDefault(); l.focus(); }
        else if (!e.shiftKey && document.activeElement === l) { e.preventDefault(); f.focus(); }
      }
    };
    document.addEventListener("keydown", onKey);
    return () => { document.removeEventListener("keydown", onKey); prev?.focus(); };
  }, [onClose]);
  return (
    <div className="overlay" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose(); }}>
      <div className={`dialog ${wide ? "wide" : ""}`} role="dialog" aria-modal="true" aria-labelledby={id} ref={ref}>
        <div className="dhead">
          <h2 id={id}>{title}</h2>
          <button className="ghost icon" onClick={onClose} aria-label="Close">✕</button>
        </div>
        <div className="body">{children}</div>
        {footer && <div className="dfoot">{footer}</div>}
      </div>
    </div>
  );
}

// ConfirmDialog states exactly what will change; typed confirmation when typeToConfirm is set.
export function ConfirmDialog({ title, children, confirmLabel, danger, typeToConfirm, onConfirm, onClose, extra }: {
  title: string; children: ReactNode; confirmLabel: string; danger?: boolean; typeToConfirm?: string;
  onConfirm: (typed: string) => Promise<void> | void; onClose: () => void; extra?: ReactNode;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<ApiError>();
  const id = useId();
  const ok = !typeToConfirm || typed === typeToConfirm;
  const go = async () => {
    setBusy(true);
    setErr(undefined);
    try { await onConfirm(typed); onClose(); }
    catch (e) { setErr(e as ApiError); }
    finally { setBusy(false); }
  };
  return (
    <Dialog title={title} onClose={onClose} footer={<>
      <button onClick={onClose}>Cancel</button>
      <button className={danger ? "danger solid" : "primary"} disabled={!ok || busy} onClick={go}>{busy && <span className="spin" />}{confirmLabel}</button>
    </>}>
      {children}
      {extra}
      {typeToConfirm && (
        <label className="field" htmlFor={id}>
          <span className="lbl">Type <strong className="mono">{typeToConfirm}</strong> to confirm</span>
          <input id={id} value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" spellCheck={false} />
        </label>
      )}
      {err && <ErrorState error={err} />}
    </Dialog>
  );
}

export function Drawer({ title, onClose, children }: { title: ReactNode; onClose: () => void; children: ReactNode }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);
  return (
    <aside className="drawer" role="dialog" aria-label={typeof title === "string" ? title : "Details"}>
      <div className="dhead"><h2>{title}</h2><button className="ghost icon" onClick={onClose} aria-label="Close">✕</button></div>
      <div className="body">{children}</div>
    </aside>
  );
}

// ---------- toasts

interface Toast { id: number; kind: "ok" | "info" | "crit"; text: string; detail?: string }
const ToastCtx = createContext<(kind: Toast["kind"], text: string, detail?: string) => void>(() => {});
export function useToast() { return useContext(ToastCtx); }
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const push = useCallback((kind: Toast["kind"], text: string, detail?: string) => {
    const id = Date.now() + Math.random();
    setToasts((t) => [...t.slice(-3), { id, kind, text, detail }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), kind === "crit" ? 10000 : 5000);
  }, []);
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {toasts.map((t) => (
          <div key={t.id} className={`toast ${t.kind}`}>
            <div className="body"><strong>{t.text}</strong>{t.detail && <div className="small muted">{t.detail}</div>}</div>
            <button className="ghost icon" aria-label="Dismiss" onClick={() => setToasts((x) => x.filter((y) => y.id !== t.id))}>✕</button>
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  );
}

// useAction wraps an async action with busy state and toasts.
export function useAction() {
  const toast = useToast();
  const [busy, setBusy] = useState<string | null>(null);
  const run = useCallback(async <T,>(name: string, f: () => Promise<T>, done?: string): Promise<T | undefined> => {
    setBusy(name);
    try {
      const r = await f();
      if (done) toast("ok", done);
      return r;
    } catch (e) {
      const err = e as ApiError;
      toast("crit", err.message, err.remediation);
      return undefined;
    } finally {
      setBusy(null);
    }
  }, [toast]);
  return { run, busy };
}

export function Field({ label: l, hint, children, htmlFor }: { label: string; hint?: string; children: ReactNode; htmlFor?: string }) {
  return (
    <label className="field" htmlFor={htmlFor}>
      <span className="lbl">{l}</span>
      {children}
      {hint && <span className="hint">{hint}</span>}
    </label>
  );
}

export function Icon({ name, size = 16 }: { name: string; size?: number }) {
  const p: Record<string, ReactNode> = {
    home: <path d="M3 10.5 12 3l9 7.5V21h-6v-6H9v6H3z" />,
    logs: <><path d="M4 5h16M4 10h16M4 15h10M4 20h7" /></>,
    events: <><circle cx="12" cy="12" r="8" /><path d="M12 7v5l3 2" /></>,
    alert: <><path d="M12 3 2 20h20z" /><path d="M12 10v4M12 17v.5" /></>,
    host: <><rect x="3" y="4" width="18" height="7" rx="1.5" /><rect x="3" y="13" width="18" height="7" rx="1.5" /><path d="M7 7.5h.01M7 16.5h.01" /></>,
    settings: <><circle cx="12" cy="12" r="3" /><path d="M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M4.9 19.1 7 17M17 7l2.1-2.1" /></>,
    system: <><rect x="3" y="3" width="18" height="14" rx="2" /><path d="M8 21h8M12 17v4" /></>,
    plus: <path d="M12 5v14M5 12h14" />,
    db: <><ellipse cx="12" cy="5.5" rx="7" ry="2.5" /><path d="M5 5.5v13c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5v-13M5 12c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5" /></>,
    search: <><circle cx="11" cy="11" r="6" /><path d="m20 20-4.5-4.5" /></>,
    sun: <><circle cx="12" cy="12" r="4" /><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5 19 19M5 19l1.5-1.5M17.5 6.5 19 5" /></>,
    moon: <path d="M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z" />,
    bell: <><path d="M6 16V11a6 6 0 1 1 12 0v5l2 2H4z" /><path d="M10 21h4" /></>,
    migration: <><path d="M4 7h11l-3-3M20 17H9l3 3" /></>,
  };
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {p[name]}
    </svg>
  );
}
