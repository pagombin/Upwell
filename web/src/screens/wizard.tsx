import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import { bytes, count, duration } from "../lib/format";
import { useApi } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Connection, Database, Migration, MigrationView, SettingItem } from "../lib/types";
import { Card, ComingSoon, Empty, ErrorState, Field, Pill, Skeleton, Tile, Trunc, useAction, useToast } from "../components/ui";
import { PreflightResults, PreflightSummary, RunPreflightButton, usePreflight } from "./preflight";

const STEPS = ["Customer and permission", "Source", "Target", "Databases", "Options", "Preflight", "Plan review"];

function WizardSteps({ step, go, max }: { step: number; go: (n: number) => void; max: number }) {
  return (
    <nav className="stepper" aria-label="Wizard steps">
      {STEPS.map((s, i) => (
        <span key={s} style={{ display: "contents" }}>
          {i > 0 && <span className="sep" aria-hidden="true" />}
          <button className={`ghost st ${i < step ? "done" : i === step ? "current" : ""}`} style={{ border: 0, minHeight: 28 }} disabled={i > max} aria-current={i === step ? "step" : undefined} onClick={() => go(i)}>
            <span className="dot" aria-hidden="true" />{i + 1}. {s}
          </button>
        </span>
      ))}
    </nav>
  );
}

export function NewMigrationScreen() {
  const { id } = useParams();
  const [sp, setSp] = useSearchParams();
  const view = useApi<MigrationView>(id ? `/api/v1/migrations/${id}` : null);
  const step = Math.max(0, Math.min(6, Number(sp.get("step") || 0)));
  const go = (n: number) => setSp({ step: String(n) });
  const v = view.data;
  const m = v?.migration;
  if (m && m.flags.started) {
    return <div className="page"><Empty title="This migration has started" action={<Link className="btn" to={`/migrations/${m.short_id}`}>Open its overview</Link>}>Its definition is frozen once it starts.</Empty></div>;
  }
  const max = !v ? 0 : !v.permission ? 0 : !v.source ? 1 : !v.target ? 2 : !v.databases?.length ? 3 : 6;
  return (
    <div className="page">
      <div className="pagehead">
        <div className="titles"><h1>{m ? <Trunc>{m.name}</Trunc> : "New migration"}</h1><span className="subtitle">Step {step + 1} of 7: {STEPS[step]}</span></div>
        {m && <div className="actions"><span className="mono faint">{m.short_id}</span><Pill state={m.state} /></div>}
      </div>
      <WizardSteps step={step} go={go} max={max} />
      {id && view.error ? <ErrorState error={view.error} retry={view.refresh} /> : id && !v ? <Skeleton lines={8} /> : (
        <>
          {step === 0 && <StepPermission v={v} onDone={(short) => { if (!id) window.history.replaceState(null, "", `/migrations/${short}/setup?step=1`); view.refresh(); go(1); }} />}
          {step === 1 && v && <StepConnection v={v} kind="source" onDone={() => { view.refresh(); go(2); }} />}
          {step === 2 && v && <StepConnection v={v} kind="target" onDone={() => { view.refresh(); go(3); }} />}
          {step === 3 && v && <StepDatabases v={v} onDone={() => { view.refresh(); go(4); }} />}
          {step === 4 && v && <StepOptions v={v} onDone={() => { view.refresh(); go(5); }} />}
          {step === 5 && v && <StepPreflight mig={v.migration.id} onDone={() => go(6)} />}
          {step === 6 && v && <StepPlan v={v} />}
        </>
      )}
    </div>
  );
}

function Nav({ back, next, nextLabel = "Save and continue", busy, disabled }: { back?: () => void; next: () => void; nextLabel?: string; busy?: boolean; disabled?: boolean }) {
  return (
    <div className="row" style={{ justifyContent: "flex-end" }}>
      {back && <button onClick={back}>Back</button>}
      <button className="primary" onClick={next} disabled={busy || disabled}>{busy && <span className="spin" />}{nextLabel}</button>
    </div>
  );
}

