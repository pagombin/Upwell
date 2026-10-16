import { createContext, useContext, useEffect, useRef, useState } from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate, useParams } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import { backlogKnown, catchUps, copyEtaMs, dbCopyEtaMs, dbCopyFraction, lifecycleBad, lifecycleIndex, overallProgress, rateFrom, readyForCutover } from "../lib/derive";
import { ago, bytes, count, duration, pct, rate, time } from "../lib/format";
import { type Loaded, useApi, useNow, useStream } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Alert, Connection, EventRow, MigrationView, Series } from "../lib/types";
import { Sparkline } from "../components/charts";
import { useOps } from "../components/ops";
import { DataTable } from "../components/table";
import { MIG_TABS } from "../components/shell";
import { Card, ConfirmDialog, Dialog, Empty, ErrorState, Field, LongText, Pill, ProgressBar, Skeleton, StaleBadge, Tile, Trunc, useAction, useToast } from "../components/ui";
import { Stepper } from "./home";
import { lagOf, safetySignals, sampleAge, SafetyStrip, StatusSummary, statusSummary, TrendsCard } from "../components/overview";

interface MigCtx { view: Loaded<MigrationView>; id: string; metrics: Loaded<Series[]>; lastEvent: number }
const Ctx = createContext<MigCtx>(null as unknown as MigCtx);
export function useMig() { return useContext(Ctx); }

// Metrics window for overview sparklines and forecasts, aligned to the minute
// so the URL (and so the fetch) changes at most once a minute.
function useWindow(minutes: number) {
  const now = useNow(60000);
  return Math.floor(now / 60000) * 60000 - minutes * 60000;
}

export function MigrationLayout() {
  const { id = "" } = useParams();
  const location = useLocation();
  const view = useApi<MigrationView>(`/api/v1/migrations/${id}`, 10000);
  const from = useWindow(30);
  const metrics = useApi<Series[]>(`/api/v1/migrations/${id}/metrics?names=backlog_bytes,replay_rate,target_size_bytes,slot_retained_bytes,slot_headroom_bytes,heartbeat_latency_s&from=${from}`, 30000);
  const [lastEvent, setLastEvent] = useState(0);
  const pending = useRef<number | null>(null);
  const { connected } = useStream(view.data ? `/api/v1/stream/migrations/${view.data.migration.id}` : null, (e) => {
    setLastEvent(Date.now());
    if (e.type === "metrics") { metrics.refresh(); }
    if (pending.current === null) pending.current = window.setTimeout(() => { pending.current = null; view.refresh(); }, 1000);
  });
  useEffect(() => () => { if (pending.current) clearTimeout(pending.current); }, []);
  const now = useNow(5000);
  if (view.error && !view.data) return <div className="page"><ErrorState error={view.error} retry={view.refresh} /></div>;
  if (!view.data) return <div className="page"><Skeleton lines={2} height={24} /><Skeleton lines={8} /></div>;
  const v = view.data, m = v.migration;
  const stale = !!view.updatedAt && now - view.updatedAt > 30000;
  return (
    <Ctx.Provider value={{ view, id: m.id, metrics, lastEvent }}>
      <div className="page">
        <div className="pagehead">
          <div className="titles">
            <h1><Trunc>{m.name}</Trunc></h1>
            <div className="row small muted">
              <span className="mono">{m.short_id}</span><Pill state={m.state} />
              {m.flags.verdict && <Pill state={m.flags.verdict === "GO" && (m.flags.unverified_count || 0) > 0 ? undefined : m.flags.verdict} kind={m.flags.verdict === "GO" && (m.flags.unverified_count || 0) > 0 ? "warn" : undefined} text={m.flags.verdict === "GO" && (m.flags.unverified_count || 0) > 0 ? `Verdict GO, ${m.flags.unverified_count} table(s) not fully verified` : `Verdict ${m.flags.verdict}`} />}
              {m.fixture && <span className="pill neutral nodot">Demonstration data</span>}
              <span>{v.included} databases · {bytes(v.total_bytes)}</span>
              {m.started_at && <span>Started {time(m.started_at)}</span>}
              <span>{connected ? "Live" : "Reconnecting…"}</span>
              <StaleBadge updatedAt={view.updatedAt} now={now} />
            </div>
            {(v.source || v.target) && (
              <div className="row small muted hosts" aria-label="Clusters">
                <span>Source <Trunc className="mono">{v.source ? `${v.source.host}:${v.source.port}` : "—"}</Trunc></span>
                <span aria-hidden="true">→</span>
                <span>Target <Trunc className="mono">{v.target ? `${v.target.host}:${v.target.port}` : "—"}</Trunc></span>
                {m.flags.freeze && m.flags.freeze.state !== "undone" && <span className="pill warn">Source writes frozen</span>}
              </div>
            )}
          </div>
          <MigrationActions v={v} refresh={view.refresh} />
        </div>
        <nav className="tabs" aria-label="Migration sections">
          {MIG_TABS.map((t) => <NavLink key={t.to} className="tab" end={t.to === ""} to={`/migrations/${m.short_id}${t.to ? "/" + t.to : ""}`}>{t.label}</NavLink>)}
        </nav>
        {m.state === "needs_attention" && !location.pathname.endsWith(`/${m.short_id}`) && (
          <div className="callout crit" role="alert" style={{ marginBottom: 16 }}><div className="body"><strong>Needs attention</strong><span>{v.databases.filter((d) => d.include && ["failed", "degraded", "restart_required"].includes(d.state)).map((d) => `${d.source_name}: ${d.last_error || d.state}`).join(" · ") || "Open the overview to see what happened."}</span></div></div>
        )}
        <div className={stale ? "stale" : ""}><Outlet /></div>
      </div>
    </Ctx.Provider>
  );
}

