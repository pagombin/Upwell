import { useEffect, useMemo, useState } from "react";
import { api } from "../lib/api";
import { duration, time } from "../lib/format";
import { useApi } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { CheckResult, PreflightRun } from "../lib/types";
import { useOps } from "../components/ops";
import { Card, Dialog, Empty, ErrorState, LevelPill, Skeleton, Tile, Trunc, useAction } from "../components/ui";

interface Latest { run: PreflightRun | null; results: CheckResult[]; catalog: { id: string; scope: string; title: string; fails_as: string }[] }

const ORDER = { blocker: 0, warning: 1, info: 2, ok: 3 } as Record<string, number>;
function groupOf(r: CheckResult) { return r.database ? `Database ${r.database}` : r.scope.charAt(0).toUpperCase() + r.scope.slice(1); }

export function usePreflight(mig: string) {
  const l = useApi<Latest>(`/api/v1/migrations/${mig}/preflight/latest`);
  const running = l.data?.run?.state === "running";
  useEffect(() => {
    if (!running) return;
    const t = setInterval(l.refresh, 1500);
    return () => clearInterval(t);
  }, [running, l.refresh]);
  return l;
}

export function RunPreflightButton({ mig, onStarted, label = "Run preflight" }: { mig: string; onStarted?: () => void; label?: string }) {
  const { run, busy } = useAction();
  const ops = useOps();
  const { can } = useSession();
  if (!can("operator")) return null;
  return (
    <button className="primary" disabled={!!busy} onClick={async () => {
      const r = await run("pf", () => api.post<{ operation_id: string }>(`/api/v1/migrations/${mig}/preflight`));
      if (r) { ops.open(r.operation_id, "Preflight"); onStarted?.(); }
    }}>{busy && <span className="spin" />}{label}</button>
  );
}

export function PreflightSummary({ run }: { run: PreflightRun }) {
  const s = run.summary;
  return (
    <div className="grid cols-4">
      <Tile label="Checks" value={s.total} sub={run.state === "running" ? "Running…" : `Finished ${time(run.ended_at)}`} />
      <Tile label="Blockers" value={<span style={{ color: s.blockers - s.accepted > 0 ? "var(--crit)" : undefined }}>{s.blockers}</span>} sub={`${s.hard} hard · ${s.accepted} accepted`} />
      <Tile label="Warnings" value={<span style={{ color: s.warnings ? "var(--warn)" : undefined }}>{s.warnings}</span>} sub={s.needs_warning_review ? "Review before starting" : "Nothing to review"} />
      <Tile label="Can start" value={s.can_start ? "Yes" : "No"} sub={s.can_start ? "Every blocker passed or accepted" : "Resolve or accept every blocker"} />
    </div>
  );
}