function StepPermission({ v, onDone }: { v?: MigrationView; onDone: (shortId: string) => void }) {
  const nav = useNavigate();
  const p = v?.permission;
  const locked = !!p?.locked_at;
  const [name, setName] = useState(v?.migration.name || "");
  const [f, setF] = useState({ customer: p?.customer || "", account_id: p?.account_id || "", ticket: p?.ticket || "", granted_by: p?.granted_by || "", granted_at: p?.granted_at || new Date().toISOString().slice(0, 10), scope: p?.scope || "Copy every included database from the source cluster to the target cluster, then cut over.", notes: p?.notes || "" });
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => setF({ ...f, [k]: e.target.value });
  const save = async () => {
    setBusy(true); setErr(undefined);
    try {
      let mig: Migration | undefined = v?.migration;
      if (!mig) mig = await api.post<Migration>("/api/v1/migrations", { name });
      else if (name !== mig.name) await api.patch(`/api/v1/migrations/${mig.id}`, { name });
      if (!locked) await api.put(`/api/v1/migrations/${mig.id}/permission`, f);
      if (!v) nav(`/migrations/${mig.short_id}/setup?step=1`, { replace: true });
      else onDone(mig.short_id);
    } catch (e) { setErr(e as ApiError); } finally { setBusy(false); }
  };
  const missing = !name.trim() || ["customer", "account_id", "ticket", "granted_by", "granted_at", "scope"].some((k) => !f[k as keyof typeof f].trim());
  return (
    <Card title="Customer and permission record">
      <div className="stack">
        <p className="muted">Upwell connects to a customer's clusters only with a recorded permission. The record is locked once Upwell first connects, and it appears in the report.</p>
        {locked && <div className="callout info"><div className="body">The permission record is locked because Upwell has connected to the customer's clusters. Create a new migration if the permission changed.</div></div>}
        <Field label="Migration name"><input value={name} onChange={(e) => setName(e.target.value)} maxLength={120} /></Field>
        <div className="fieldgrid">
          <Field label="Customer"><input value={f.customer} onChange={set("customer")} disabled={locked} /></Field>
          <Field label="Customer account ID"><input value={f.account_id} onChange={set("account_id")} disabled={locked} /></Field>
          <Field label="Support ticket"><input value={f.ticket} onChange={set("ticket")} disabled={locked} /></Field>
          <Field label="Permission granted by"><input value={f.granted_by} onChange={set("granted_by")} disabled={locked} /></Field>
          <Field label="Permission granted on"><input type="date" value={f.granted_at} onChange={set("granted_at")} disabled={locked} /></Field>
        </div>
        <Field label="Scope of permission"><textarea value={f.scope} onChange={set("scope")} disabled={locked} /></Field>
        <Field label="Notes (optional)"><textarea value={f.notes} onChange={set("notes")} disabled={locked} /></Field>
        {err && <ErrorState error={err} />}
        <Nav next={save} busy={busy} disabled={missing} />
      </div>
    </Card>
  );
}

function StepConnection({ v, kind, onDone }: { v: MigrationView; kind: "source" | "target"; onDone: () => void }) {
  const c: Connection | undefined = kind === "source" ? v.source : v.target;
  const [f, setF] = useState({ host: c?.host || "", port: c?.port || 25060, user: c?.user || "doadmin", password: "", dbname: c?.dbname || "defaultdb", sslmode: c?.sslmode || "require", ca_cert: "", storage_gb: c?.storage_gb || 0 });
  const [results, setResults] = useState<{ name: string; ok: boolean; message: string }[]>();
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState<string>();
  const save = async () => {
    const body: Record<string, unknown> = { ...f, port: Number(f.port), storage_gb: Number(f.storage_gb) || 0 };
    await api.put(`/api/v1/migrations/${v.migration.id}/connections/${kind}`, body);
  };
  const test = async () => {
    setBusy("test"); setErr(undefined); setResults(undefined);
    try {
      if (f.password || !c) await save();
      setResults(await api.post(`/api/v1/migrations/${v.migration.id}/connections/${kind}/test`));
    } catch (e) { setErr(e as ApiError); } finally { setBusy(undefined); }
  };
  const next = async () => {
    setBusy("save"); setErr(undefined);
    try { if (f.password || !c) await save(); onDone(); } catch (e) { setErr(e as ApiError); } finally { setBusy(undefined); }
  };
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) => setF({ ...f, [k]: e.target.value });
  return (
    <Card title={kind === "source" ? "Source cluster" : "Target cluster"} actions={<span className="row small muted">Pick from the DigitalOcean API <ComingSoon what="DigitalOcean API discovery" /></span>}>
      <div className="stack">
        <p className="muted">{kind === "source" ? "The cluster you migrate from. The user needs the REPLICATION privilege; doadmin has it." : "The cluster you migrate to. The user must be able to apply replicated changes (replication origin functions and session_replication_role) and create databases."}</p>
        <div className="fieldgrid">
          <Field label="Host"><input value={f.host} onChange={set("host")} spellCheck={false} placeholder="db-postgresql-nyc3-12345-do-user-1-0.b.db.ondigitalocean.com" /></Field>
          <Field label="Port"><input type="number" value={f.port} onChange={set("port")} /></Field>
          <Field label="User"><input value={f.user} onChange={set("user")} autoComplete="off" /></Field>
          <Field label="Password" hint={c?.has_password ? "Stored encrypted. Leave empty to keep it." : "Stored encrypted on this droplet."}><input type="password" value={f.password} onChange={set("password")} autoComplete="new-password" /></Field>
          <Field label="Maintenance database"><input value={f.dbname} onChange={set("dbname")} /></Field>
          <Field label="TLS mode">
            <select value={f.sslmode} onChange={set("sslmode")}>
              <option value="verify-full">verify-full (needs the CA certificate)</option><option value="require">require</option><option value="prefer">prefer</option><option value="disable">disable (local testing only)</option>
            </select>
          </Field>
          {kind === "target" && <Field label="Target storage (GB)" hint="Used by the disk-space check. 0 means unknown."><input type="number" value={f.storage_gb} onChange={set("storage_gb")} /></Field>}
        </div>
        {f.sslmode === "verify-full" && <Field label="CA certificate (PEM)"><textarea className="mono" value={f.ca_cert} onChange={set("ca_cert")} spellCheck={false} /></Field>}
        <div className="row"><button onClick={test} disabled={!!busy || !f.host || (!f.password && !c?.has_password)}>{busy === "test" && <span className="spin" />}Test connection</button></div>
        {results && (
          <ul className="opsteps" aria-label="Connection test results">
            {results.map((r) => (
              <li key={r.name} className={r.ok ? "done" : "failed"}><span className="num" aria-hidden="true">{r.ok ? "✓" : "!"}</span><span className="wrapany">{r.message}</span><span className="mono small faint">{r.name}</span></li>
            ))}
          </ul>
        )}
        {err && <ErrorState error={err} />}
        <Nav next={next} busy={busy === "save"} disabled={!f.host || (!f.password && !c?.has_password)} />
      </div>
    </Card>
  );
}