// Engines run in these states; an address change waits for them (D100).
const ENGINE_STATES = ["preparing", "base_copy", "catch_up", "in_sync", "draining", "degraded"];

// ConnectionsDialog changes a started migration's source or target
// connection within the rules the server enforces (D100): a password at any
// time, another address of the same cluster only with no engine running.
function ConnectionsDialog({ v, onClose, onDone }: { v: MigrationView; onClose: () => void; onDone: () => void }) {
  const [kind, setKind] = useState<"source" | "target">("source");
  const c: Connection | undefined = kind === "source" ? v.source : v.target;
  const init = (c?: Connection) => ({ host: c?.host || "", port: c?.port || 25060, user: c?.user || "doadmin", password: "", dbname: c?.dbname || "defaultdb", sslmode: c?.sslmode || "require", ca_cert: c?.ca_cert || "", storage_gb: c?.storage_gb || 0 });
  const [f, setF] = useState(() => init(c));
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const m = v.migration;
  const running = v.databases.filter((d) => d.include && ENGINE_STATES.includes(d.state));
  const inCutover = !!m.flags.cutover && !m.flags.cutover.ended_at;
  const pick = (k: "source" | "target") => { setKind(k); setF(init(k === "source" ? v.source : v.target)); setErr(undefined); };
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) => setF({ ...f, [k]: e.target.value });
  const endpoint = !!c && (f.host.trim() !== c.host || Number(f.port) !== c.port || f.user !== c.user || f.dbname !== c.dbname || f.sslmode !== c.sslmode || f.ca_cert.trim() !== (c.ca_cert || "").trim());
  const changed = endpoint || !!f.password || (Number(f.storage_gb) || 0) !== (c?.storage_gb || 0);
  const blocked = endpoint && (running.length > 0 || inCutover);
  const save = async () => {
    setBusy(true); setErr(undefined);
    try {
      await api.put(`/api/v1/migrations/${m.id}/connections/${kind}`, { ...f, host: f.host.trim(), port: Number(f.port), storage_gb: Number(f.storage_gb) || 0 });
      toast("ok", `${kind === "source" ? "Source" : "Target"} connection changed`);
      onDone(); onClose();
    } catch (e) { setErr(e as ApiError); } finally { setBusy(false); }
  };
  return (
    <Dialog title="Change a connection" onClose={onClose} wide footer={<>
      <button onClick={onClose}>Cancel</button>
      <button className="primary" onClick={save} disabled={busy || !changed || blocked || !f.host.trim()}>{busy && <span className="spin" />}Save</button>
    </>}>
      <div className="stack">
        <div className="row" role="radiogroup" aria-label="Connection">
          <label className="row small"><input type="radio" checked={kind === "source"} onChange={() => pick("source")} />Source</label>
          <label className="row small"><input type="radio" checked={kind === "target"} onChange={() => pick("target")} />Target</label>
        </div>
        <div className="callout info"><div className="body">
          <strong>What can change now</strong>
          <span>A new password can change at any time; Upwell checks that it works first. Running engines keep their sessions, and their next connection uses it.{kind === "target" ? " So can the target storage size." : ""}</span>
          <span>The host, port, user, database, TLS mode or CA can change only while no engine runs, and only to another address of the same cluster, for example the private hostname instead of the public one, or a connection pool. Upwell checks that it reaches the same cluster. A different cluster needs a new migration.</span>
          {running.length > 0 && <span>Running now: {running.map((d) => d.source_name).join(", ")}. Stop them on the Databases tab, or pause the migration, to change the address.</span>}
          {inCutover && <span>A cutover is in progress: only the password can change until it ends.</span>}
        </div></div>
        <div className="fieldgrid">
          <Field label="Host"><input value={f.host} onChange={set("host")} spellCheck={false} /></Field>
          <Field label="Port"><input type="number" value={f.port} onChange={set("port")} /></Field>
          <Field label="User"><input value={f.user} onChange={set("user")} autoComplete="off" /></Field>
          <Field label="New password" hint="Leave empty to keep the stored one."><input type="password" value={f.password} onChange={set("password")} autoComplete="new-password" /></Field>
          <Field label="Maintenance database"><input value={f.dbname} onChange={set("dbname")} /></Field>
          <Field label="TLS mode">
            <select value={f.sslmode} onChange={set("sslmode")}>
              <option value="verify-full">verify-full (needs the CA certificate)</option><option value="require">require</option><option value="prefer">prefer</option><option value="disable">disable (local testing only)</option>
            </select>
          </Field>
          {kind === "target" && <Field label="Target storage (GB)"><input type="number" value={f.storage_gb} onChange={set("storage_gb")} /></Field>}
        </div>
        {f.sslmode === "verify-full" && <Field label="CA certificate (PEM)"><textarea className="mono" value={f.ca_cert} onChange={set("ca_cert")} spellCheck={false} /></Field>}
        {blocked && <div className="callout warn" role="status"><div className="body">This changes where Upwell connects, which cannot happen while {inCutover ? "a cutover is in progress" : "an engine runs"}. Only a password change can be saved now.</div></div>}
        {err && <ErrorState error={err} />}
      </div>
    </Dialog>
  );
}

