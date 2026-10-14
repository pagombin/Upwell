import { Link } from "react-router-dom";
import { dbCopyEtaMs, readyForCutover } from "../lib/derive";
import { bytes, duration, rate, time } from "../lib/format";
import type { Database, MigrationView, Series, SnapshotBlocker } from "../lib/types";
import { CrosshairProvider, seriesFor, TimeChart } from "./charts";
import { Card, label } from "./ui";

// The migration overview's first view (docs/design/migration-overview.md):
// is my data safe, is it on track, does anything need me, what happens next.

const STALE_MS = 60_000;
const ATTENTION = new Set(["failed", "degraded", "restart_required"]);
const STREAMING = new Set(["catch_up", "in_sync", "draining"]);

export type Tone = "ok" | "warn" | "crit" | "neutral";

export interface Signal { label: string; value: string; sub: string; tone: Tone; title?: string }

function worst(a: Tone, b: Tone): Tone {
  const r: Record<Tone, number> = { neutral: 0, ok: 1, warn: 2, crit: 3 };
  return r[a] >= r[b] ? a : b;
}

// lagOf is how far behind the target is in time: the age of the newest
// heartbeat on the target (what a cutover waits for).
export function lagOf(v: MigrationView, d: Database): number | undefined {
  const h = v.live?.[d.source_name]?.heartbeat_latency_s;
  return STREAMING.has(d.state) && h !== undefined ? h : undefined;
}

export function sampleAge(v: MigrationView, d: Database, now: number): number | undefined {
  const at = v.live?.[d.source_name]?.sampled_at;
  return at ? Math.max(0, now - at) : undefined;
}

export function safetySignals(v: MigrationView, now: number): Signal[] {
  const m = v.migration;
  const inc = v.databases.filter((d) => d.include);
  const out: Signal[] = [];
  // 1. How far behind the target is, in time.
  const lags = inc.map((d) => ({ d, lag: lagOf(v, d) })).filter((x) => x.lag !== undefined) as { d: Database; lag: number }[];
  if (lags.length === 0) {
    out.push({ label: "Target behind by", value: "—", sub: inc.some((d) => d.state === "base_copy" || d.state === "preparing") ? "Measured once a database streams" : "No database is streaming", tone: "neutral" });
  } else {
    const w = lags.reduce((a, b) => (b.lag > a.lag ? b : a));
    const tone: Tone = w.lag > 300 ? "crit" : w.lag > 60 ? "warn" : "ok";
    out.push({ label: "Target behind by", value: duration(w.lag * 1000), sub: lags.length > 1 ? `worst: ${w.d.source_name}` : "age of the last heartbeat on the target", tone, title: "Time for a row written to the source to appear on the target, from the heartbeat table." });
  }
  // 2. Freshness of what this page shows.
  const srcStale = v.cluster?.source_stale_samples || 0, dstStale = v.cluster?.target_stale_samples || 0;
  const ages = inc.map((d) => sampleAge(v, d, now)).filter((a): a is number => a !== undefined);
  const oldest = ages.length ? Math.max(...ages) : undefined;
  if (srcStale >= 1 || dstStale >= 1) {
    const side = srcStale && dstStale ? "source and target" : srcStale ? "source" : "target";
    out.push({ label: "Data freshness", value: "Stale", sub: `the ${side} did not answer the last ${Math.max(srcStale, dstStale)} sample(s)`, tone: Math.max(srcStale, dstStale) >= 3 ? "crit" : "warn" });
  } else if (oldest === undefined) {
    out.push({ label: "Data freshness", value: "—", sub: m.flags.started ? "Waiting for the first sample" : "Not started", tone: "neutral" });
  } else {
    out.push({ label: "Data freshness", value: `${duration(oldest)} ago`, sub: oldest > STALE_MS ? "older than a minute: values may be out of date" : "every database sampled", tone: oldest > STALE_MS ? "warn" : "ok" });
  }
  // 3. Keeping up: apply against the source's write rate.
  const applying = inc.filter((d) => STREAMING.has(d.state));
  const apply = applying.reduce((a, d) => a + (v.live?.[d.source_name]?.replay_rate || 0), 0);
  const wal = v.cluster?.src_wal_rate;
  if (!applying.length) {
    out.push({ label: "Keeping up", value: "—", sub: "Measured once a database streams", tone: "neutral" });
  } else {
    const growing = applying.some((d) => (v.live?.[d.source_name]?.backlog_bytes ?? 0) > 64 * 2 ** 20);
    out.push({ label: "Keeping up", value: rate(apply), sub: `applied; source writes ${rate(wal)} (whole cluster)`, tone: growing ? "warn" : "ok", title: "Changes applied to the target per second, against the source cluster's WAL rate (which includes databases not migrated)." });
  }
  // 4. Verification coverage, once there is a verdict.
  const vd = m.flags.verdict;
  if (!vd) {
    out.push({ label: "Verification", value: m.flags.verifying ? "Running" : "Not yet", sub: "runs at the cutover, after the drain", tone: "neutral" });
  } else if (vd === "NO-GO") {
    out.push({ label: "Verification", value: "NO-GO", sub: "a data-safety check failed", tone: "crit" });
  } else if ((m.flags.unverified_count || 0) > 0) {
    out.push({ label: "Verification", value: "GO, partial", sub: `${m.flags.unverified_count} table(s) not compared row by row`, tone: "warn" });
  } else {
    out.push({ label: "Verification", value: "GO", sub: "every row of every table compared", tone: "ok" });
  }
  return out;
}

