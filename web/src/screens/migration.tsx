import { createContext, useContext, useEffect, useRef, useState } from "react";
import { Link, NavLink, Outlet, useNavigate, useParams } from "react-router-dom";
import { api } from "../lib/api";
import { copyEtaMs, dbCopyFraction, lifecycleIndex, overallProgress, rateFrom, readyForCutover } from "../lib/derive";
import { ago, bytes, count, duration, pct, rate, time } from "../lib/format";
import { Loaded, useApi, useNow, useStream } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Alert, EventRow, MigrationView, Series } from "../lib/types";
import { Sparkline } from "../components/charts";
import { useOps } from "../components/ops";
import { DataTable } from "../components/table";
import { MIG_TABS } from "../components/shell";
import { Card, ConfirmDialog, Empty, ErrorState, Pill, ProgressBar, Skeleton, StaleBadge, Tile, Trunc, useAction } from "../components/ui";
import { Stepper } from "./home";

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
              {m.flags.verdict && <Pill state={m.flags.verdict} text={`Verdict ${m.flags.verdict}`} />}
              {m.fixture && <span className="pill neutral nodot">Demonstration data</span>}
              <span>{v.included} databases · {bytes(v.total_bytes)}</span>
              {m.started_at && <span>Started {time(m.started_at)}</span>}
              <span>{connected ? "Live" : "Reconnecting…"}</span>
              <StaleBadge updatedAt={view.updatedAt} now={now} />
            </div>
          </div>
          <MigrationActions v={v} refresh={view.refresh} />
        </div>
        <nav className="tabs" aria-label="Migration sections">
          {MIG_TABS.map((t) => <NavLink key={t.to} className="tab" end={t.to === ""} to={`/migrations/${m.short_id}${t.to ? "/" + t.to : ""}`}>{t.label}</NavLink>)}
        </nav>
        <div className={stale ? "stale" : ""}><Outlet /></div>
      </div>
    </Ctx.Provider>
  );
}

function MigrationActions({ v, refresh }: { v: MigrationView; refresh: () => void }) {
  const { can } = useSession();
  const { run, busy } = useAction();
  const ops = useOps();
  const nav = useNavigate();
  const [dlg, setDlg] = useState<"abort" | "cleanup" | "delete" | "pause" | null>(null);
  const m = v.migration, f = m.flags;
  if (!can("operator")) return null;
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
  const backlog = inc.reduce((a, d) => a + (d.backlog_bytes || 0), 0);
  return (
    <div className="stack" style={{ gap: 24 }}>
      <Card><Stepper index={lifecycleIndex(v)} bad={m.state === "needs_attention"} /></Card>
      {m.state === "needs_attention" && (
        <div className="callout crit" role="alert"><div className="body"><strong>Needs attention</strong><span>{inc.filter((d) => ["failed", "degraded", "restart_required"].includes(d.state)).map((d) => `${d.source_name}: ${d.last_error || d.state}`).join(" · ") || "Open a database below to see what happened."}</span></div></div>
      )}
      <div className="grid cols-4">
        <Tile label="Base copy" value={pct(prog * 100)} sub={<ProgressBar value={prog} label="Overall base copy progress" />} />
        <Tile label="Copy ETA" value={eta ? duration(eta) : prog >= 1 ? "Done" : m.flags.started ? "Measuring" : "Not started"} sub={eta ? `around ${time(now + eta, false)}` : "From target growth over 5 minutes"} />
        <Tile label="In sync" value={`${inSync} of ${v.included}`} sub={readyForCutover(v) ? <Link to="cutover">Ready for cutover</Link> : "Cutover needs every database in sync"} />
        <Tile label="Total backlog" value={bytes(backlog)} title={`${backlog.toLocaleString("en-US")} bytes`} sub={`Source WAL ${rate(v.cluster?.src_wal_rate)}`} />
      </div>
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
            { key: "backlog", label: "Backlog", num: true, sort: (d) => d.backlog_bytes ?? -1, render: (d) => d.backlog_bytes === undefined || d.backlog_bytes === null ? "—" : <span title={`${d.backlog_bytes.toLocaleString("en-US")} bytes`}>{bytes(d.backlog_bytes)}</span> },
            { key: "trend", label: "Trend", optional: true, render: (d) => <Sparkline points={last(metrics.data, "backlog_bytes", d.source_name).map((p) => p.v).slice(-60)} label={`${d.source_name} backlog trend`} /> },
            { key: "replay", label: "Replay", num: true, optional: true, sort: (d) => v.live?.[d.source_name]?.replay_rate ?? -1, render: (d) => rate(v.live?.[d.source_name]?.replay_rate) },
            { key: "slot", label: "Slot", optional: true, sort: (d) => v.live?.[d.source_name]?.slot_retained_bytes ?? -1, render: (d) => { const l = v.live?.[d.source_name] || {}; const ws = ["reserved", "extended", "unreserved", "lost"][l.slot_wal_status ?? -1]; return l.slot_retained_bytes === undefined ? <span className="faint">—</span> : <span className="small" title={`WAL status ${ws || "unknown"}; headroom ${bytes(l.slot_headroom_bytes)}`}>{bytes(l.slot_retained_bytes)} held{ws && ws !== "reserved" ? ` · ${ws}` : ""}</span>; } },
            { key: "hb", label: "Heartbeat", num: true, optional: true, sort: (d) => v.live?.[d.source_name]?.heartbeat_latency_s ?? -1, render: (d) => { const h = v.live?.[d.source_name]?.heartbeat_latency_s; return h === undefined ? "—" : duration(h * 1000); } },
          ]} />
      </Card>
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
            <span className="wrapany" style={{ flex: 1, minWidth: 200 }}>{e.message}</span>
          </div>
        </li>
      ))}
    </ul>
  );
}

