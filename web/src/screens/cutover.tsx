import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../lib/api";
import { bytes, duration, time } from "../lib/format";
import { useApi, useNow } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Operation, VerificationRow } from "../lib/types";
import { OpSteps, useOps } from "../components/ops";
import { Card, ComingSoon, ConfirmDialog, Dialog, Empty, ErrorState, LevelPill, Pill, Skeleton, Trunc, useAction } from "../components/ui";
import { useMig } from "./migration";

interface Cond { key: string; label: string; ok: boolean; detail: string; database?: string }

export function Verdict({ verdict, at }: { verdict?: string; at?: number }) {
  if (!verdict) return null;
  const go = verdict === "GO";
  return (
    <div className={`callout ${go ? "ok" : "crit"}`} role="status" data-testid="verdict">
      <div className="body">
        <span className={`verdict ${go ? "go" : "nogo"}`}>{go ? "GO" : "NO-GO"}</span>
        <span>{go
          ? "Every database passed: the final heartbeat reached the target, each target origin reached its end position, and the data comparison matched. Applications may switch to the target."
          : "At least one database failed a data-safety check. Do not switch applications to the target. The source is unchanged; resume writes there and read the failed checks below."}</span>
        {at && <span className="small muted">Decided {time(at)}</span>}
      </div>
    </div>
  );
}

