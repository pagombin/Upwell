import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../lib/api";
import { rateFrom } from "../lib/derive";
import { ago, bytes, count, duration, pct, time } from "../lib/format";
import { useApi, useDebounced, useNow } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Alert, AuditEntry, EventRow, MigrationView, Series, SettingItem, User } from "../lib/types";
import { CrosshairProvider, seriesFor, TimeChart } from "../components/charts";
import { LogViewer } from "../components/logview";
import { DataTable } from "../components/table";
import { Card, ComingSoon, ConfirmDialog, Dialog, Empty, ErrorState, Field, Pill, Skeleton, Tile, Trunc, useAction, useToast } from "../components/ui";
import type { SystemInfo } from "./home";
import { LogFilters } from "./mtabs";
import { SettingInput } from "./wizard";

function Page({ title, subtitle, actions, children }: { title: string; subtitle?: string; actions?: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className="page">
      <div className="pagehead"><div className="titles"><h1>{title}</h1>{subtitle && <span className="subtitle">{subtitle}</span>}</div>{actions && <div className="actions">{actions}</div>}</div>
      {children}
    </div>
  );
}

// ---------- Logs

export function LogsScreen() {
  const migs = useApi<MigrationView[]>("/api/v1/migrations");
  const [f, setF] = useState<Record<string, string>>({ level: "info" });
  const q = useDebounced(f.q || "", 400);
  return (
    <Page title="Logs" subtitle="Everything Upwell and the engines write, live. Click a line for its full detail.">
      <Card>
        <div className="stack">
          <div className="row">
            <LogFilters value={f} onChange={setF} />
            <label className="row small" style={{ gap: 6 }}>Migration
              <select value={f.migration || ""} onChange={(e) => setF({ ...f, migration: e.target.value })} style={{ maxWidth: 260 }}>
                <option value="">All</option>{(migs.data || []).map((v) => <option key={v.migration.id} value={v.migration.id}>{v.migration.name}</option>)}
              </select>
            </label>
          </div>
          <LogViewer filter={{ migration: f.migration, level: f.level, component: f.component, q }} height={600} label="All logs" />
          <p className="small faint">Saved views <ComingSoon what="Saved log views" /></p>
        </div>
      </Card>
    </Page>
  );
}

// ---------- Events and audit