// blockerText names one session a slot creation waits for.
export function blockerText(b: SnapshotBlocker): string {
  return `PID ${b.pid}${b.application ? ` (${b.application})` : ""}, ${b.user} in ${b.database}, transaction open ${duration(b.open_seconds * 1000)}${b.state ? `, ${b.state}` : ""}${b.client ? `, from ${b.client}` : ""}`;
}

export interface Summary { state: string; tone: Tone; actions: { db: string; text: string; to: string }[]; next: string }

export function statusSummary(v: MigrationView, metrics: Series[] | undefined, now: number): Summary {
  const m = v.migration;
  const inc = v.databases.filter((d) => d.include);
  const n = inc.length;
  const count = (f: (d: Database) => boolean) => inc.filter(f).length;
  const copying = inc.filter((d) => d.state === "base_copy" || d.state === "preparing");
  const syncing = count((d) => d.state === "in_sync");
  const attention = inc.filter((d) => ATTENTION.has(d.state) || (d.state === "stopped" && !d.next_retry_at));
  const base = `/migrations/${m.short_id}/databases/`;
  const actions = attention.map((d) => ({
    db: d.source_name,
    to: base + encodeURIComponent(d.source_name),
    text: d.state === "restart_required" ? "needs a restart from zero" : d.state === "stopped" ? "is stopped: resume it" : `${label(d.state).toLowerCase()}: open it to see why and resume`,
  }));
  // A base copy that cannot start: the slot creation waits for open transactions.
  for (const d of inc) {
    const w = v.snapshot_waits?.[d.source_name];
    if (!w || attention.includes(d)) continue;
    const b = w.blockers[0];
    actions.push({ db: d.source_name, to: base + encodeURIComponent(d.source_name),
      text: `cannot start its base copy: the slot creation has waited ${duration(now - w.since)} for ${w.blockers.length} open transaction${w.blockers.length === 1 ? "" : "s"}${b ? ` (oldest: ${blockerText(b)})` : ""}. Ask the customer to end ${w.blockers.length === 1 ? "it" : "them"}.` });
  }
  let state = "", tone: Tone = "ok", next = "";
  const c = m.flags.cutover;
  if (!m.flags.started) {
    state = "Not started"; tone = "neutral";
    next = m.flags.preflight_ok ? "Start the migration from the plan review." : "Finish the setup and pass preflight.";
  } else if (m.flags.verdict) {
    const partial = m.flags.verdict === "GO" && (m.flags.unverified_count || 0) > 0;
    state = m.flags.verdict === "GO" ? (partial ? "Cutover finished: GO, not fully verified" : "Cutover finished: GO") : "Cutover finished: NO-GO";
    tone = m.flags.verdict === "GO" ? (partial ? "warn" : "ok") : "crit";
    next = m.flags.verdict === "GO" ? (partial ? "Verify every table fully before switching applications, or switch if those tables were checked another way." : m.flags.apps_switched ? "Clean up the slots, origins and heartbeat schema." : "Point applications at the target, then mark them switched.") : "Do not switch applications. Read the failed checks on the Verification tab; writes can resume on the source.";
  } else if (c && !c.ended_at) {
    state = `Cutover in progress (${c.phase})`; tone = "warn";
    next = "Writes must stay stopped on the source until the verdict.";
  } else if (m.flags.paused) {
    state = "Paused"; tone = "warn"; next = "Resume the migration; the slots keep source WAL while it is paused.";
  } else if (m.flags.aborted) {
    state = "Aborted"; tone = "crit"; next = "Clean up to remove the slots, publications and origins.";
  } else {
    const parts = [];
    if (copying.length) parts.push(`${copying.length} copying`);
    if (syncing) parts.push(`${syncing} in sync`);
    const catching = count((d) => d.state === "catch_up");
    if (catching) parts.push(`${catching} catching up`);
    if (attention.length) parts.push(`${attention.length} need${attention.length === 1 ? "s" : ""} attention`);
    state = `${n} database${n === 1 ? "" : "s"}: ${parts.join(", ") || label(m.state)}`;
    tone = attention.length ? "crit" : actions.length ? "warn" : "ok";
    if (readyForCutover(v)) {
      next = "Every database is in sync: the cutover can start when the customer is ready (Cutover tab).";
    } else if (copying.length) {
      const etas = copying.map((d) => ({ d, eta: dbCopyEtaMs(d, v.live?.[d.source_name], metrics) })).filter((x) => x.eta !== undefined) as { d: Database; eta: number }[];
      const last = etas.length ? etas.reduce((a, b) => (b.eta > a.eta ? b : a)) : undefined;
      next = last ? `${last.d.source_name} finishes its base copy in about ${duration(last.eta)} (around ${time(now + last.eta, false)}); then every database streams and the cutover can start.` : "The base copy is running; its ETA appears after a few minutes of target growth.";
    } else if (attention.length) {
      next = "The cutover needs every database in sync: deal with the databases above first.";
    } else {
      next = "Databases are catching up; the cutover can start once every one is in sync.";
    }
  }
  if (attention.length && tone !== "crit") tone = worst(tone, "crit");
  return { state, tone, actions, next };
}