export function CutoverTab() {
  const { view, id } = useMig();
  const v = view.data!, m = v.migration, c = m.flags.cutover;
  const ready = useApi<{ conditions: Cond[]; ready: boolean }>(`/api/v1/migrations/${id}/cutover/readiness`, 5000);
  const op = useApi<Operation>(c?.op_id ? `/api/v1/operations/${c.op_id}` : null);
  const { can, me } = useSession();
  const ops = useOps();
  const { run, busy } = useAction();
  const now = useNow(1000);
  const running = !!c && !c.ended_at;
  useEffect(() => {
    if (!running) return;
    const t = setInterval(() => { op.refresh(); }, 1000);
    return () => clearInterval(t);
  }, [running, op.refresh]); // eslint-disable-line react-hooks/exhaustive-deps
  const [start, setStart] = useState(false);
  const [abort, setAbort] = useState(false);
  const [switched, setSwitched] = useState(false);
  const inc = v.databases.filter((d) => d.include);
  const endposSet = inc.some((d) => d.endpos);
  const pauseStart = c?.writes_stopped_at || c?.started_at;
  const pauseMs = pauseStart ? (c?.ended_at || now) - pauseStart : 0;
  const retryable = m.flags.verdict === "NO-GO" && !endposSet;
  const canStart = can("operator") && !m.fixture && !m.flags.cleaned_up && !m.flags.aborted && (!c || retryable) && (!m.flags.verdict || retryable);
  return (
    <div className="stack" style={{ gap: 24 }}>
      <Verdict verdict={m.flags.verdict} at={m.flags.verdict_at} />
      <div className="grid cols-2">
        <Card title="Readiness" actions={ready.data ? <Pill kind={ready.data.ready ? "ok" : "warn"} text={ready.data.ready ? "Ready" : "Not ready"} /> : undefined}>
          {ready.error ? <ErrorState error={ready.error} retry={ready.refresh} /> : !ready.data ? <Skeleton lines={5} /> : (
            <ul className="opsteps" aria-label="Readiness conditions">
              {ready.data.conditions.map((x, i) => (
                <li key={x.key + (x.database || "") + i} className={x.ok ? "done" : "failed"}>
                  <span className="num" aria-hidden="true">{x.ok ? "✓" : "!"}</span>
                  <div className="stack" style={{ gap: 2 }}><Trunc>{x.label}</Trunc>{x.detail && <span className="detail">{x.detail}</span>}</div>
                  <span />
                </li>
              ))}
            </ul>
          )}
        </Card>
        <Card title="Write pause">
          <div className="stack">
            <span className="timer" aria-live="off" data-testid="pause-timer">{c ? duration(pauseMs) : "0s"}</span>
            <span className="muted small">{!c ? "Starts when you confirm writers are stopped." : c.ended_at ? `Writes were paused for ${duration(pauseMs)}; the cutover ended ${time(c.ended_at)}.` : "Applications must not write to the source while this runs."}</span>
            <fieldset className="stack" style={{ border: 0, padding: 0, margin: 0, gap: 6 }}>
              <legend className="small muted" style={{ marginBottom: 6 }}>How writes stop</legend>
              <label className="check"><input type="radio" name="stopmethod" defaultChecked />Manual: the customer or you stop every application writer</label>
              <span className="row small muted"><span>Write freeze on the source (revokes write privileges, with preview)</span><ComingSoon what="Write freeze" /></span>
            </fieldset>
            {canStart && (
              <div className="row">
                <button className="cutover" disabled={!ready.data?.ready || !!busy} onClick={() => setStart(true)}>{retryable ? "Retry the cutover" : "Start the cutover"}</button>
                {!ready.data?.ready && <span className="small muted">Every readiness condition must pass first.</span>}
              </div>
            )}
            {running && can("operator") && !endposSet && <div><button className="danger" onClick={() => setAbort(true)}>Abort the cutover</button></div>}
          </div>
        </Card>
      </div>
      {c && (
        <Card title="Cutover steps" actions={c.op_id && <button className="ghost" onClick={() => ops.open(c.op_id, "Cutover")}>Open with logs</button>}>
          {op.error ? <ErrorState error={op.error} retry={op.refresh} /> : !op.data ? <Skeleton lines={6} /> : <OpSteps op={op.data} now={now} />}
        </Card>
      )}
      {c && (
        <Card title="Drain per database">
          <div className="tablewrap">
            <table className="data" aria-label="Drain progress">
              <thead><tr><th>Database</th><th>Phase</th><th>End position</th><th>Target origin</th><th>Final heartbeat at</th><th>Backlog</th><th>Verdict</th></tr></thead>
              <tbody>
                {inc.map((d) => (
                  <tr key={d.id}>
                    <td style={{ maxWidth: 260 }}><Trunc className="mono">{d.source_name}</Trunc></td>
                    <td><Pill state={d.state} /></td>
                    <td className="mono">{d.endpos || "—"}</td>
                    <td className="mono">{d.origin_lsn || "—"}</td>
                    <td className="mono">{d.hb_before_lsn || "—"}</td>
                    <td>{d.backlog_bytes === undefined || d.backlog_bytes === null ? "—" : bytes(d.backlog_bytes)}</td>
                    <td>{d.verdict ? <Pill state={d.verdict} text={d.verdict} /> : "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
      {m.flags.verdict && <VerificationSummary />}
      <div className="grid cols-2">
        <Card title="After GO">
          <ol className="stack" style={{ margin: 0, paddingLeft: 18, gap: 8 }}>
            <li>Point every application at the target cluster and start writers there.</li>
            <li>Mark applications as switched {m.flags.apps_switched ? <Pill kind="ok" text="Done" /> : m.flags.verdict === "GO" && can("operator") && !m.fixture ? <button style={{ marginLeft: 8 }} onClick={() => setSwitched(true)}>Mark switched</button> : null}</li>
            <li>Read and keep the <Link to="../report">report</Link>.</li>
            <li>Clean up: removes slots, origins and the heartbeat schema.</li>
          </ol>
        </Card>
        <Card title="Rollback">
          <div className="stack">
            <p>Until applications write to the target, rolling back means pointing them at the source again: Upwell never changes data on the source.</p>
            <p className="muted small">{endposSet ? "End positions are set, so the cutover can no longer be aborted; it finishes with a verdict. After a NO-GO, restart writers on the source." : "Before end positions are set, aborting the cutover keeps every database streaming."}</p>
          </div>
        </Card>
      </div>
      {start && <StartCutoverDialog mig={id} name={m.name} admin={me?.user.role === "admin"} onClose={() => setStart(false)} onStarted={(opId) => { ops.open(opId, "Cutover"); view.refresh(); }} />}
      {abort && (
        <ConfirmDialog title="Abort the cutover" danger confirmLabel="Abort the cutover" onClose={() => setAbort(false)}
          onConfirm={async () => { await run("abort", () => api.post(`/api/v1/migrations/${id}/cutover/abort`), "Cutover aborted; databases keep streaming"); view.refresh(); }}>
          <p>The cutover stops before end positions are set. Every database keeps streaming. Applications may write to the source again.</p>
        </ConfirmDialog>
      )}
      {switched && (
        <ConfirmDialog title="Mark applications switched" typeToConfirm={m.name} confirmLabel="Mark switched" onClose={() => setSwitched(false)}
          onConfirm={async (t) => { await api.post(`/api/v1/migrations/${id}/switched`, { confirm: t }); view.refresh(); }}>
          <p>Records that applications now write to the target. The report and audit log show who confirmed it and when.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}

function StartCutoverDialog({ mig, name, admin, onClose, onStarted }: { mig: string; name: string; admin: boolean; onClose: () => void; onStarted: (op: string) => void }) {
  const [stopped, setStopped] = useState(false);
  const [override, setOverride] = useState("");
  const [err, setErr] = useState<Error>();
  const [busy, setBusy] = useState(false);
  const go = async () => {
    setBusy(true); setErr(undefined);
    try {
      const r = await api.post<{ operation_id: string }>(`/api/v1/migrations/${mig}/cutover`, { confirm_writers_stopped: true, override_confirm: override });
      onStarted(r.operation_id); onClose();
    } catch (e) { setErr(e as Error); } finally { setBusy(false); }
  };
  return (
    <Dialog title="Start the cutover" onClose={onClose} footer={<><button onClick={onClose}>Cancel</button><button className="cutover" disabled={!stopped || busy} onClick={go}>{busy && <span className="spin" />}Start the cutover</button></>}>
      <p>Upwell will sample the source for writes, write a final heartbeat in every database, wait for it on the target, set each end position, drain, check each target origin, sync sequences and verify the data. The write pause lasts until the verdict.</p>
      <label className="check"><input type="checkbox" checked={stopped} onChange={(e) => setStopped(e.target.checked)} />Every application writer to the source is stopped</label>
      {admin && (
        <details>
          <summary>Admin override if Upwell keeps detecting writes</summary>
          <label className="field" style={{ marginTop: 8 }}><span className="lbl">Type <strong className="mono">{name}</strong> to continue despite detected writes</span><input value={override} onChange={(e) => setOverride(e.target.value)} autoComplete="off" /></label>
          <p className="small muted">Writes during the cutover are not lost from the source, but they may not reach the target. The override is recorded in the audit log and the report.</p>
        </details>
      )}
      {err && <ErrorState error={err} />}
    </Dialog>
  );
}

function useVerification(id: string) {
  return useApi<{ run_id: string; results: VerificationRow[] }>(`/api/v1/migrations/${id}/verification`, 15000);
}

function VerificationSummary() {
  const { id } = useMig();
  const ver = useVerification(id);
  if (!ver.data) return <Skeleton />;
  const fails = ver.data.results.filter((r) => r.result === "blocker");
  return (
    <Card title="Verification" actions={<Link to="../verification">Details</Link>}>
      {fails.length === 0 ? <p>{ver.data.results.length} checks passed across every database.</p> : (
        <ul className="stack" style={{ margin: 0, paddingLeft: 18 }}>{fails.map((f) => <li key={f.database + f.check_id}><span className="mono">{f.database}</span>: <strong>{f.title}</strong> {f.detail?.message}</li>)}</ul>
      )}
    </Card>
  );
}

export function VerificationTab() {
  const { id, view } = useMig();
  const ver = useVerification(id);
  const { can } = useSession();
  const ops = useOps();
  const { run, busy } = useAction();
  const [open, setOpen] = useState<VerificationRow | null>(null);
  const byDb = useMemo(() => {
    const g = new Map<string, VerificationRow[]>();
    for (const r of ver.data?.results || []) { if (!g.has(r.database)) g.set(r.database, []); g.get(r.database)!.push(r); }
    // Failed databases first, so a NO-GO's cause is at the top.
    return [...g.entries()].sort((a, b) => Number(b[1].some((r) => r.result === "blocker")) - Number(a[1].some((r) => r.result === "blocker")) || a[0].localeCompare(b[0]));
  }, [ver.data]);
  const m = view.data!.migration;
  return (
    <div className="stack" style={{ gap: 24 }}>
      <Verdict verdict={m.flags.verdict} at={m.flags.verdict_at} />
      <div className="row">
        <span className="muted" style={{ flex: 1 }}>Upwell never accepts the engine's own success: each database is checked for the final heartbeat, the target origin position, the schema, row counts and row checksums.</span>
        {can("operator") && !m.fixture && m.flags.verdict && !m.flags.cleaned_up && <button disabled={!!busy} onClick={async () => { const r = await run("v", () => api.post<{ operation_id: string }>(`/api/v1/migrations/${id}/verify`)); if (r) ops.open(r.operation_id, "Verify again"); }}>Verify again</button>}
      </div>
      {ver.error ? <ErrorState error={ver.error} retry={ver.refresh} /> : !ver.data ? <Skeleton lines={8} /> : byDb.length === 0 ? (
        <Empty title="Not verified yet">Verification runs as part of the cutover, after every database drains to its end position.</Empty>
      ) : byDb.map(([db, rows]) => {
        const bad = rows.some((r) => r.result === "blocker");
        return (
          <Card key={db} title={<span className="row" style={{ gap: 8, flexWrap: "nowrap" }}><Trunc className="mono">{db}</Trunc><Pill kind={bad ? "crit" : "ok"} text={bad ? "Failed" : "Passed"} /></span>}>
            <div className="tablewrap">
              <table className="data" aria-label={`Verification for ${db}`}>
                <thead><tr><th style={{ width: 150 }}>Result</th><th>Check</th><th>Detail</th><th style={{ width: 100 }}><span className="sr-only">Evidence</span></th></tr></thead>
                <tbody>
                  {rows.map((r) => (
                    <tr key={r.check_id}>
                      <td><LevelPill level={r.result} hard={r.result === "blocker"} /></td>
                      <td>{r.title}</td>
                      <td className="wrapany">{r.detail?.message}</td>
                      <td>{r.detail?.evidence && <button className="ghost" onClick={() => setOpen(r)}>Evidence</button>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
        );
      })}
      {open && <Dialog title={open.title} wide onClose={() => setOpen(null)}><p className="wrapany">{open.detail.message}</p><pre className="cmd">{JSON.stringify(open.detail.evidence, null, 2)}</pre></Dialog>}
    </div>
  );
}

export function ReportTab() {
  const { id, view } = useMig();
  const m = view.data!.migration;
  const [n, setN] = useState(0);
  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="row">
        <span className="muted" style={{ flex: 1 }}>The report is the shareable record: permission, timings, every database, verification and the verdict, accepted risks and operator actions.</span>
        <button className="ghost" onClick={() => setN((x) => x + 1)}>Refresh</button>
        <a className="btn" href={`/api/v1/migrations/${id}/report?format=html&download=1`} download>Download HTML</a>
        <a className="btn" href={`/api/v1/migrations/${id}/report?format=md&download=1`} download>Download Markdown</a>
        <span className="row small muted" style={{ gap: 6 }}>PDF <ComingSoon what="PDF reports" /></span>
      </div>
      <iframe key={n} title={`Report for ${m.name}`} src={`/api/v1/migrations/${id}/report?format=html`} style={{ width: "100%", height: "calc(100vh - 300px)", minHeight: 480, border: "1px solid var(--border)", borderRadius: 6, background: "#fff" }} />
    </div>
  );
}