export function EventsScreen() {
  const [tab, setTab] = useState<"timeline" | "audit">("timeline");
  const ev = useApi<EventRow[]>(tab === "timeline" ? "/api/v1/events?limit=500" : null, 15000);
  const au = useApi<AuditEntry[]>(tab === "audit" ? "/api/v1/audit?limit=500" : null, 30000);
  const ver = useApi<{ ok: boolean; entries: number; bad_seq?: number; message: string }>(tab === "audit" ? "/api/v1/audit/verify" : null);
  const migs = useApi<MigrationView[]>("/api/v1/migrations");
  const names = useMemo(() => new Map((migs.data || []).map((v) => [v.migration.id, v.migration])), [migs.data]);
  const [open, setOpen] = useState<AuditEntry | null>(null);
  const now = useNow(10000);
  return (
    <Page title="Events and audit" subtitle="What happened, and who did it.">
      <div className="tabs" role="tablist">
        <button role="tab" className="tab ghost" aria-selected={tab === "timeline"} aria-current={tab === "timeline" ? "page" : undefined} onClick={() => setTab("timeline")}>Timeline</button>
        <button role="tab" className="tab ghost" aria-selected={tab === "audit"} aria-current={tab === "audit" ? "page" : undefined} onClick={() => setTab("audit")}>Audit log</button>
      </div>
      {tab === "timeline" ? (
        <Card>
          {ev.error ? <ErrorState error={ev.error} retry={ev.refresh} /> : !ev.data ? <Skeleton lines={8} /> : (
            <DataTable label="Events" rows={ev.data} rowKey={(e) => e.id} csvName="upwell-events" filterText={(e) => `${e.message} ${e.database} ${e.type}`}
              emptyTitle="No events yet" emptyHint="Phase changes, alerts, operations and operator actions appear here."
              columns={[
                { key: "ts", label: "Time", sort: (e) => e.ts, render: (e) => <span title={time(e.ts)}>{ago(e.ts, now)}</span>, csv: (e) => new Date(e.ts).toISOString(), width: 120 },
                { key: "mig", label: "Migration", sort: (e) => names.get(e.migration_id)?.name || "", render: (e) => { const m = names.get(e.migration_id); return m ? <Link to={`/migrations/${m.short_id}/events`}><Trunc>{m.name}</Trunc></Link> : <span className="faint">—</span>; }, width: 200 },
                { key: "db", label: "Database", sort: (e) => e.database, render: (e) => e.database ? <Trunc className="mono">{e.database}</Trunc> : <span className="faint">—</span>, width: 180 },
                { key: "sev", label: "Severity", sort: (e) => e.severity, render: (e) => <Pill kind={e.severity === "critical" || e.severity === "error" ? "crit" : e.severity === "warning" ? "warn" : "info"} text={e.severity} />, csv: (e) => e.severity, width: 110 },
                { key: "msg", label: "Event", sort: (e) => e.message, render: (e) => <span className="wrapany">{e.message}</span> },
              ]} />
          )}
        </Card>
      ) : (
        <Card title="Audit log" actions={<a className="btn" href="/api/v1/audit?format=download&limit=100000" download>Download</a>}>
          <div className="stack">
            {ver.data && <div className={`callout ${ver.data.ok ? "ok" : "crit"}`}><div className="body"><strong>{ver.data.ok ? "The audit chain is intact" : "The audit chain is broken"}</strong><span>{ver.data.message}</span></div></div>}
            {au.error ? <ErrorState error={au.error} retry={au.refresh} /> : !au.data ? <Skeleton lines={8} /> : (
              <DataTable label="Audit entries" rows={au.data} rowKey={(a) => a.id} csvName="upwell-audit" filterText={(a) => `${a.action} ${a.username} ${a.target} ${a.ip}`}
                onRow={setOpen} emptyTitle="No audit entries"
                columns={[
                  { key: "ts", label: "Time", sort: (a) => a.ts, render: (a) => time(a.ts), csv: (a) => new Date(a.ts).toISOString(), width: 220 },
                  { key: "user", label: "User", sort: (a) => a.username || "", render: (a) => a.username || <span className="faint">system</span>, width: 140 },
                  { key: "ip", label: "IP", sort: (a) => a.ip || "", render: (a) => <span className="mono">{a.ip || "—"}</span>, width: 140 },
                  { key: "action", label: "Action", sort: (a) => a.action, render: (a) => <button className="ghost" style={{ padding: 0, minHeight: 0 }} onClick={(e) => { e.stopPropagation(); setOpen(a); }}><span className="mono">{a.action}</span></button> },
                  { key: "target", label: "Target", sort: (a) => a.target || "", render: (a) => <Trunc className="mono">{a.target || "—"}</Trunc>, width: 260 },
                ]} />
            )}
          </div>
        </Card>
      )}
      {open && (
        <Dialog title={open.action} wide onClose={() => setOpen(null)}>
          <dl className="kv"><dt>Time</dt><dd>{time(open.ts)}</dd><dt>User</dt><dd>{open.username || "system"}</dd><dt>IP</dt><dd className="mono">{open.ip || "—"}</dd><dt>Target</dt><dd className="mono">{open.target || "—"}</dd><dt>Hash</dt><dd className="mono">{open.hash}</dd></dl>
          <div className="grid cols-2">
            <div className="stack"><h3>Before</h3><pre className="cmd">{open.before === undefined || open.before === null ? "—" : JSON.stringify(open.before, null, 2)}</pre></div>
            <div className="stack"><h3>After</h3><pre className="cmd">{open.after === undefined || open.after === null ? "—" : JSON.stringify(open.after, null, 2)}</pre></div>
          </div>
        </Dialog>
      )}
    </Page>
  );
}

// ---------- Alerts