function StepDatabases({ v, onDone }: { v: MigrationView; onDone: () => void }) {
  const [rows, setRows] = useState<Database[]>(v.databases || []);
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState<string>();
  const discover = async () => {
    setBusy("discover"); setErr(undefined);
    try { setRows(await api.post<Database[]>(`/api/v1/migrations/${v.migration.id}/discover`)); } catch (e) { setErr(e as ApiError); } finally { setBusy(undefined); }
  };
  useEffect(() => { if (!v.databases?.length) discover(); /* first visit */ // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const save = async () => {
    setBusy("save"); setErr(undefined);
    try {
      await api.put(`/api/v1/migrations/${v.migration.id}/databases`, rows.map((r) => ({ source_name: r.source_name, target_name: r.target_name, include: r.include })));
      onDone();
    } catch (e) { setErr(e as ApiError); } finally { setBusy(undefined); }
  };
  const upd = (i: number, p: Partial<Database>) => setRows(rows.map((r, j) => (j === i ? { ...r, ...p } : r)));
  const inc = rows.filter((r) => r.include);
  return (
    <Card title="Databases" actions={<button onClick={discover} disabled={!!busy}>{busy === "discover" && <span className="spin" />}Discover again</button>}>
      <div className="stack">
        <p className="muted">Every database on the source with its size. Each included database gets its own engine process, replication slot and origin. Target names default to the source names.</p>
        {busy === "discover" && rows.length === 0 ? <Skeleton lines={5} /> : rows.length === 0 ? <Empty title="No databases found">Check the source connection, then discover again.</Empty> : (
          <div className="tablewrap">
            <table className="data" aria-label="Databases to migrate">
              <thead><tr><th style={{ width: 70 }}>Include</th><th>Source database</th><th>Target database</th><th className="num">Size</th><th className="num">Tables</th><th className="num">Rows (est.)</th><th>Note</th></tr></thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={r.source_name}>
                    <td><input type="checkbox" aria-label={`Include ${r.source_name}`} checked={r.include} disabled={!!r.skip_reason && !r.include && r.skip_reason.startsWith("system")} onChange={(e) => upd(i, { include: e.target.checked })} /></td>
                    <td style={{ maxWidth: 280 }}><Trunc className="mono">{r.source_name}</Trunc></td>
                    <td style={{ maxWidth: 300 }}><input aria-label={`Target name for ${r.source_name}`} value={r.target_name} onChange={(e) => upd(i, { target_name: e.target.value })} disabled={!r.include} style={{ width: "100%" }} className="mono" maxLength={63} /></td>
                    <td className="num" title={`${r.size_bytes.toLocaleString("en-US")} bytes`}>{bytes(r.size_bytes)}</td>
                    <td className="num">{count(r.table_count)}</td>
                    <td className="num" title={r.rows_estimate.toLocaleString("en-US")}>{count(r.rows_estimate)}</td>
                    <td style={{ maxWidth: 260 }}>{r.skip_reason ? <Trunc className="muted">{r.skip_reason}</Trunc> : ""}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div className="row muted small">{inc.length} included · {bytes(inc.reduce((a, r) => a + r.size_bytes, 0))}</div>
        {err && <ErrorState error={err} />}
        <Nav next={save} busy={busy === "save"} disabled={inc.length === 0} />
      </div>
    </Card>
  );
}

const OPTION_KEYS = ["decoding_plugin", "table_jobs", "index_jobs", "max_concurrent_base_copies", "split_tables_larger_than", "heartbeat", "copy_roles", "create_target_databases", "exact_count_max_mb", "verify_checksum_budget_seconds", "cutover_lag_threshold_mb", "analyze_after_go"];

function StepOptions({ v, onDone }: { v: MigrationView; onDone: () => void }) {
  const cat = useApi<SettingItem[]>("/api/v1/settings");
  const eff = v.settings_effective || {};
  const [vals, setVals] = useState<Record<string, unknown>>(() => ({ ...eff }));
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const items = (cat.data || []).filter((i) => OPTION_KEYS.includes(i.key));
  const save = async () => {
    setBusy(true); setErr(undefined);
    const changed: Record<string, unknown> = {};
    for (const i of items) if (JSON.stringify(vals[i.key]) !== JSON.stringify(eff[i.key])) changed[i.key] = vals[i.key];
    try {
      if (Object.keys(changed).length) await api.patch(`/api/v1/migrations/${v.migration.id}`, { settings: changed });
      if (vals.heartbeat === false) toast("info", "Heartbeat is off", "GO will rely on data comparison only; this is recorded as an accepted risk.");
      onDone();
    } catch (e) { setErr(e as ApiError); } finally { setBusy(false); }
  };
  return (
    <Card title="Options">
      <div className="stack">
        <div className="row">
          <Field label="Mode"><select value="online" onChange={() => {}} aria-describedby="mode-hint"><option value="online">Online: copy, then stream changes until cutover</option></select></Field>
          <span className="small muted" id="mode-hint">Offline mode <ComingSoon what="Offline mode" /></span>
        </div>
        {cat.error ? <ErrorState error={cat.error} retry={cat.refresh} /> : !cat.data ? <Skeleton lines={6} /> : (
          <div className="fieldgrid">
            {items.map((i) => <SettingInput key={i.key} item={i} value={vals[i.key]} onChange={(x) => setVals({ ...vals, [i.key]: x })} />)}
          </div>
        )}
        {vals.heartbeat === false && <div className="callout warn"><div className="body"><strong>Heartbeat off</strong><span>The final heartbeat proves the last writes reached the target. Without it, GO relies on data comparison alone and the report records the accepted risk.</span></div></div>}
        <div className="row small muted"><span>Watched tables</span><ComingSoon what="Watched tables" /><span>Notifications</span><ComingSoon what="Notifications" /></div>
        {err && <ErrorState error={err} />}
        <Nav next={save} busy={busy} />
      </div>
    </Card>
  );
}

export function SettingInput({ item, value, onChange, disabled }: { item: SettingItem; value: unknown; onChange: (v: unknown) => void; disabled?: boolean }) {
  const id = `set-${item.key}`;
  const lbl = item.key.replace(/_/g, " ").replace(/^./, (c) => c.toUpperCase());
  let input;
  if (item.type === "bool") input = <label className="check"><input id={id} type="checkbox" checked={!!value} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />{value ? "On" : "Off"}</label>;
  else if (item.type === "enum") input = <select id={id} value={String(value ?? "")} disabled={disabled} onChange={(e) => onChange(e.target.value)}>{item.enum!.map((x) => <option key={x}>{x}</option>)}</select>;
  else if (item.type === "int" || item.type === "float") input = <input id={id} type="number" value={value === undefined || value === null ? "" : String(value)} min={item.min} max={item.max} step={item.type === "float" ? "any" : 1} disabled={disabled} onChange={(e) => onChange(e.target.value === "" ? null : Number(e.target.value))} />;
  else input = <input id={id} value={String(value ?? "")} disabled={disabled} onChange={(e) => onChange(e.target.value)} />;
  return (
    <div className="field" style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <label className="lbl small muted" htmlFor={id} style={{ fontWeight: 500 }}>{lbl}{item.unit ? ` (${item.unit})` : ""}</label>
      {input}
      <span className="hint">{item.description}{item.min !== undefined && item.max !== undefined ? ` Range ${item.min} to ${item.max}.` : ""}</span>
    </div>
  );
}

function StepPreflight({ mig, onDone }: { mig: string; onDone: () => void }) {
  const l = usePreflight(mig);
  const run = l.data?.run;
  return (
    <Card title="Preflight" actions={<RunPreflightButton mig={mig} onStarted={l.refresh} label={run ? "Run again" : "Run preflight"} />}>
      <div className="stack">
        {l.error ? <ErrorState error={l.error} retry={l.refresh} /> : !l.data ? <Skeleton lines={5} /> : !run ? (
          <Empty title="Run preflight to continue">Preflight checks the source, target, this droplet and every database. Blockers stop the start; acceptable blockers can be accepted with a reason.</Empty>
        ) : <>
          <PreflightSummary run={run} />
          <PreflightResults mig={mig} latest={l.data} refresh={l.refresh} />
        </>}
        <Nav next={onDone} nextLabel="Continue to plan review" disabled={!run || run.state === "running"} />
      </div>
    </Card>
  );
}

interface Plan { databases: { database: string; target: string; plan: { command: string; unit: string }; size_bytes: number; estimated_copy_seconds: number; plugin: string }[]; estimates: Record<string, number | string> }

function StepPlan({ v }: { v: MigrationView }) {
  const plan = useApi<Plan>(`/api/v1/migrations/${v.migration.id}/plan`);
  const pf = usePreflight(v.migration.id);
  const nav = useNavigate();
  const { can } = useSession();
  const { run, busy } = useAction();
  const [reviewed, setReviewed] = useState(false);
  const sum = pf.data?.run?.summary;
  const e = plan.data?.estimates || {};
  const start = async () => {
    const r = await run("start", () => api.post(`/api/v1/migrations/${v.migration.id}/start`, { warnings_reviewed: reviewed }), "Migration started");
    if (r) nav(`/migrations/${v.migration.short_id}`);
  };
  return (
    <Card title="Plan review">
      <div className="stack">
        {plan.error ? <ErrorState error={plan.error} retry={plan.refresh} /> : !plan.data ? <Skeleton lines={6} /> : <>
          <div className="grid cols-4">
            <Tile label="Data to copy" value={bytes(Number(e.total_bytes))} sub={`${plan.data.databases.length} databases, ${e.concurrent} at once`} />
            <Tile label="Base copy estimate" value={duration(Number(e.estimated_copy_seconds) * 1000 / Math.max(1, Number(e.concurrent)))} sub={`At ${e.assumed_mibs} MiB/s per database`} />
            <Tile label="Target disk needed" value={bytes(Number(e.target_disk_needed_bytes))} sub="Data plus indexes, with headroom" />
            <Tile label="Peak connections" value={`${e.source_connections_peak} / ${e.target_connections_peak}`} sub={`Source / target · ${e.table_jobs} table and ${e.index_jobs} index jobs`} />
          </div>
          <p className="small muted">WAL retained on the source during the base copy: {e.wal_retained_during_copy_bytes ? `about ${bytes(Number(e.wal_retained_during_copy_bytes))} at the current source write rate` : "depends on the source write rate, which is measured once the migration runs; the slot headroom check watches it."}</p>
          <h3>Engine command per database</h3>
          {plan.data.databases.map((d) => (
            <div key={d.database} className="stack" style={{ gap: 6 }}>
              <div className="row small"><strong className="mono"><Trunc>{d.database}</Trunc></strong><span className="muted">→ <span className="mono">{d.target}</span> · {bytes(d.size_bytes)} · about {duration(d.estimated_copy_seconds * 1000)} · plugin {d.plugin}</span></div>
              <pre className="cmd">{d.plan.command}</pre>
            </div>
          ))}
        </>}
        {sum && !sum.can_start && <div className="callout crit"><div className="body"><strong>Preflight does not allow a start yet</strong><span>Resolve or accept every blocker, then run preflight again.</span></div></div>}
        {sum?.needs_warning_review && <label className="check"><input type="checkbox" checked={reviewed} onChange={(x) => setReviewed(x.target.checked)} />I reviewed the {sum.warnings} preflight warnings</label>}
        <div className="row" style={{ justifyContent: "flex-end" }}>
          <span className="small muted">Queue instead <ComingSoon what="Queueing several migrations" /></span>
          {can("operator") && <button className="primary" disabled={!sum?.can_start || (sum.needs_warning_review && !reviewed) || !!busy} onClick={start}>{busy && <span className="spin" />}Start migration</button>}
        </div>
      </div>
    </Card>
  );
}
