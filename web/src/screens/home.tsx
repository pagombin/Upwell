import { api } from "../lib/api";
import { Link } from "react-router-dom";
import { copyEtaMs, LIFECYCLE, lifecycleBad, lifecycleIndex, overallProgress, readyForCutover } from "../lib/derive";
import { ago, bytes, duration, pct, time } from "../lib/format";
import { useApi, useNow } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Alert, MigrationView, Series } from "../lib/types";
import { Card, Empty, ErrorState, Pill, ProgressBar, Skeleton, StaleBadge, Tile, Trunc, useAction } from "../components/ui";

export interface SystemInfo {
  version: string; go: string; started_at: number; uptime_s: number;
  engine: { name: string; version: string; runner: string }; tools: Record<string, string>;
  store: { path: string; bytes: number; schema_version: number }; logs: { dir: string; bytes: number }; runs: { dir: string; bytes: number };
  watchdog: { healthy: boolean; late: number; beats: number; systemd: boolean }; ready: boolean; ready_detail: Record<string, unknown>;
  tls: { fingerprint: string; not_after: number; subject: string; self_signed: boolean }; rebooted: boolean; reconciled: boolean;
  updates: { available: boolean; note: string }; dev: boolean;
}

export function Stepper({ index, bad }: { index: number; bad?: boolean }) {
  return (
    <div className="stepper" aria-label="Lifecycle">
      {LIFECYCLE.map((s, i) => (
        <span key={s.key} style={{ display: "contents" }}>
          {i > 0 && <span className="sep" aria-hidden="true" />}
          <span className={`st ${i < index ? "done" : i === index ? `current ${bad ? "bad" : i === 4 || i === 5 ? "cut" : ""}` : ""}`} aria-current={i === index ? "step" : undefined}>
            <span className="dot" aria-hidden="true" />{s.label}
          </span>
        </span>
      ))}
    </div>
  );
}

function ActiveCard({ id, alerts }: { id: string; alerts: Alert[] }) {
  const v = useApi<MigrationView>(`/api/v1/migrations/${id}`, 5000);
  const now = useNow(5000);
  const from = now - 15 * 60e3;
  const mx = useApi<Series[]>(`/api/v1/migrations/${id}/metrics?names=target_size_bytes&from=${Math.floor(from / 60000) * 60000}`, 30000);
  if (v.error) return <ErrorState error={v.error} retry={v.refresh} />;
  if (!v.data) return <Card title="Active migration"><Skeleton lines={4} /></Card>;
  const d = v.data, m = d.migration;
  const prog = overallProgress(d);
  const eta = copyEtaMs(d, mx.data);
  const open = alerts.filter((a) => a.migration_id === m.id);
  const ready = readyForCutover(d);
  return (
    <Card title={<Link to={`/migrations/${m.short_id}`}><Trunc>{m.name}</Trunc></Link>} id={`active-mig-${m.short_id}`} actions={<><StaleBadge updatedAt={v.updatedAt} now={now} /><Pill state={m.state} /></>}>
      <div className="stack">
        <Stepper index={lifecycleIndex(d)} bad={lifecycleBad(d)} />
        <div className="grid cols-4">
          <Tile label="Base copy" value={pct(prog * 100)} sub={<ProgressBar value={prog} label="Base copy progress" />} />
          <Tile label="Copy ETA" value={eta ? duration(eta) : prog >= 1 ? "Done" : "—"} sub={eta ? `at ${time(now + eta, false)}` : "Estimated from target growth"} />
          <Tile label="Databases in sync" value={`${d.state_counts["in_sync"] || 0} of ${d.included}`} sub={ready ? "Ready for cutover" : "Cutover needs every database in sync"} />
          <Tile label="Open alerts" value={open.length} sub={open[0] ? <Trunc>{open[0].message}</Trunc> : "None firing"} />
        </div>
      </div>
    </Card>
  );
}