export function PreflightResults({ mig, latest, refresh }: { mig: string; latest: Latest; refresh: () => void }) {
  const [only, setOnly] = useState<"problems" | "all">("problems");
  const [open, setOpen] = useState<CheckResult | null>(null);
  const [accepting, setAccepting] = useState<CheckResult | null>(null);
  const { can } = useSession();
  const results = latest.results;
  const groups = useMemo(() => {
    const g = new Map<string, CheckResult[]>();
    for (const r of results) {
      if (only === "problems" && (r.level === "ok" || r.level === "info")) continue;
      const k = groupOf(r);
      if (!g.has(k)) g.set(k, []);
      g.get(k)!.push(r);
    }
    for (const v of g.values()) v.sort((a, b) => (ORDER[a.level] - ORDER[b.level]) || a.title.localeCompare(b.title));
    return [...g.entries()];
  }, [results, only]);
  return (
    <div className="stack">
      <div className="row">
        <label className="row small" style={{ gap: 6 }}>Show
          <select value={only} onChange={(e) => setOnly(e.target.value as "problems" | "all")}><option value="problems">Blockers and warnings</option><option value="all">Every check</option></select>
        </label>
      </div>
      {groups.length === 0 ? <Empty title={results.length ? "Every check passed" : "No results yet"}>{results.length ? "Switch to every check to see the evidence for each one." : "Results appear here as each check finishes."}</Empty> :
        groups.map(([g, rs]) => (
          <Card key={g} title={<Trunc>{g}</Trunc>}>
            <div className="tablewrap">
              <table className="data" aria-label={`${g} checks`}>
                <thead><tr><th style={{ width: 150 }}>Result</th><th>Check</th><th>Finding</th><th style={{ width: 210 }}></th></tr></thead>
                <tbody>
                  {rs.map((r) => (
                    <tr key={r.id}>
                      <td><LevelPill level={r.level} hard={r.hard} />{r.accepted && <span className="pill warn nodot" style={{ marginLeft: 6 }} title={`Accepted by ${r.accepted.accepted_by}: ${r.accepted.reason}`}>Accepted</span>}</td>
                      <td style={{ maxWidth: 260 }}><Trunc>{r.title}</Trunc><span className="mono faint small">{r.check_id}</span></td>
                      <td className="wrapany">{r.message}</td>
                      <td>
                        <div className="row" style={{ gap: 6, flexWrap: "nowrap" }}>
                          <button className="ghost" onClick={() => setOpen(r)}>Evidence</button>
                          {r.level === "blocker" && !r.hard && !r.accepted && can("operator") && <button onClick={() => setAccepting(r)}>Accept risk</button>}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
        ))}
      {open && (
        <Dialog title={open.title} onClose={() => setOpen(null)} wide>
          <div className="row"><LevelPill level={open.level} hard={open.hard} /><span className="mono small faint">{open.check_id} v{open.version}</span><span className="small muted">{duration(open.duration_ms)}</span></div>
          <p className="wrapany">{open.message}</p>
          {open.remediation && <div className="callout info"><div className="body"><strong>What to do</strong><span className="wrapany">{open.remediation}</span></div></div>}
          {open.accepted && <div className="callout warn"><div className="body"><strong>Accepted by {open.accepted.accepted_by} at {time(open.accepted.accepted_at)}</strong><span>{open.accepted.reason}</span></div></div>}
          <h3>Evidence</h3>
          <pre className="cmd">{JSON.stringify(open.evidence, null, 2)}</pre>
        </Dialog>
      )}
      {accepting && <AcceptDialog mig={mig} r={accepting} onClose={() => setAccepting(null)} onDone={refresh} />}
    </div>
  );
}

function AcceptDialog({ mig, r, onClose, onDone }: { mig: string; r: CheckResult; onClose: () => void; onDone: () => void }) {
  const [reason, setReason] = useState("");
  const { run, busy } = useAction();
  return (
    <Dialog title="Accept this risk" onClose={onClose} footer={<>
      <button onClick={onClose}>Cancel</button>
      <button className="primary" disabled={reason.trim().length < 10 || !!busy} onClick={async () => {
        const ok = await run("accept", () => api.post(`/api/v1/migrations/${mig}/acceptances`, { result_id: r.id, reason: reason.trim() }), "Risk accepted");
        if (ok) { onDone(); onClose(); }
      }}>Accept risk</button>
    </>}>
      <p><strong>{r.title}</strong>{r.database ? <> for <span className="mono">{r.database}</span></> : null}</p>
      <p className="wrapany muted">{r.message}</p>
      <p className="small">The acceptance is recorded in the audit log and the report with your name and reason. It lapses if a later preflight run finds different evidence.</p>
      <label className="field"><span className="lbl">Reason (at least 10 characters)</span><textarea value={reason} onChange={(e) => setReason(e.target.value)} /></label>
    </Dialog>
  );
}

export function PreflightScreen({ mig }: { mig: string }) {
  const l = usePreflight(mig);
  const runs = useApi<PreflightRun[]>(`/api/v1/migrations/${mig}/preflight/runs`);
  const [cmp, setCmp] = useState("");
  const other = useApi<CheckResult[]>(cmp ? `/api/v1/migrations/${mig}/preflight/runs/${cmp}` : null);
  const diff = useMemo(() => {
    if (!cmp || !other.data || !l.data) return [];
    const k = (r: CheckResult) => `${r.check_id}|${r.database || ""}`;
    const before = new Map(other.data.map((r) => [k(r), r]));
    const out: { key: string; title: string; db?: string; from: string; to: string }[] = [];
    for (const r of l.data.results) {
      const b = before.get(k(r));
      if (!b || b.level !== r.level) out.push({ key: k(r), title: r.title, db: r.database, from: b?.level || "not run", to: r.level });
      before.delete(k(r));
    }
    for (const b of before.values()) out.push({ key: k(b), title: b.title, db: b.database, from: b.level, to: "not run" });
    return out;
  }, [cmp, other.data, l.data]);
  const accepted = (l.data?.results || []).filter((r) => r.accepted);
  return (
    <div className="stack" style={{ gap: 24 }}>
      <div className="row"><h2 style={{ flex: 1 }}>Latest run</h2><RunPreflightButton mig={mig} onStarted={() => { l.refresh(); runs.refresh(); }} label="Run preflight again" /></div>
      {l.error ? <ErrorState error={l.error} retry={l.refresh} /> : !l.data ? <Skeleton lines={6} /> : !l.data.run ? (
        <Empty title="Preflight has not run yet" action={<RunPreflightButton mig={mig} onStarted={l.refresh} />}>Preflight checks the source, the target, this droplet and each database, and decides whether the migration may start.</Empty>
      ) : <>
        <PreflightSummary run={l.data.run} />
        <PreflightResults mig={mig} latest={l.data} refresh={l.refresh} />
      </>}
      <Card title="Accepted risks">
        {accepted.length === 0 ? <p className="muted">No risks are accepted on the latest run.</p> : (
          <ul className="stack" style={{ margin: 0, paddingLeft: 18 }}>
            {accepted.map((r) => <li key={r.id}><strong>{r.title}</strong>{r.database && <span className="mono"> {r.database}</span>}: {r.accepted!.reason} <span className="muted small">({r.accepted!.accepted_by}, {time(r.accepted!.accepted_at)})</span></li>)}
          </ul>
        )}
      </Card>
      <Card title="History" actions={runs.data && runs.data.length > 1 ? (
        <label className="row small" style={{ gap: 6 }}>Compare latest with
          <select value={cmp} onChange={(e) => setCmp(e.target.value)}>
            <option value="">Choose a run</option>
            {runs.data.slice(1).map((r) => <option key={r.id} value={r.id}>{time(r.started_at)}</option>)}
          </select>
        </label>
      ) : undefined}>
        {!runs.data ? <Skeleton /> : runs.data.length === 0 ? <p className="muted">No runs yet.</p> : (
          <div className="stack">
            <div className="tablewrap">
              <table className="data" aria-label="Preflight runs">
                <thead><tr><th>Started</th><th>State</th><th className="num">Checks</th><th className="num">Blockers</th><th className="num">Warnings</th><th>Could start</th></tr></thead>
                <tbody>{runs.data.map((r) => <tr key={r.id}><td>{time(r.started_at)}</td><td>{r.state}</td><td className="num">{r.summary.total}</td><td className="num">{r.summary.blockers}</td><td className="num">{r.summary.warnings}</td><td>{r.summary.can_start ? "Yes" : "No"}</td></tr>)}</tbody>
              </table>
            </div>
            {cmp && (other.data ? (diff.length === 0 ? <p className="muted">The two runs have the same results.</p> : (
              <div className="tablewrap">
                <table className="data" aria-label="Changes between runs">
                  <thead><tr><th>Check</th><th>Database</th><th>Before</th><th>Now</th></tr></thead>
                  <tbody>{diff.map((d) => <tr key={d.key}><td>{d.title}</td><td className="mono">{d.db || "—"}</td><td>{d.from}</td><td>{d.to}</td></tr>)}</tbody>
                </table>
              </div>
            )) : <Skeleton />)}
          </div>
        )}
      </Card>
    </div>
  );
}