export function StatusSummary({ s }: { s: Summary }) {
  return (
    <div className={`callout ${s.tone === "crit" ? "crit" : s.tone === "warn" ? "warn" : s.tone === "ok" ? "ok" : "info"}`} role="status" data-testid="status-summary">
      <div className="body">
        <strong>{s.state}</strong>
        {s.actions.length > 0 ? (
          <ul className="stack" style={{ margin: 0, paddingLeft: 18, gap: 2 }} aria-label="Needs your attention">
            {s.actions.map((a) => <li key={a.db}><Link to={a.to} className="mono">{a.db}</Link> {a.text}</li>)}
          </ul>
        ) : <span>Nothing needs you right now.</span>}
        <span><span className="muted">Next:</span> {s.next}</span>
      </div>
    </div>
  );
}

export function SafetyStrip({ signals }: { signals: Signal[] }) {
  return (
    <div className="grid cols-4" aria-label="Data safety">
      {signals.map((x) => (
        <div key={x.label} className={`tile tone-${x.tone}`} title={x.title}>
          <span className="label">{x.label}</span>
          <span className="value">{x.value}</span>
          <span className="sub">{x.sub}</span>
        </div>
      ))}
    </div>
  );
}

// Trends over the run: backlog and lag per database, apply against source writes.
export function TrendsCard({ metrics, from, to }: { metrics: Series[] | undefined; from: number; to: number }) {
  const applied = seriesFor(metrics, "replay_rate");
  const sum = new Map<number, number>();
  for (const s of applied) for (const p of s.points) { const k = Math.round(p.ts / 30000) * 30000; sum.set(k, (sum.get(k) || 0) + p.v); }
  const appliedTotal = { name: "applied (all databases)", points: [...sum.entries()].sort((a, b) => a[0] - b[0]).map(([ts, v]) => ({ ts, v })) };
  const wal = seriesFor(metrics, "src_wal_rate").map((s) => ({ ...s, name: "source WAL (whole cluster)" }));
  return (
    <Card title="Trends over the run">
      <CrosshairProvider>
        <div className="grid cols-3">
          <TimeChart title="Target behind by" series={seriesFor(metrics, "heartbeat_latency_s")} fmt={(n) => duration(n * 1000)} from={from} to={to}
            empty="Age of the last heartbeat on the target, per database. Measured once a database streams." />
          <TimeChart title="Backlog" series={seriesFor(metrics, "backlog_bytes")} fmt={(n) => bytes(n, 0)} from={from} to={to}
            empty="Changes received but not yet applied, per database. Measured once a database streams." />
          <TimeChart title="Applied against source writes" series={[appliedTotal, ...wal]} fmt={(n) => rate(n)} from={from} to={to}
            empty="Bytes applied per second on the target against the source's WAL rate." />
        </div>
      </CrosshairProvider>
    </Card>
  );
}