function MigrationActions({ v, refresh }: { v: MigrationView; refresh: () => void }) {
  const { can } = useSession();
  const { run, busy } = useAction();
  const ops = useOps();
  const nav = useNavigate();
  const [dlg, setDlg] = useState<"abort" | "cleanup" | "delete" | "pause" | "bundle" | "conn" | null>(null);
  const m = v.migration, f = m.flags;
  if (!can("operator")) return null;
  if (m.fixture) return <div className="actions"><span className="small muted">Demonstration data: actions are turned off</span></div>;
  const inCutover = !!f.cutover && !f.cutover.ended_at;
  const mid = m.id;
  return (
    <div className="actions">
      {!f.started && !f.cleaned_up && <Link className="btn" to={`/migrations/${m.short_id}/setup`}>Continue setup</Link>}
      {f.started && !f.paused && !f.aborted && !f.verdict && !inCutover && <button onClick={() => setDlg("pause")}>Pause</button>}
      {f.started && f.paused && !f.aborted && <button className="primary" disabled={!!busy} onClick={() => run("resume", () => api.post(`/api/v1/migrations/${mid}/resume`), "Resuming every database").then(refresh)}>Resume</button>}
      {f.started && !f.aborted && !f.verdict && !inCutover && <button className="danger" onClick={() => setDlg("abort")}>Abort</button>}
      {(f.aborted || f.verdict || !f.started) && !f.cleaned_up && !f.cleanup_running && <button onClick={() => setDlg("cleanup")}>Clean up</button>}
      {f.cleaned_up && <button className="danger" onClick={() => setDlg("delete")}>Delete</button>}
      {f.started && !f.aborted && !f.cleaned_up && !f.verdict && <button className="ghost" onClick={() => setDlg("conn")}>Connections</button>}
      <button className="ghost" onClick={() => setDlg("bundle")}>Support bundle</button>
      {dlg === "conn" && <ConnectionsDialog v={v} onClose={() => setDlg(null)} onDone={refresh} />}
      {dlg === "bundle" && <SupportBundleDialog mig={mid} onClose={() => setDlg(null)} />}
      {dlg === "pause" && (
        <ConfirmDialog title="Pause every database" confirmLabel="Pause" onClose={() => setDlg(null)} onConfirm={async () => { await api.post(`/api/v1/migrations/${mid}/pause`); refresh(); }}>
          <p>Every running engine stops within about 10 seconds. Replication slots stay on the source, so WAL accumulates there until you resume; watch the slot headroom.</p>
        </ConfirmDialog>
      )}
      {dlg === "abort" && (
        <ConfirmDialog title="Abort this migration" danger typeToConfirm={m.name} confirmLabel="Abort migration" onClose={() => setDlg(null)}
          onConfirm={async (t) => { await api.post(`/api/v1/migrations/${mid}/abort`, { confirm: t }); refresh(); }}>
          <p>Every engine stops and the migration ends without a verdict. Slots, publications and origins stay until you clean up. Data already copied stays on the target. This cannot be resumed.</p>
        </ConfirmDialog>
      )}
      {dlg === "cleanup" && (
        <ConfirmDialog title="Clean up" danger typeToConfirm={m.name} confirmLabel="Clean up" onClose={() => setDlg(null)}
          onConfirm={async (t) => { const r = await api.post<{ operation_id: string }>(`/api/v1/migrations/${mid}/cleanup`, { confirm: t }); ops.open(r.operation_id, "Clean up"); refresh(); }}>
          <p>Removes from the customer's clusters: the replication slot and publication of each database on the source, the replication origin on the target, and the <span className="mono">upwell</span> heartbeat schema. Also deletes the engine work directories and the stored connection passwords on this droplet.</p>
          <p>Migrated data on the target is not touched.</p>
        </ConfirmDialog>
      )}
      {dlg === "delete" && (
        <ConfirmDialog title="Delete this migration" danger typeToConfirm={m.name} confirmLabel="Delete" onClose={() => setDlg(null)}
          onConfirm={async () => { await api.del(`/api/v1/migrations/${mid}`); nav("/"); }}>
          <p>Removes the migration, its history, metrics and logs from Upwell. Download the report first if you need it. The audit log keeps its entries.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}

function last(s: Series[] | undefined, name: string, db: string) {
  return s?.find((x) => x.name === name && (x.database || "") === db)?.points || [];
}

export function OverviewTab() {
  const { view, metrics, id } = useMig();
  const v = view.data!;
  const m = v.migration;
  const nav = useNavigate();
  const now = useNow(5000);
  const alerts = useApi<Alert[]>(`/api/v1/alerts`, 15000);
  const events = useApi<EventRow[]>(`/api/v1/migrations/${id}/events?limit=12`, 10000);
  // Trends over the run (at most the last 24 hours), aligned to the minute.
  const runFrom = Math.floor(Math.max(m.started_at || now - 3600e3, now - 24 * 3600e3) / 60000) * 60000;
  const trends = useApi<Series[]>(`/api/v1/migrations/${id}/metrics?names=backlog_bytes,heartbeat_latency_s,replay_rate,src_wal_rate&from=${runFrom}`, 60000);
  const prog = overallProgress(v);
  const eta = copyEtaMs(v, metrics.data);
  const open = (alerts.data || []).filter((a) => a.migration_id === m.id && a.state !== "resolved");
  const inc = v.databases.filter((d) => d.include);
  // Forecast: slot headroom exhaustion at the current retained-WAL growth.
  const forecasts = inc.map((d) => {
    const pts = last(metrics.data, "slot_retained_bytes", d.source_name);
    const r = rateFrom(pts, 10 * 60e3);
    const head = v.live?.[d.source_name]?.slot_headroom_bytes;
    return { db: d.source_name, growth: r, hoursLeft: r && r > 0 && head ? head / r / 3600 : undefined };
  }).filter((f) => f.hoursLeft !== undefined).sort((a, b) => a.hoursLeft! - b.hoursLeft!);
  const inSync = v.state_counts["in_sync"] || 0;
  const backlog = inc.reduce((a, d) => a + (backlogKnown(d) || 0), 0);
  const summary = statusSummary(v, metrics.data, now);
  const catchup = catchUps(v, metrics.data);
  const worst = catchup[0];
  const signals = safetySignals(v, now);
  return (
    <div className="stack" style={{ gap: 24 }}>
      <StatusSummary s={summary} />
      <SafetyStrip signals={signals} />
      {m.state === "needs_attention" && (
        <div className="callout crit" role="alert"><div className="body"><strong>Why</strong><span className="wrapany">{inc.filter((d) => ["failed", "degraded", "restart_required"].includes(d.state)).map((d) => `${d.source_name}: ${d.last_error || d.state}`).join(" · ") || "Open a database below to see what happened."}</span></div></div>
      )}
      <Card><Stepper index={lifecycleIndex(v)} bad={lifecycleBad(v)} /></Card>
      <div className="grid cols-4">
        <Tile label="Base copy" value={pct(prog * 100)} sub={<ProgressBar value={prog} label="Overall base copy progress" />} />
        <Tile label="Copy ETA" value={eta ? duration(eta) : prog >= 1 ? "Done" : m.flags.started ? "Measuring" : "Not started"} sub={eta ? `around ${time(now + eta, false)}` : "From target growth over 5 minutes"} />
        <Tile label="In sync" value={`${inSync} of ${v.included}`} sub={readyForCutover(v) ? <Link to="cutover">Ready for cutover</Link> : "Cutover needs every database in sync"} />
        <Tile label="Total backlog" value={bytes(backlog)} title={`${backlog.toLocaleString("en-US")} bytes`} sub={worst ? (worst.growing ? `Growing ${rate(-(worst.drain || 0))} (${worst.db})` : worst.etaMs ? `Draining ${rate(worst.drain)}: about ${duration(worst.etaMs)}` : `Source WAL ${rate(v.cluster?.src_wal_rate)}`) : `Source WAL ${rate(v.cluster?.src_wal_rate)}`} />
      </div>
      {catchup.filter((c) => c.restartFaster || c.growing).map((c) => (
        <div key={c.db} className="callout warn" role="status"><div className="body">
          <strong>{c.db}: {c.growing ? "the backlog is growing" : `catching up will take about ${duration(c.etaMs)}`}</strong>
          <span>{bytes(c.backlog)} of changes wait to be applied, {c.growing ? `and the backlog grew by ${rate(-(c.drain || 0))} over the last 15 minutes: the engine applies changes more slowly than the source writes them.` : `draining at ${rate(c.drain)}.`}{c.baseCopyMs ? ` Its last base copy took ${duration(c.baseCopyMs)}.` : ""}</span>
          {c.restartFaster && <span>Restarting {c.db} from zero (open it below, then Restart from zero) would likely be faster. The new copy starts from a fresh snapshot, but it will fall behind again if the source keeps writing at this rate.</span>}
        </div></div>
      ))}
      <Card title="Databases">
        <DataTable label="Databases" rows={inc} rowKey={(d) => d.id} csvName={`upwell-${m.short_id}-databases`}
          filterText={(d) => d.source_name + " " + d.state}
          onRow={(d) => nav(`/migrations/${m.short_id}/databases/${encodeURIComponent(d.source_name)}`)}
          emptyTitle="No databases included" emptyHint="Choose databases in the setup wizard."
          initialSort={{ key: "size", dir: -1 }}
          columns={[
            { key: "name", label: "Database", width: 220, sort: (d) => d.source_name, render: (d) => <Link to={`/migrations/${m.short_id}/databases/${encodeURIComponent(d.source_name)}`} onClick={(e) => e.stopPropagation()}><Trunc className="mono">{d.source_name}</Trunc></Link> },
            { key: "phase", label: "Phase", sort: (d) => d.state, render: (d) => <span className="row" style={{ gap: 6, flexWrap: "nowrap" }}><Pill state={d.state} />{d.retry_count > 0 && <span className="small faint" title="Automatic retries">×{d.retry_count}</span>}</span>, csv: (d) => d.state },
            { key: "size", label: "Size", num: true, sort: (d) => d.size_bytes, render: (d) => <span title={`${d.size_bytes.toLocaleString("en-US")} bytes`}>{bytes(d.size_bytes)}</span> },
            { key: "progress", label: "Copied", width: 130, sort: (d) => dbCopyFraction(d, v.live?.[d.source_name]), render: (d) => { const f = dbCopyFraction(d, v.live?.[d.source_name]); return <span className="row" style={{ gap: 6, flexWrap: "nowrap" }}><ProgressBar value={f} label={`${d.source_name} copied`} /><span className="small" style={{ width: 36, textAlign: "right" }}>{pct(f * 100)}</span></span>; } },
            { key: "rows", label: "Rows", num: true, optional: true, sort: (d) => d.rows_estimate, render: (d) => <span title={d.rows_estimate.toLocaleString("en-US")}>{count(d.rows_estimate)}</span> },
            { key: "eta", label: "Copy ETA", num: true, sort: (d) => dbCopyEtaMs(d, v.live?.[d.source_name], metrics.data) ?? (d.state === "base_copy" ? Infinity : -1), render: (d) => { const e = dbCopyEtaMs(d, v.live?.[d.source_name], metrics.data); return e ? <span title={`around ${time(Date.now() + e, false)}`}>{duration(e)}</span> : d.state === "base_copy" ? <span className="muted">Measuring</span> : "—"; } },
            { key: "behind", label: "Behind", num: true, sort: (d) => lagOf(v, d) ?? -1, render: (d) => { const l = lagOf(v, d); return l === undefined ? <span className="faint">—</span> : <span className={l > 300 ? "crit-text" : l > 60 ? "warn-text" : ""} title="Age of the last heartbeat on the target">{duration(l * 1000)}</span>; } },
            { key: "updated", label: "Updated", num: true, sort: (d) => sampleAge(v, d, now) ?? Infinity, render: (d) => { const a = sampleAge(v, d, now); return a === undefined ? <span className="faint">—</span> : <span className={a > 60000 ? "warn-text" : "muted"} title={`Last sampled ${time(now - a)}`}>{duration(a)} ago</span>; } },
            { key: "backlog", label: "Backlog", num: true, sort: (d) => backlogKnown(d) ?? -1, render: (d) => { const b = backlogKnown(d); return b === undefined ? "—" : <span title={`${b.toLocaleString("en-US")} bytes`}>{bytes(b)}</span>; } },
            { key: "trend", label: "Trend", optional: true, render: (d) => <Sparkline points={last(metrics.data, "backlog_bytes", d.source_name).map((p) => p.v).slice(-60)} label={`${d.source_name} backlog trend`} /> },
            { key: "replay", label: "Replay", num: true, optional: true, sort: (d) => v.live?.[d.source_name]?.replay_rate ?? -1, render: (d) => rate(v.live?.[d.source_name]?.replay_rate) },
            { key: "slot", label: "Slot", optional: true, sort: (d) => v.live?.[d.source_name]?.slot_retained_bytes ?? -1, render: (d) => { const l = v.live?.[d.source_name] || {}; const ws = ["reserved", "extended", "unreserved", "lost"][l.slot_wal_status ?? -1]; return l.slot_retained_bytes === undefined ? <span className="faint">—</span> : <span className="small" title={`WAL status ${ws || "unknown"}; headroom ${bytes(l.slot_headroom_bytes)}`}>{bytes(l.slot_retained_bytes)} held{ws && ws !== "reserved" ? ` · ${ws}` : ""}</span>; } },
            { key: "hb", label: "Heartbeat", num: true, optional: true, sort: (d) => v.live?.[d.source_name]?.heartbeat_latency_s ?? -1, render: (d) => { const h = v.live?.[d.source_name]?.heartbeat_latency_s; return h === undefined ? "—" : duration(h * 1000); } },
          ]} />
      </Card>
      <TrendsCard metrics={trends.data} from={runFrom} to={now} />
      <div className="grid cols-2">
        <Card title="Forecasts">
          {forecasts.length === 0 ? <p className="muted">No slot is growing toward its limit. Forecasts appear when retained WAL grows for 10 minutes.</p> : (
            <ul className="stack" style={{ margin: 0, paddingLeft: 18 }}>
              {forecasts.slice(0, 5).map((f) => <li key={f.db}><span className="mono">{f.db}</span>: slot headroom runs out in about {duration(f.hoursLeft! * 3600e3)} at {rate(f.growth)}.</li>)}
            </ul>
          )}
        </Card>
        <Card title="Open alerts" actions={<Link to="/alerts">All alerts</Link>}>
          {!alerts.data ? <Skeleton /> : open.length === 0 ? <p className="muted">No alerts are open for this migration.</p> : (
            <ul className="stack" style={{ margin: 0, padding: 0, listStyle: "none" }}>
              {open.slice(0, 6).map((a) => <li key={a.id} className={`sev-stripe ${a.severity === "critical" ? "crit" : "warn"}`} style={{ paddingLeft: 10 }}><div className="wrapany">{a.message}</div><div className="small muted">{a.state} · since {ago(a.first_at, now)}</div></li>)}
            </ul>
          )}
        </Card>
      </div>
      <Card title="Recent events" actions={<Link to="events">Timeline</Link>}>
        {events.error ? <ErrorState error={events.error} retry={events.refresh} /> : !events.data ? <Skeleton lines={5} /> : events.data.length === 0 ? <Empty title="No events yet">Phase changes, operations and alerts appear here.</Empty> : <EventList rows={events.data} now={now} />}
      </Card>
    </div>
  );
}

export function EventList({ rows, now }: { rows: EventRow[]; now: number }) {
  return (
    <ul className="stack" style={{ margin: 0, padding: 0, listStyle: "none", gap: 8 }}>
      {rows.map((e) => (
        <li key={e.id} className={`sev-stripe ${e.severity === "critical" || e.severity === "error" ? "crit" : e.severity === "warning" ? "warn" : "info"}`} style={{ paddingLeft: 10 }}>
          <div className="row" style={{ gap: 8 }}>
            <span className="small muted" title={time(e.ts)} style={{ width: 90 }}>{ago(e.ts, now)}</span>
            {e.database && <span className="mono small" style={{ maxWidth: 220 }}><Trunc>{e.database}</Trunc></span>}
            <span style={{ flex: 1, minWidth: 200 }}><LongText text={e.message} /></span>
          </div>
        </li>
      ))}
    </ul>
  );
}


interface BundlePreview { files: { name: string; size: number; redactions: number }[]; total_size: number; redaction_rules: string[] }

// The support bundle (overnight item 6): what it holds, previewed before download.
function SupportBundleDialog({ mig, onClose }: { mig: string; onClose: () => void }) {
  // D101: network captures limited to this migration's failures by default.
  const [net, setNet] = useState<"" | "full" | "none">("");
  const q = net ? `?network=${net}` : "";
  const pv = useApi<BundlePreview>(`/api/v1/migrations/${mig}/support-bundle${q}`);
  return (
    <Dialog title="Support bundle" wide onClose={onClose} footer={<><button onClick={onClose}>Close</button><a className="btn primary" href={`/api/v1/migrations/${mig}/support-bundle/download${q}`} download>Download .tar.gz</a></>}>
      <p>A .tar.gz for support: the migration's settings and connections (without passwords), databases, preflight, verification, operations, events, alerts, 24 hours of metrics, the report, Upwell's and the engine's logs, network diagnostics, versions and host facts. Downloading it is recorded in the audit log.</p>
      <label className="field" style={{ maxWidth: 520 }}>
        <span className="lbl">Network captures</span>
        <select value={net} onChange={(e) => setNet(e.target.value as "" | "full" | "none")}>
          <option value="">Around this migration's failures, its clusters only (smallest)</option>
          <option value="full">Everything from the last 48 hours, every cluster (up to 40 MB)</option>
          <option value="none">None: incident reports only</option>
        </select>
      </label>
      {pv.error ? <ErrorState error={pv.error} retry={pv.refresh} /> : !pv.data ? <Skeleton lines={6} /> : <>
        <div className="tablewrap"><table className="data" aria-label="Support bundle files">
          <thead><tr><th>File</th><th>Size</th><th>Values removed</th></tr></thead>
          <tbody>{pv.data.files.map((f) => <tr key={f.name}><td className="mono">{f.name}</td><td>{bytes(f.size)}</td><td>{f.redactions || "—"}</td></tr>)}</tbody>
        </table></div>
        <p className="small muted">Total {bytes(pv.data.total_size)}. Removed before anything is written: {pv.data.redaction_rules.join("; ")}.</p>
      </>}
    </Dialog>
  );
}