export function AlertsScreen() {
  const [state, setState] = useState("");
  const al = useApi<Alert[]>(`/api/v1/alerts${state ? `?state=${state}` : ""}`, 10000);
  const migs = useApi<MigrationView[]>("/api/v1/migrations");
  const names = useMemo(() => new Map((migs.data || []).map((v) => [v.migration.id, v.migration])), [migs.data]);
  const { can } = useSession();
  const [ack, setAck] = useState<Alert | null>(null);
  const now = useNow(10000);
  return (
    <Page title="Alerts" subtitle="Firing, acknowledged and resolved alerts across migrations. Notifications to Slack, email or PagerDuty are coming soon.">
      <Card>
        {al.error ? <ErrorState error={al.error} retry={al.refresh} /> : !al.data ? <Skeleton lines={6} /> : (
          <DataTable label="Alerts" rows={al.data} rowKey={(a) => a.id} csvName="upwell-alerts" filterText={(a) => `${a.message} ${a.rule}`}
            toolbar={<label className="row small" style={{ gap: 6 }}>State<select value={state} onChange={(e) => setState(e.target.value)}><option value="">All</option><option value="firing">Firing</option><option value="acknowledged">Acknowledged</option><option value="resolved">Resolved</option></select></label>}
            emptyTitle="No alerts" emptyHint="Alerts fire on slot headroom, disk space, stalled replication, engine errors and more. Thresholds are in Settings."
            columns={[
              { key: "sev", label: "Severity", sort: (a) => a.severity, render: (a) => <Pill kind={a.severity === "critical" ? "crit" : "warn"} text={a.severity} />, csv: (a) => a.severity, width: 110 },
              { key: "state", label: "State", sort: (a) => a.state, render: (a) => <Pill state={a.state} text={a.state} />, csv: (a) => a.state, width: 130 },
              { key: "msg", label: "Alert", sort: (a) => a.message, render: (a) => <div className="stack" style={{ gap: 2 }}><span className="wrapany">{a.message}</span><span className="small faint mono">{a.rule}{a.scope ? ` · ${a.scope}` : ""}</span>{a.ack_note && <span className="small muted">Note from {a.acked_by}: {a.ack_note}</span>}</div> },
              { key: "mig", label: "Migration", sort: (a) => names.get(a.migration_id || "")?.name || "", render: (a) => { const m = names.get(a.migration_id || ""); return m ? <Link to={`/migrations/${m.short_id}`}><Trunc>{m.name}</Trunc></Link> : <span className="faint">Host</span>; }, width: 200 },
              { key: "since", label: "Since", sort: (a) => a.first_at, render: (a) => <span title={time(a.first_at)}>{ago(a.first_at, now)}</span>, csv: (a) => new Date(a.first_at).toISOString(), width: 120 },
              { key: "act", label: "Actions", srLabel: true, render: (a) => a.state === "firing" && can("operator") ? <button onClick={() => setAck(a)}>Acknowledge</button> : null, width: 140 },
            ]} />
        )}
      </Card>
      {ack && <AckDialog a={ack} onClose={() => setAck(null)} onDone={al.refresh} />}
    </Page>
  );
}

function AckDialog({ a, onClose, onDone }: { a: Alert; onClose: () => void; onDone: () => void }) {
  const [note, setNote] = useState("");
  const { run, busy } = useAction();
  return (
    <Dialog title="Acknowledge alert" onClose={onClose} footer={<><button onClick={onClose}>Cancel</button><button className="primary" disabled={!!busy} onClick={async () => { const r = await run("ack", () => api.post(`/api/v1/alerts/${a.id}/ack`, { note }), "Alert acknowledged"); if (r) { onDone(); onClose(); } }}>Acknowledge</button></>}>
      <p className="wrapany">{a.message}</p>
      <Field label="Note (optional)" hint="Say what you are doing about it; others see the note."><textarea value={note} onChange={(e) => setNote(e.target.value)} /></Field>
    </Dialog>
  );
}

// ---------- Host

