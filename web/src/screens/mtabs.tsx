import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api } from "../lib/api";
import { ago, bytes, count, duration, rate, time } from "../lib/format";
import { backlogKnown } from "../lib/derive";
import { useApi, useDebounced, useNow } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Database, EventRow, Migration, Series, SnapshotWait } from "../lib/types";
import { blockerText } from "../components/overview";
import { CrosshairProvider, seriesFor, TimeChart } from "../components/charts";
import { LogViewer } from "../components/logview";
import { Card, ConfirmDialog, Dialog, Empty, ErrorState, Pill, ProgressBar, Skeleton, Tile, Trunc, useAction } from "../components/ui";
import { EventList, useMig } from "./migration";

const RANGES = [{ m: 30, l: "30 minutes" }, { m: 180, l: "3 hours" }, { m: 720, l: "12 hours" }, { m: 2880, l: "2 days" }, { m: 10080, l: "7 days" }];

function useRange(def = 30) {
  const [mins, setMins] = useState(def);
  const now = useNow(30000);
  const from = Math.floor(now / 30000) * 30000 - mins * 60000;
  const picker = (
    <label className="row small" style={{ gap: 6 }}>Range
      <select value={mins} onChange={(e) => setMins(Number(e.target.value))}>{RANGES.map((r) => <option key={r.m} value={r.m}>{r.l}</option>)}</select>
    </label>
  );
  return { from, to: Math.floor(now / 30000) * 30000, picker };
}

interface Attempt { number: number; kind: string; unit: string; command: string; started_at: number; ended_at?: number; exit_code?: number; end_reason: string }
interface DBDetail { migration: Migration; database: Database; attempts: Attempt[]; live: Record<string, number>; unit: { active: boolean; main_pid?: number; main_live?: boolean; exit_code?: number } | null; unit_name: string; now: number; snapshot_wait?: SnapshotWait; copy_progress?: { copies: { table: string; bytes: number; bytes_total: number; tuples: number; rows_estimate?: number; fraction?: number; source_bytes?: number; parts?: number; target_bytes?: number }[]; indexes: { index: string; phase: string; percent: number }[]; at?: number; error?: string; pending?: boolean } }