export function HomeScreen() {
  const migs = useApi<MigrationView[]>("/api/v1/migrations", 10000);
  const sys = useApi<SystemInfo>("/api/v1/system", 30000);
  const host = useApi<Series[]>(`/api/v1/host/metrics?names=work_volume_pct,work_volume_free_bytes&from=${Math.floor(Date.now() / 600000) * 600000 - 600000}`, 30000);
  const alerts = useApi<Alert[]>("/api/v1/alerts?state=firing", 15000);
  const { can } = useSession();
  const now = useNow(10000);
  const list = migs.data || [];
  const inProgress = (v: MigrationView) => v.migration.flags.started && !v.migration.flags.cleaned_up && !v.migration.flags.aborted && !v.migration.flags.verdict;
  const real = list.filter((v) => inProgress(v) && !v.migration.fixture);
  const activeShown = real.length ? real : list.filter(inProgress);
  const disk = host.data?.find((s) => s.name === "work_volume_pct")?.points.slice(-1)[0]?.v;
  const free = host.data?.find((s) => s.name === "work_volume_free_bytes")?.points.slice(-1)[0]?.v;
  const certDays = sys.data ? Math.floor((sys.data.tls.not_after - now) / 86400e3) : undefined;
  return (
    <div className="page">
      <div className="pagehead">
        <div className="titles"><h1>Home</h1><span className="subtitle">Everything running on this droplet at a glance.</span></div>
        <div className="actions">{can("operator") && <Link className="btn primary" to="/migrations/new">New migration</Link>}</div>
      </div>
      {migs.error ? <ErrorState error={migs.error} retry={migs.refresh} /> : !migs.data ? <Card title="Active migration"><Skeleton lines={4} /></Card> : activeShown.length === 0 ? (
        <Empty title="No migration is running" action={can("operator") ? <Link className="btn" to="/migrations/new">Define a migration</Link> : undefined}>
          Define a migration, run preflight and start it. Its progress, readiness for cutover and alerts appear here.
        </Empty>
      ) : activeShown.slice(0, 2).map((v) => <ActiveCard key={v.migration.id} id={v.migration.id} alerts={alerts.data || []} />)}
      <div className="grid cols-4" aria-label="System health">
        {sys.error ? <ErrorState error={sys.error} retry={sys.refresh} /> : !sys.data ? <><Skeleton /><Skeleton /><Skeleton /><Skeleton /></> : <>
          <Tile label="Work volume" value={disk !== undefined ? pct(disk) : "—"} sub={free !== undefined ? `${bytes(free)} free` : "Waiting for the first sample"} title={free !== undefined ? `${Math.round(free).toLocaleString("en-US")} bytes free` : undefined} />
          <Tile label="Service" value={sys.data.watchdog.healthy ? "Healthy" : "Late"} sub={`Up ${duration(sys.data.uptime_s * 1000)}${sys.data.watchdog.systemd ? " · watchdog on" : " · no systemd watchdog"}`} />
          <Tile label="Certificate" value={certDays !== undefined ? `${certDays} days` : "—"} sub={sys.data.tls.self_signed ? "Self-signed, valid until " + time(sys.data.tls.not_after, true).split(" ").slice(0, 3).join(" ") : "Valid until " + time(sys.data.tls.not_after)} />
          <Tile label="Engine" value={<Trunc>{sys.data.engine.version || "not installed"}</Trunc>} sub={`Upwell ${sys.data.version}`} />
        </>}
      </div>
      <QueueCard />
      <Card title="Recent migrations">
        {!migs.data ? <Skeleton lines={4} /> : list.length === 0 ? <Empty title="No migrations yet">Migrations you define appear here with their state and verdict.</Empty> : (
          <div className="tablewrap">
            <table className="data" aria-label="Recent migrations">
              <thead><tr><th>Name</th><th>ID</th><th>State</th><th className="num">Databases</th><th className="num">Size</th><th>Verdict</th><th>Updated</th></tr></thead>
              <tbody>
                {list.slice(0, 20).map((v) => (
                  <tr key={v.migration.id}>
                    <td style={{ maxWidth: 360 }}><Link to={`/migrations/${v.migration.short_id}`}><Trunc>{v.migration.name}</Trunc></Link>{v.migration.fixture && <span className="pill neutral nodot" style={{ marginLeft: 8 }}>Demo</span>}</td>
                    <td className="mono">{v.migration.short_id}</td>
                    <td><Pill state={v.migration.state} /></td>
                    <td className="num">{v.included}</td>
                    <td className="num" title={`${v.total_bytes.toLocaleString("en-US")} bytes`}>{bytes(v.total_bytes)}</td>
                    <td>{v.migration.flags.verdict ? <Pill state={v.migration.flags.verdict} text={v.migration.flags.verdict} /> : <span className="faint">—</span>}</td>
                    <td title={time(v.migration.updated_at)}>{ago(v.migration.updated_at, now)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}

interface QueueView { running: { id: string; short_id: string; name: string; state: string } | null; entries: { migration_id: string; short_id: string; name: string; position: number; auto_start: boolean; queued_by: string; queued_at: number }[] }

// The run lock and the queue (overnight item 7): one migration runs at a
// time; queued ones start when an operator starts them, or by themselves
// when their entry has auto-start.
function QueueCard() {
  const q = useApi<QueueView>("/api/v1/queue", 5000);
  const { can } = useSession();
  const { run, busy } = useAction();
  const op = can("operator");
  const move = (i: number, d: number) => {
    const ids = (q.data?.entries || []).map((e) => e.migration_id);
    const j = i + d;
    if (j < 0 || j >= ids.length) return;
    [ids[i], ids[j]] = [ids[j], ids[i]];
    run("order", () => api.put("/api/v1/queue/order", { ids })).then(q.refresh);
  };
  return (
    <Card title="Queue">
      {q.error ? <ErrorState error={q.error} retry={q.refresh} /> : !q.data ? <Skeleton lines={3} /> : (
        <div className="stack">
          <p className="small muted">{q.data.running ? <>Running now: <Link to={`/migrations/${q.data.running.short_id}`}>{q.data.running.name}</Link>. One migration runs at a time; the next starts when it ends GO, is aborted or is cleaned up.</> : "Nothing is running: any ready migration can start."}</p>
          {q.data.entries.length === 0 ? <p className="muted">No migrations are queued. Queue one from its plan review.</p> : (
            <div className="tablewrap">
              <table className="data" aria-label="Queue">
                <thead><tr><th>#</th><th>Migration</th><th>Queued</th><th>Auto-start</th><th><span className="sr-only">Actions</span></th></tr></thead>
                <tbody>{q.data.entries.map((e, i) => (
                  <tr key={e.migration_id}>
                    <td>{i + 1}</td>
                    <td style={{ maxWidth: 280 }}><Link to={`/migrations/${e.short_id}`}><Trunc>{e.name}</Trunc></Link></td>
                    <td>{time(e.queued_at)} <span className="small muted">by {e.queued_by}</span></td>
                    <td><label className="check"><input type="checkbox" aria-label={`Auto-start ${e.name}`} checked={e.auto_start} disabled={!op || !!busy} onChange={(x) => run("auto", () => api.patch(`/api/v1/migrations/${e.migration_id}/queue`, { auto_start: x.target.checked })).then(q.refresh)} />{e.auto_start ? "On" : "Off"}</label></td>
                    <td>{op && <div className="row" style={{ gap: 6, flexWrap: "wrap" }}>
                      <button className="ghost" aria-label={`Move ${e.name} up`} disabled={i === 0 || !!busy} onClick={() => move(i, -1)}>Up</button>
                      <button className="ghost" aria-label={`Move ${e.name} down`} disabled={i === q.data!.entries.length - 1 || !!busy} onClick={() => move(i, 1)}>Down</button>
                      <button className="ghost" disabled={!!busy} onClick={() => run("unq", () => api.del(`/api/v1/migrations/${e.migration_id}/queue`), "Taken out of the queue").then(q.refresh)}>Unqueue</button>
                    </div>}</td>
                  </tr>
                ))}</tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </Card>
  );
}