export function HostScreen() {
  const [mins, setMins] = useState(60);
  const now = useNow(30000);
  const from = Math.floor(now / 30000) * 30000 - mins * 60000, to = Math.floor(now / 30000) * 30000;
  const mx = useApi<Series[]>(`/api/v1/host/metrics?names=work_volume_pct,work_volume_free_bytes,staging_bytes,cpu_pct,mem_pct,load1&from=${from}&to=${to}`, 30000);
  const sys = useApi<SystemInfo & { units: { migration: string; migration_name: string; database: string; state: string; unit: string; status: { active: boolean; main_pid: number; main_live: boolean } | null }[] }>("/api/v1/system", 15000);
  const lastV = (n: string) => mx.data?.find((s) => s.name === n)?.points.slice(-1)[0]?.v;
  const staging = mx.data?.find((s) => s.name === "staging_bytes")?.points || [];
  const growth = rateFrom(staging, 15 * 60e3);
  const free = lastV("work_volume_free_bytes");
  const fullIn = growth && growth > 0 && free ? (free / growth) * 1000 : undefined;
  return (
    <Page title="Host" subtitle="This droplet: work volume, staging growth, CPU, memory and engine units." actions={
      <label className="row small" style={{ gap: 6 }}>Range<select value={mins} onChange={(e) => setMins(Number(e.target.value))}><option value={60}>1 hour</option><option value={360}>6 hours</option><option value={1440}>1 day</option><option value={10080}>7 days</option></select></label>}>
      {mx.error && <ErrorState error={mx.error} retry={mx.refresh} />}
      <div className="grid cols-4">
        <Tile label="Work volume used" value={pct(lastV("work_volume_pct"))} sub={free !== undefined ? `${bytes(free)} free` : "Waiting for a sample"} />
        <Tile label="Staging" value={bytes(lastV("staging_bytes"))} sub={fullIn ? `Full in about ${duration(fullIn)} at this growth` : "Not growing"} />
        <Tile label="CPU" value={pct(lastV("cpu_pct"))} sub={`Load ${lastV("load1")?.toFixed(2) ?? "—"}`} />
        <Tile label="Memory" value={pct(lastV("mem_pct"))} sub="Used, excluding cache" />
      </div>
      <Card title="Charts">
        <CrosshairProvider>
          <div className="grid cols-2">
            <TimeChart title="Work volume used" series={seriesFor(mx.data, "work_volume_pct", "")} fmt={(n) => pct(n)} from={from} to={to} threshold={{ v: 85, label: "Alert at 85%" }} />
            <TimeChart title="Staging size" series={seriesFor(mx.data, "staging_bytes", "")} fmt={(n) => bytes(n, 0)} from={from} to={to} />
            <TimeChart title="CPU" series={seriesFor(mx.data, "cpu_pct", "")} fmt={(n) => pct(n)} from={from} to={to} />
            <TimeChart title="Memory" series={seriesFor(mx.data, "mem_pct", "")} fmt={(n) => pct(n)} from={from} to={to} />
          </div>
        </CrosshairProvider>
        <p className="small faint" style={{ marginTop: 12 }}>Network throughput <ComingSoon what="Network metrics" /></p>
      </Card>
      <Card title="Engine units">
        {!sys.data ? <Skeleton lines={4} /> : sys.data.units.length === 0 ? <p className="muted">No engine units: no migration is running.</p> : (
          <div className="tablewrap">
            <table className="data" aria-label="Engine units">
              <thead><tr><th>Unit</th><th>Migration</th><th>Database</th><th>Phase</th><th>Process</th></tr></thead>
              <tbody>{sys.data.units.map((u) => (
                <tr key={u.unit}><td className="mono" style={{ maxWidth: 280 }}><Trunc>{u.unit}</Trunc></td><td style={{ maxWidth: 200 }}><Link to={`/migrations/${u.migration}`}><Trunc>{u.migration_name}</Trunc></Link></td><td style={{ maxWidth: 200 }}><Trunc className="mono">{u.database}</Trunc></td><td><Pill state={u.state} /></td><td>{u.status?.active ? `Running (pid ${u.status.main_pid})` : "Not running"}</td></tr>
              ))}</tbody>
            </table>
          </div>
        )}
      </Card>
    </Page>
  );
}

// ---------- System