export function DatabaseScreen() {
  const { db = "" } = useParams();
  const { view, id } = useMig();
  const name = decodeURIComponent(db);
  const d = useApi<DBDetail>(`/api/v1/migrations/${id}/databases/${encodeURIComponent(name)}`, 5000);
  const { from, to, picker } = useRange();
  const mx = useApi<Series[]>(`/api/v1/migrations/${id}/metrics?names=backlog_bytes,replay_rate,slot_retained_bytes,slot_headroom_bytes,heartbeat_latency_s,target_size_bytes&from=${from}&to=${to}`, 30000);
  const { can } = useSession();
  const { run, busy } = useAction();
  const [restart, setRestart] = useState(false);
  const [att, setAtt] = useState("");
  const now = useNow(5000);
  if (d.error) return <ErrorState error={d.error} retry={d.refresh} />;
  if (!d.data) return <Skeleton lines={8} />;
  const x = d.data.database, live = d.data.live || {};
  const short = view.data!.migration.short_id;
  const stoppable = ["preparing", "base_copy", "catch_up", "in_sync", "degraded"].includes(x.state);
  const resumable = ["stopped", "failed"].includes(x.state);
  const restartable = view.data!.migration.flags.started && !view.data!.migration.flags.verdict && !["draining", "drained", "verified"].includes(x.state);
  const ws = ["reserved", "extended", "unreserved", "lost"][live.slot_wal_status ?? -1];
  return (
    <div className="stack" style={{ gap: 24 }}>
      <div className="row">
        <Link to={`/migrations/${short}`}>← All databases</Link>
        <h2 style={{ flex: 1, minWidth: 0 }}><Trunc className="mono">{x.source_name}</Trunc></h2>
        <Pill state={x.state} />
        {can("operator") && <>
          {stoppable && <button disabled={!!busy} onClick={() => run("stop", () => api.post(`/api/v1/migrations/${id}/databases/${encodeURIComponent(x.source_name)}/stop`), "Database stopped; its slot is kept").then(d.refresh)}>{busy === "stop" && <span className="spin" />}Stop</button>}
          {resumable && <button className="primary" disabled={!!busy} onClick={() => run("resume", () => api.post(`/api/v1/migrations/${id}/databases/${encodeURIComponent(x.source_name)}/resume`), "Resuming").then(d.refresh)}>Resume</button>}
          {restartable && <button className="danger" onClick={() => setRestart(true)}>Restart from zero</button>}
        </>}
      </div>
      {x.last_error && <div className={`callout ${x.state === "failed" || x.state === "restart_required" || x.state === "degraded" ? "crit" : "warn"}`}><div className="body"><strong>{x.error_class ? `Last error (${x.error_class.replace(/_/g, " ")})` : "Last error"}</strong><span className="wrapany">{x.last_error}</span>{x.next_retry_at && <span className="small">{x.next_retry_at > now ? `Next automatic retry in ${duration(x.next_retry_at - now)}` : "Automatic retry due now"}</span>}</div></div>}
      {d.data.snapshot_wait && (
        <div className="callout warn" data-testid="snapshot-wait"><div className="body">
          <strong>The base copy cannot start: creating the replication slot has waited {duration(now - d.data.snapshot_wait.since)} for open transactions</strong>
          <span>PostgreSQL creates a logical slot only after every transaction running on the source cluster when it started has ended, in any database. Ask the customer to commit or end these sessions; the copy starts by itself once they do.</span>
          <ul className="stack" style={{ margin: 0, paddingLeft: 18, gap: 2 }}>{d.data.snapshot_wait.blockers.map((b) => <li key={b.pid} className="mono small">{blockerText(b)}</li>)}</ul>
          <span className="small muted">Checked {ago(d.data.snapshot_wait.at, now)}; every 10 seconds.</span>
        </div></div>
      )}
      <div className="grid cols-4">
        <Tile label="Target" value={<Trunc className="mono">{x.target_name}</Trunc>} sub={`Plugin ${x.plugin || "auto"} · ${count(x.table_count)} tables`} />
        <Tile label="Backlog" value={backlogKnown(x) === undefined ? "—" : bytes(backlogKnown(x)!)} sub={`Replay ${rate(live.replay_rate)}`} />
        <Tile label="Slot" value={live.slot_retained_bytes === undefined ? "—" : bytes(live.slot_retained_bytes)} sub={`${ws ? `WAL ${ws}` : "No slot data"}${live.slot_headroom_bytes !== undefined ? ` · headroom ${bytes(live.slot_headroom_bytes)}` : ""}`} />
        <Tile label="Heartbeat" value={live.heartbeat_latency_s === undefined ? "—" : duration(live.heartbeat_latency_s * 1000)} sub={x.in_sync_since ? `In sync since ${time(x.in_sync_since, false)}` : "End-to-end latency"} />
      </div>
      <Card title="Positions">
        <dl className="kv">
          <dt>Slot</dt><dd className="mono">{x.slot_name}</dd>
          <dt>Origin</dt><dd className="mono">{x.origin_name}</dd>
          <dt>Engine unit</dt><dd className="mono">{d.data.unit_name}{d.data.unit ? ` · ${d.data.unit.active ? "running" : "not running"}${d.data.unit.main_pid ? ` (pid ${d.data.unit.main_pid})` : ""}` : ""}</dd>
          <dt>Source write / replay LSN</dt><dd className="mono">{x.write_lsn || "—"} / {x.replay_lsn || "—"}</dd>
          {x.endpos && <><dt>End position</dt><dd className="mono">{x.endpos}</dd></>}
          {x.origin_lsn && <><dt>Target origin</dt><dd className="mono">{x.origin_lsn}</dd></>}
          {x.hb_before_lsn && <><dt>Final heartbeat at</dt><dd className="mono">{x.hb_before_lsn}</dd></>}
        </dl>
      </Card>
      {d.data.copy_progress && (
        <Card title="Base copy in progress">
          <p className="small muted">Tables being copied right now, with indexes being built. Each bar compares the rows copied with the source's estimated row count, so it can stop short of or run past the end by a little; a large table copied in parts compares its size on the target with its size on the source. Finished tables leave the list.</p>
          {d.data.copy_progress.error && <div className="callout warn"><div className="body"><strong>Upwell could not read the progress from the target</strong><span className="wrapany">{d.data.copy_progress.error}</span><span className="small">The copy itself is not affected; Upwell tries again every 15 seconds.</span></div></div>}
          {d.data.copy_progress.at && <p className="small muted">Read from the target {ago(d.data.copy_progress.at, now)}; refreshed every 15 seconds.</p>}
          {d.data.copy_progress.pending ? <p className="muted">Reading the progress from the target…</p> : d.data.copy_progress.error ? null : d.data.copy_progress.copies.length === 0 && d.data.copy_progress.indexes.length === 0 ? <p className="muted">No COPY or index build was running when the target was last read.</p> : (
            <div className="stack">
              {d.data.copy_progress.copies.map((c) => (
                <div key={c.table} className="row" style={{ flexWrap: "nowrap" }}>
                  <span className="mono" style={{ width: 300, minWidth: 0 }}><Trunc>{c.parts && c.parts > 1 ? `${c.table} (${c.parts} parts)` : c.table}</Trunc></span>
                  <span style={{ flex: 1 }}>{c.fraction !== undefined ? <ProgressBar value={c.fraction} label={`${c.table} copy`} /> : <span className="small muted">No row estimate on the source (the table was never analyzed)</span>}</span>
                  <span className="small muted" style={{ width: 280, textAlign: "right" }} title="Rows copied of the source's estimated row count (pg_class.reltuples), and bytes sent so far; for a table copied in parts, the rows of the parts in progress">{count(c.tuples)}{c.rows_estimate && !(c.parts && c.parts > 1) ? ` of about ${count(c.rows_estimate)}` : ""} rows{c.fraction !== undefined ? ` · ${Math.round(c.fraction * 100)}%` : ""} · {bytes(c.bytes)}</span>
                </div>
              ))}
              {d.data.copy_progress.indexes.map((c) => (
                <div key={c.index} className="row" style={{ flexWrap: "nowrap" }}>
                  <span className="mono" style={{ width: 300, minWidth: 0 }}><Trunc>{c.index}</Trunc></span>
                  <span style={{ flex: 1 }}><ProgressBar value={c.percent / 100} kind="ok" label={`${c.index} build`} /></span>
                  <span className="small muted" style={{ width: 200, textAlign: "right" }}>{c.phase}</span>
                </div>
              ))}
            </div>
          )}
        </Card>
      )}
      <Card title="Charts" actions={picker}>
        <CrosshairProvider>
          <div className="grid cols-2">
            <TimeChart title="Copied to the target" series={seriesFor(mx.data, "target_size_bytes", x.source_name)} fmt={(n) => bytes(n, 0)} from={from} to={to}
              threshold={live.source_size_bytes ? { v: live.source_size_bytes, label: `Source size ${bytes(live.source_size_bytes, 0)}` } : undefined}
              empty="Size of the target database, sampled every 30 seconds; it grows during the base copy (indexes are built at the end, so it can pass the source's size)." />
            <TimeChart title="Slot retained WAL" series={seriesFor(mx.data, "slot_retained_bytes", x.source_name)} fmt={(n) => bytes(n, 0)} from={from} to={to}
              empty="WAL the source keeps for this database's replication slot, sampled every 10 seconds once the slot exists." />
            <TimeChart title="Backlog" series={seriesFor(mx.data, "backlog_bytes", x.source_name)} fmt={(n) => bytes(n, 0)} from={from} to={to}
              empty="How far the target is behind the source. Measured once the base copy has finished and the engine applies changes." />
            <TimeChart title="Replay rate" series={seriesFor(mx.data, "replay_rate", x.source_name)} fmt={(n) => rate(n)} from={from} to={to}
              empty="How fast changes are applied to the target. Measured once the base copy has finished and the engine applies changes." />
            <TimeChart title="Heartbeat latency" series={seriesFor(mx.data, "heartbeat_latency_s", x.source_name)} fmt={(n) => duration(n * 1000)} from={from} to={to}
              empty="Time for a row written to the source to arrive on the target. Measured once the base copy has finished (and only with the heartbeat on)." />
          </div>
        </CrosshairProvider>
      </Card>
      <Card title="Attempts">
        {d.data.attempts.length === 0 ? <p className="muted">The engine has not started for this database yet.</p> : (
          <div className="tablewrap">
            <table className="data" aria-label="Attempts">
              <thead><tr><th className="num">#</th><th>Kind</th><th>Started</th><th>Duration</th><th>Exit</th><th>End reason</th></tr></thead>
              <tbody>{d.data.attempts.map((a) => (
                <tr key={a.number}><td className="num">{a.number}</td><td>{a.kind}</td><td>{time(a.started_at)}</td><td>{duration((a.ended_at || now) - a.started_at)}{!a.ended_at && " (running)"}</td><td>{a.exit_code ?? "—"}</td><td style={{ maxWidth: 420 }}><Trunc>{a.end_reason || "—"}</Trunc></td></tr>
              ))}</tbody>
            </table>
          </div>
        )}
        {d.data.attempts[0] && <details style={{ marginTop: 12 }}><summary>Engine command of attempt {d.data.attempts[0].number}</summary><pre className="cmd" style={{ marginTop: 8 }}>{d.data.attempts[0].command}</pre></details>}
      </Card>
      <WatchedTables mig={id} db={x.source_name} canEdit={can("operator") && !view.data?.migration.fixture} />
      <Card title="Logs for this database" actions={d.data.attempts.length > 1 ? (
        <label className="row small" style={{ gap: 6 }}>Attempt
          <select value={att} onChange={(e) => setAtt(e.target.value)}>
            <option value="">All attempts</option>
            {d.data.attempts.map((a) => <option key={a.number} value={String(a.number)}>#{a.number} {a.kind}, {time(a.started_at)}</option>)}
          </select>
        </label>
      ) : undefined}><LogViewer filter={{ migration: id, database: x.source_name, level: "info", attempt: att || undefined }} height={360} label={`Logs for ${x.source_name}`} /></Card>
      {restart && (
        <ConfirmDialog title={`Restart ${x.source_name} from zero`} danger typeToConfirm={x.source_name} confirmLabel="Restart from zero" onClose={() => setRestart(false)}
          onConfirm={async (t) => { await api.post(`/api/v1/migrations/${id}/databases/${encodeURIComponent(x.source_name)}/restart`, { confirm: t }); d.refresh(); }}>
          <p>This stops the engine, drops the replication slot, origin and publication, <strong>drops and recreates the target database <span className="mono">{x.target_name}</span></strong>, and copies everything again. Other databases are not affected.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}

export function ReplicationTab() {
  const { id, view } = useMig();
  const { from, to, picker } = useRange();
  const mx = useApi<Series[]>(`/api/v1/migrations/${id}/metrics?names=backlog_bytes,replay_rate,slot_retained_bytes,slot_headroom_bytes,src_wal_rate&from=${from}&to=${to}`, 30000);
  const v = view.data!;
  const head = v.databases.filter((d) => d.include).map((d) => ({ d, h: v.live?.[d.source_name]?.slot_headroom_bytes })).filter((x) => x.h !== undefined).sort((a, b) => a.h! - b.h!);
  return (
    <div className="stack" style={{ gap: 24 }}>
      {mx.error && <ErrorState error={mx.error} retry={mx.refresh} />}
      <Card title="CDC across all databases" actions={picker}>
        <CrosshairProvider>
          <div className="grid cols-2">
            <TimeChart title="Backlog per database" series={seriesFor(mx.data, "backlog_bytes")} fmt={(n) => bytes(n, 0)} from={from} to={to} empty="How far each target is behind its source. Measured once a database has finished its base copy." />
            <TimeChart title="Replay rate per database" series={seriesFor(mx.data, "replay_rate")} fmt={(n) => rate(n)} from={from} to={to} empty="How fast changes are applied to each target. Measured once a database has finished its base copy." />
            <TimeChart title="Slot retained WAL per database" series={seriesFor(mx.data, "slot_retained_bytes")} fmt={(n) => bytes(n, 0)} from={from} to={to} empty="WAL the source keeps for each replication slot, sampled every 10 seconds once the slots exist." />
            <TimeChart title="Source WAL rate" series={seriesFor(mx.data, "src_wal_rate", "")} fmt={(n) => rate(n)} from={from} to={to} />
          </div>
        </CrosshairProvider>
      </Card>
      <Card title="Slot headroom">
        {head.length === 0 ? <p className="muted">Headroom appears once slots exist and max_slot_wal_keep_size is limited.</p> : (
          <div className="tablewrap">
            <table className="data" aria-label="Slot headroom">
              <thead><tr><th>Database</th><th className="num">Retained</th><th className="num">Headroom</th></tr></thead>
              <tbody>{head.map(({ d, h }) => <tr key={d.id}><td style={{ maxWidth: 360 }}><Trunc className="mono">{d.source_name}</Trunc></td><td className="num">{bytes(v.live?.[d.source_name]?.slot_retained_bytes)}</td><td className="num">{bytes(h)}</td></tr>)}</tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}

interface Health { available: boolean; reason?: string; oldest_xact_seconds?: number; oldest_snapshot_seconds?: number; xmin_age?: number; connections?: number; max_connections?: number; transactions?: { pid: number; user: string; database: string; application: string; state: string; seconds: number; query: string }[]; top_dead_tuples?: { database: string; table: string; dead_tuples: number; live_tuples: number; changes: number; since_vacuum_seconds: number }[] }

export function SourceHealthTab() {
  const { id } = useMig();
  const h = useApi<Health>(`/api/v1/migrations/${id}/source-health`, 30000);
  const { from, to, picker } = useRange();
  const mx = useApi<Series[]>(`/api/v1/migrations/${id}/metrics?names=src_oldest_xact_s,src_xmin_age,src_connections&from=${from}&to=${to}`, 30000);
  if (h.error) return <ErrorState error={h.error} retry={h.refresh} />;
  if (!h.data) return <Skeleton lines={8} />;
  if (!h.data.available) return <Empty title="Source health is not available">{h.data.reason || "Upwell could not connect to the source."}</Empty>;
  const d = h.data;
  return (
    <div className="stack" style={{ gap: 24 }}>
      <div className="grid cols-4">
        <Tile label="Oldest snapshot" value={duration((d.oldest_snapshot_seconds || 0) * 1000)} sub="VACUUM cannot remove rows newer than this" />
        <Tile label="xmin age" value={count(d.xmin_age)} sub="Transactions since the oldest snapshot" title={(d.xmin_age || 0).toLocaleString("en-US")} />
        <Tile label="Oldest transaction" value={duration((d.oldest_xact_seconds || 0) * 1000)} sub="Including the engine's snapshot" />
        <Tile label="Connections" value={`${d.connections} of ${d.max_connections}`} sub={`${Math.round(((d.connections || 0) / (d.max_connections || 1)) * 100)}% used`} />
      </div>
      <Card title="Trends" actions={picker}>
        <CrosshairProvider>
          <div className="grid cols-3">
            <TimeChart title="Oldest transaction" series={seriesFor(mx.data, "src_oldest_xact_s", "")} fmt={(n) => duration(n * 1000)} from={from} to={to} />
            <TimeChart title="xmin age" series={seriesFor(mx.data, "src_xmin_age", "")} fmt={(n) => count(n)} from={from} to={to} />
            <TimeChart title="Source connections" series={seriesFor(mx.data, "src_connections", "")} fmt={(n) => count(n)} from={from} to={to} />
          </div>
        </CrosshairProvider>
      </Card>
      <Card title="Long transactions">
        {(d.transactions || []).length === 0 ? <p className="muted">No open transactions.</p> : (
          <div className="tablewrap">
            <table className="data" aria-label="Long transactions">
              <thead><tr><th className="num">PID</th><th>User</th><th>Database</th><th>Application</th><th>State</th><th className="num">Open for</th><th>Query</th></tr></thead>
              <tbody>{d.transactions!.map((t) => <tr key={t.pid}><td className="num">{t.pid}</td><td>{t.user}</td><td style={{ maxWidth: 160 }}><Trunc className="mono">{t.database}</Trunc></td><td style={{ maxWidth: 160 }}><Trunc>{t.application || "—"}</Trunc></td><td>{t.state}</td><td className="num">{duration(t.seconds * 1000)}</td><td style={{ maxWidth: 360 }}><Trunc className="mono">{t.query || "—"}</Trunc></td></tr>)}</tbody>
            </table>
          </div>
        )}
      </Card>
      <Card title="Dead tuples on the busiest tables">
        {(d.top_dead_tuples || []).length === 0 ? <p className="muted">No tables with dead tuples.</p> : (
          <div className="tablewrap">
            <table className="data" aria-label="Dead tuples">
              <thead><tr><th>Database</th><th>Table</th><th className="num">Dead</th><th className="num">Live</th><th className="num">Changes</th><th className="num">Since vacuum</th></tr></thead>
              <tbody>{d.top_dead_tuples!.map((t) => <tr key={t.database + t.table}><td style={{ maxWidth: 200 }}><Trunc className="mono">{t.database}</Trunc></td><td style={{ maxWidth: 260 }}><Trunc className="mono">{t.table}</Trunc></td><td className="num">{count(t.dead_tuples)}</td><td className="num">{count(t.live_tuples)}</td><td className="num">{count(t.changes)}</td><td className="num">{t.since_vacuum_seconds < 0 ? "never" : duration(t.since_vacuum_seconds * 1000)}</td></tr>)}</tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}

export function LogFilters({ value, onChange, databases }: { value: Record<string, string>; onChange: (v: Record<string, string>) => void; databases?: string[] }) {
  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) => onChange({ ...value, [k]: e.target.value });
  return (
    <div className="row">
      <input className="search" type="search" placeholder="Search messages" aria-label="Search messages" value={value.q || ""} onChange={set("q")} />
      <label className="row small" style={{ gap: 6 }}>Level
        <select value={value.level || "info"} onChange={set("level")}><option value="debug">Debug and up</option><option value="info">Info and up</option><option value="warn">Warnings and up</option><option value="error">Errors only</option></select>
      </label>
      <label className="row small" style={{ gap: 6 }}>Component
        <select value={value.component || ""} onChange={set("component")}><option value="">All</option><option value="engine">Engine</option><option value="orchestrator">Orchestrator</option><option value="preflight">Preflight</option><option value="cutover">Cutover</option><option value="verify">Verify</option><option value="api">API</option><option value="app">App</option></select>
      </label>
      {databases && (
        <label className="row small" style={{ gap: 6 }}>Database
          <select value={value.database || ""} onChange={set("database")} style={{ maxWidth: 240 }}><option value="">All</option>{databases.map((d) => <option key={d} value={d}>{d}</option>)}</select>
        </label>
      )}
    </div>
  );
}

export function MigrationLogsTab() {
  const { id, view } = useMig();
  const [f, setF] = useState<Record<string, string>>({ level: "info" });
  const q = useDebounced(f.q || "", 400);
  return (
    <Card title="Logs">
      <div className="stack">
        <LogFilters value={f} onChange={setF} databases={view.data!.databases.filter((d) => d.include).map((d) => d.source_name)} />
        <LogViewer filter={{ migration: id, level: f.level, component: f.component, database: f.database, q }} height={560} label="Migration logs" />
      </div>
    </Card>
  );
}

export function MigrationEventsTab() {
  const { id } = useMig();
  const ev = useApi<EventRow[]>(`/api/v1/migrations/${id}/events?limit=500`, 15000);
  const now = useNow(10000);
  return (
    <Card title="Timeline">
      {ev.error ? <ErrorState error={ev.error} retry={ev.refresh} /> : !ev.data ? <Skeleton lines={8} /> : ev.data.length === 0 ? <Empty title="No events yet">Phase changes, operations, alerts and operator actions appear here.</Empty> : <EventList rows={ev.data} now={now} />}
    </Card>
  );
}

interface Watched { id: string; database: string; schema: string; table: string; added_by: string; added_at: number; source_rows?: number; target_rows?: number; counted_at?: number; count_error?: string }

// Watched tables (overnight item 9): exact counts on both sides while streaming.
function WatchedTables({ mig, db, canEdit }: { mig: string; db: string; canEdit: boolean }) {
  const ws = useApi<Watched[]>(`/api/v1/migrations/${mig}/watched?database=${encodeURIComponent(db)}`, 15000);
  const [adding, setAdding] = useState(false);
  const { run, busy } = useAction();
  const now = useNow(5000);
  return (
    <Card title="Watched tables" actions={canEdit && <button onClick={() => setAdding(true)}>Watch a table</button>}>
      <p className="muted small">Exact row counts on both sides, once a minute while the database streams, for the tables you choose. Each count reads the whole table, so watch only the tables you care about. The target is normally a little behind; verification compares every table after the cutover.</p>
      {ws.error ? <ErrorState error={ws.error} retry={ws.refresh} /> : !ws.data ? <Skeleton lines={2} /> : ws.data.length === 0 ? <Empty title="No watched tables">Watch a table to see its exact counts here.</Empty> : (
        <div className="tablewrap">
          <table className="data" aria-label="Watched tables">
            <thead><tr><th>Table</th><th className="num">Source rows</th><th className="num">Target rows</th><th className="num">Difference</th><th>Counted</th><th><span className="sr-only">Actions</span></th></tr></thead>
            <tbody>{ws.data.map((w) => {
              const diff = w.source_rows !== undefined && w.source_rows !== null && w.target_rows !== undefined && w.target_rows !== null ? w.source_rows - w.target_rows : undefined;
              return (
                <tr key={w.id}>
                  <td style={{ maxWidth: 260 }}><Trunc className="mono">{`${w.schema}.${w.table}`}</Trunc></td>
                  <td className="num">{w.source_rows ?? "—"}</td>
                  <td className="num">{w.target_rows ?? "—"}</td>
                  <td className="num">{diff === undefined ? "—" : diff === 0 ? <Pill kind="ok" text="Equal" /> : diff > 0 ? `${count(diff)} behind` : `${count(-diff)} more on the target`}</td>
                  <td>{w.count_error ? <span className="small" style={{ color: "var(--crit)" }}>Could not count: {w.count_error}</span> : w.counted_at ? ago(w.counted_at, now) : "Not counted yet"}</td>
                  <td>{canEdit && <button className="ghost" disabled={!!busy} onClick={() => run("unwatch", () => api.del(`/api/v1/migrations/${mig}/watched/${w.id}`)).then(ws.refresh)}>Stop watching</button>}</td>
                </tr>
              );
            })}</tbody>
          </table>
        </div>
      )}
      {adding && <WatchPicker mig={mig} db={db} onClose={() => setAdding(false)} onDone={ws.refresh} />}
    </Card>
  );
}

function WatchPicker({ mig, db, onClose, onDone }: { mig: string; db: string; onClose: () => void; onDone: () => void }) {
  const tables = useApi<{ schema: string; table: string; estimated_rows: number; size_bytes: number }[]>(`/api/v1/migrations/${mig}/databases/${encodeURIComponent(db)}/tables`);
  const [sel, setSel] = useState("");
  const [err, setErr] = useState<Error>();
  const save = async () => {
    const [schema, ...rest] = sel.split(".");
    try { await api.post(`/api/v1/migrations/${mig}/watched`, { database: db, schema, table: rest.join(".") }); onDone(); onClose(); } catch (e) { setErr(e as Error); }
  };
  return (
    <Dialog title={`Watch a table in ${db}`} onClose={onClose} footer={<><button onClick={onClose}>Cancel</button><button className="primary" disabled={!sel} onClick={save}>Watch</button></>}>
      {tables.error ? <ErrorState error={tables.error} retry={tables.refresh} /> : !tables.data ? <Skeleton lines={3} /> : (
        <label className="field"><span className="lbl">Table</span>
          <select value={sel} onChange={(e) => setSel(e.target.value)}>
            <option value="">Choose a table</option>
            {tables.data.map((t) => <option key={t.schema + "." + t.table} value={t.schema + "." + t.table}>{t.schema}.{t.table} (about {count(t.estimated_rows)} rows, {bytes(t.size_bytes)})</option>)}
          </select>
        </label>
      )}
      {err && <ErrorState error={err} />}
    </Dialog>
  );
}