export function SystemScreen() {
  const sys = useApi<SystemInfo>("/api/v1/system", 10000);
  const { can } = useSession();
  const toast = useToast();
  const [restart, setRestart] = useState(false);
  const now = useNow(10000);
  return (
    <Page title="System" subtitle="Health of Upwell itself." actions={can("admin") ? <button onClick={() => setRestart(true)}>Restart Upwell</button> : undefined}>
      {sys.error ? <ErrorState error={sys.error} retry={sys.refresh} /> : !sys.data ? <Skeleton lines={8} /> : <>
        <div className="grid cols-4">
          <Tile label="Service" value={sys.data.ready ? "Ready" : "Starting"} sub={`Up ${duration(sys.data.uptime_s * 1000)} since ${time(sys.data.started_at, false)}`} />
          <Tile label="Watchdog" value={sys.data.watchdog.healthy ? "Healthy" : "Late"} sub={`${count(sys.data.watchdog.beats)} heartbeats${sys.data.watchdog.systemd ? "" : " · not under systemd"}`} />
          <Tile label="Store" value={bytes(sys.data.store.bytes)} sub={`Schema version ${sys.data.store.schema_version}`} />
          <Tile label="Logs on disk" value={bytes(sys.data.logs.bytes)} sub={`Engine runs ${bytes(sys.data.runs.bytes)}`} />
        </div>
        {sys.data.rebooted && <div className="callout info"><div className="body">The droplet rebooted before this start. Upwell reconciled every migration{sys.data.reconciled ? "" : " (still reconciling)"}.</div></div>}
        <Card title="Installed components">
          <dl className="kv">
            <dt>Upwell</dt><dd className="mono">{sys.data.version} ({sys.data.go})</dd>
            <dt>Engine</dt><dd className="mono">{sys.data.engine.version} · {sys.data.engine.runner} runner</dd>
            {Object.entries(sys.data.tools).map(([k, v]) => <><dt key={k + "t"}>{k}</dt><dd key={k + "v"} className="mono">{v}</dd></>)}
            <dt>Store</dt><dd className="mono wrapany">{sys.data.store.path}</dd>
            <dt>Logs</dt><dd className="mono wrapany">{sys.data.logs.dir}</dd>
            <dt>Engine work</dt><dd className="mono wrapany">{sys.data.runs.dir}</dd>
            <dt>Certificate</dt><dd className="mono wrapany">{sys.data.tls.fingerprint} · valid until {time(sys.data.tls.not_after)}</dd>
          </dl>
        </Card>
        <Card title="Updates"><p>{sys.data.updates.note}</p><p className="small muted">Checking for new versions automatically is coming soon.</p></Card>
        <Card title="Readiness detail"><pre className="cmd">{JSON.stringify(sys.data.ready_detail, null, 2)}</pre><p className="small faint" style={{ marginTop: 8 }}>Checked {ago(sys.data.started_at, now)} after start.</p></Card>
      </>}
      {restart && (
        <ConfirmDialog title="Restart Upwell" confirmLabel="Restart" onClose={() => setRestart(false)}
          onConfirm={async () => { await api.post("/api/v1/system/restart"); toast("info", "Upwell is restarting", "This page reconnects by itself."); }}>
          <p>The Upwell service restarts. Engines run in their own systemd units and keep copying and streaming; Upwell reattaches to them when it starts. The browser reconnects automatically.</p>
        </ConfirmDialog>
      )}
    </Page>
  );
}

// ---------- Settings

const GROUPS = ["Migration defaults", "Data safety", "Cutover", "Preflight", "Recovery", "Alerts", "Monitoring", "Logging", "Access"];
const SPECIAL = ["Users and roles", "TLS", "Notifications", "DigitalOcean API", "API tokens", "Metrics export", "System"];

export function SettingsScreen() {
  const cat = useApi<SettingItem[]>("/api/v1/settings");
  const [sec, setSec] = useState("Migration defaults");
  const groups = useMemo(() => new Set((cat.data || []).map((i) => i.group)), [cat.data]);
  return (
    <Page title="Settings" subtitle="Defaults for new migrations, alert thresholds, users and this install. Changes are audited.">
      <div className="grid" style={{ gridTemplateColumns: "220px minmax(0, 1fr)" }}>
        <nav className="stack" style={{ gap: 2 }} aria-label="Settings sections">
          {[...GROUPS.filter((g) => groups.has(g)), ...SPECIAL].map((s) => (
            <button key={s} className="ghost" style={{ justifyContent: "flex-start", color: sec === s ? "var(--text)" : undefined, background: sec === s ? "var(--raised)" : undefined }} aria-current={sec === s ? "page" : undefined} onClick={() => setSec(s)}>{s}</button>
          ))}
        </nav>
        <div className="stack" style={{ minWidth: 0 }}>
          {sec === "Users and roles" ? <UsersSection /> : sec === "TLS" ? <TLSSection /> : sec === "System" ? <Card title="System"><p>Versions, services, watchdog and restart are on the <Link to="/system">System</Link> screen.</p></Card>
            : ["Notifications", "DigitalOcean API", "API tokens", "Metrics export"].includes(sec) ? <Card title={sec} actions={<ComingSoon what={sec} />}><p className="muted">{sec} is not part of this build.</p></Card>
            : cat.error ? <ErrorState error={cat.error} retry={cat.refresh} /> : !cat.data ? <Skeleton lines={8} /> : <CatalogSection key={sec} items={cat.data.filter((i) => i.group === sec)} title={sec} refresh={cat.refresh} />}
        </div>
      </div>
    </Page>
  );
}

function CatalogSection({ items, title, refresh }: { items: SettingItem[]; title: string; refresh: () => void }) {
  const { can } = useSession();
  const [vals, setVals] = useState<Record<string, unknown>>({});
  const { run, busy } = useAction();
  const changed = Object.keys(vals).filter((k) => JSON.stringify(vals[k]) !== JSON.stringify(items.find((i) => i.key === k)?.value));
  if (items.length === 0) return <Card title={title}><p className="muted">Nothing to configure here in this build.</p></Card>;
  return (
    <Card title={title} actions={can("admin") ? <button className="primary" disabled={!changed.length || !!busy} onClick={async () => {
      const body: Record<string, unknown> = {};
      for (const k of changed) body[k] = vals[k];
      const r = await run("save", () => api.patch("/api/v1/settings", body), "Settings saved");
      if (r) { setVals({}); refresh(); }
    }}>Save changes</button> : <span className="small muted">Only Admins change settings.</span>}>
      <div className="stack">
        <p className="small muted">{items.some((i) => i.scope === "migration") ? "Defaults for new migrations; each migration can override them before it starts, and its values are frozen when it starts." : "Applies to the whole install."}</p>
        <div className="fieldgrid">
          {items.map((i) => (
            <div key={i.key} className="stack" style={{ gap: 4 }}>
              <SettingInput item={i} value={i.key in vals ? vals[i.key] : i.value} disabled={!can("admin")} onChange={(v) => setVals({ ...vals, [i.key]: v })} />
              <span className="small faint">Default {JSON.stringify(i.default)}{i.updated_by ? ` · changed by ${i.updated_by} ${time(i.updated_at)}` : ""}</span>
            </div>
          ))}
        </div>
      </div>
    </Card>
  );
}

function UsersSection() {
  const { can, me } = useSession();
  const users = useApi<User[]>(can("admin") ? "/api/v1/users" : null);
  const [create, setCreate] = useState(false);
  const [del, setDel] = useState<User | null>(null);
  const [reset, setReset] = useState<User | null>(null);
  const { run } = useAction();
  if (!can("admin")) return <Card title="Users and roles"><p className="muted">Only Admins manage users.</p></Card>;
  const update = (u: User, p: Partial<User>) => run("u", () => api.patch(`/api/v1/users/${u.id}`, { role: p.role ?? u.role, disabled: p.disabled ?? u.disabled }), "User updated").then(users.refresh);
  return (
    <Card title="Users and roles" actions={<button className="primary" onClick={() => setCreate(true)}>Add user</button>}>
      <div className="stack">
        <p className="small muted">Viewers see everything. Operators also run migrations, preflight, cutover and cleanup. Admins also manage users and settings. SSO and TOTP are coming soon.</p>
        {users.error ? <ErrorState error={users.error} retry={users.refresh} /> : !users.data ? <Skeleton lines={4} /> : (
          <div className="tablewrap">
            <table className="data" aria-label="Users">
              <thead><tr><th>Username</th><th>Role</th><th>Status</th><th>Last sign-in</th><th><span className="sr-only">Actions</span></th></tr></thead>
              <tbody>{users.data.map((u) => (
                <tr key={u.id}>
                  <td style={{ maxWidth: 240 }}><Trunc>{u.username}</Trunc></td>
                  <td><select aria-label={`Role of ${u.username}`} value={u.role} disabled={u.id === me?.user.id} onChange={(e) => update(u, { role: e.target.value as User["role"] })}><option value="viewer">Viewer</option><option value="operator">Operator</option><option value="admin">Admin</option></select></td>
                  <td>{u.disabled ? <Pill kind="warn" text="Disabled" /> : <Pill kind="ok" text="Active" />}</td>
                  <td>{u.last_login_at ? time(u.last_login_at) : "Never"}</td>
                  <td>{u.id !== me?.user.id && <div className="row" style={{ gap: 6, flexWrap: "nowrap" }}>
                    <button className="ghost" onClick={() => update(u, { disabled: !u.disabled })}>{u.disabled ? "Enable" : "Disable"}</button>
                    <button className="ghost" onClick={() => setReset(u)}>Reset password</button>
                    <button className="ghost" onClick={() => setDel(u)}>Delete</button>
                  </div>}</td>
                </tr>
              ))}</tbody>
            </table>
          </div>
        )}
      </div>
      {create && <CreateUser onClose={() => setCreate(false)} onDone={users.refresh} />}
      {reset && <ResetPassword u={reset} onClose={() => setReset(null)} />}
      {del && <ConfirmDialog title={`Delete ${del.username}`} danger typeToConfirm={del.username} confirmLabel="Delete user" onClose={() => setDel(null)} onConfirm={async () => { await api.del(`/api/v1/users/${del.id}`); users.refresh(); }}><p>The user can no longer sign in. Their audit entries stay.</p></ConfirmDialog>}
    </Card>
  );
}

function CreateUser({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [f, setF] = useState({ username: "", password: "", role: "operator" });
  const { run, busy } = useAction();
  return (
    <Dialog title="Add user" onClose={onClose} footer={<><button onClick={onClose}>Cancel</button><button className="primary" disabled={!f.username || f.password.length < 14 || !!busy} onClick={async () => { const r = await run("c", () => api.post("/api/v1/users", f), "User added"); if (r) { onDone(); onClose(); } }}>Add user</button></>}>
      <Field label="Username"><input value={f.username} onChange={(e) => setF({ ...f, username: e.target.value })} autoComplete="off" /></Field>
      <Field label="Initial password" hint="At least 14 characters. Share it securely; the user can change it."><input type="password" value={f.password} onChange={(e) => setF({ ...f, password: e.target.value })} autoComplete="new-password" /></Field>
      <Field label="Role"><select value={f.role} onChange={(e) => setF({ ...f, role: e.target.value })}><option value="viewer">Viewer</option><option value="operator">Operator</option><option value="admin">Admin</option></select></Field>
    </Dialog>
  );
}

function ResetPassword({ u, onClose }: { u: User; onClose: () => void }) {
  const [pw, setPw] = useState("");
  const { run, busy } = useAction();
  return (
    <Dialog title={`Reset password for ${u.username}`} onClose={onClose} footer={<><button onClick={onClose}>Cancel</button><button className="primary" disabled={pw.length < 14 || !!busy} onClick={async () => { const r = await run("r", () => api.patch(`/api/v1/users/${u.id}`, { role: u.role, disabled: u.disabled, password: pw }), "Password reset"); if (r) onClose(); }}>Reset password</button></>}>
      <Field label="New password" hint="At least 14 characters."><input type="password" value={pw} onChange={(e) => setPw(e.target.value)} autoComplete="new-password" /></Field>
    </Dialog>
  );
}

function TLSSection() {
  const sys = useApi<SystemInfo>("/api/v1/system");
  return (
    <Card title="TLS">
      {!sys.data ? <Skeleton /> : (
        <div className="stack">
          <dl className="kv"><dt>Subject</dt><dd>{sys.data.tls.subject}</dd><dt>Fingerprint (SHA-256)</dt><dd className="mono wrapany">{sys.data.tls.fingerprint}</dd><dt>Valid until</dt><dd>{time(sys.data.tls.not_after)}</dd><dt>Type</dt><dd>{sys.data.tls.self_signed ? "Self-signed, created by install.sh" : "Provided certificate"}</dd></dl>
          <p className="small muted">To use your own certificate, set tls_cert and tls_key in /etc/upwell/config.yaml and restart Upwell. Uploading a certificate here is coming soon.</p>
        </div>
      )}
    </Card>
  );
}

export function NotFound() {
  return <Page title="Not found"><Empty title="This page does not exist" action={<Link className="btn" to="/">Go home</Link>}>Check the address, or use Ctrl K to jump to a screen or migration.</Empty></Page>;
}
