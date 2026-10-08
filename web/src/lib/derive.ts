import type { Database, MigrationView, Series } from "./types";

export const LIFECYCLE = [
  { key: "draft", label: "Define" },
  { key: "ready", label: "Preflight" },
  { key: "running", label: "Base copy" },
  { key: "streaming", label: "Streaming" },
  { key: "cutover", label: "Cutover" },
  { key: "verifying", label: "Verify" },
  { key: "completed", label: "Completed" },
  { key: "cleaned_up", label: "Cleaned up" },
];

export function lifecycleIndex(v: MigrationView): number {
  const m = v.migration;
  if (m.flags.cleaned_up) return 7;
  if (m.flags.verdict) return 6;
  if (m.flags.verifying || m.state === "verifying") return 5;
  if (m.flags.cutover && !m.flags.cutover.ended_at) return 4;
  if (m.state === "streaming") return 3;
  if (m.flags.started) return 2;
  if (m.flags.preflight_ok || m.state === "ready") return 1;
  return 0;
}

const AFTER_COPY = new Set(["catch_up", "in_sync", "draining", "drained", "verified"]);

// dbCopyFraction estimates base-copy completion for one database.
export function dbCopyFraction(d: Database, live?: Record<string, number>): number {
  if (d.base_copy_done || AFTER_COPY.has(d.state)) return 1;
  if (d.state !== "base_copy") return 0;
  const t = live?.target_size_bytes, s = live?.source_size_bytes || d.size_bytes;
  if (!t || !s) return 0;
  return Math.min(0.99, t / s);
}

export function overallProgress(v: MigrationView): number {
  const inc = v.databases.filter((d) => d.include);
  const total = inc.reduce((a, d) => a + Math.max(1, d.size_bytes), 0);
  if (!total) return 0;
  return inc.reduce((a, d) => a + Math.max(1, d.size_bytes) * dbCopyFraction(d, v.live?.[d.source_name]), 0) / total;
}

// rateFrom returns the slope (units per second) of the last minutes of a series.
export function rateFrom(points: { ts: number; v: number }[], windowMs = 5 * 60e3): number | undefined {
  if (points.length < 2) return undefined;
  const last = points[points.length - 1];
  const first = points.find((p) => p.ts >= last.ts - windowMs) || points[0];
  const dt = (last.ts - first.ts) / 1000;
  if (dt <= 0) return undefined;
  return (last.v - first.v) / dt;
}

// copyEtaMs estimates time to finish the base copy from target growth.
export function copyEtaMs(v: MigrationView, metrics?: Series[]): number | undefined {
  const inc = v.databases.filter((d) => d.include && d.state === "base_copy");
  if (!inc.length || !metrics) return undefined;
  let remaining = 0, rate = 0;
  for (const d of inc) {
    const live = v.live?.[d.source_name] || {};
    remaining += Math.max(0, (live.source_size_bytes || d.size_bytes) - (live.target_size_bytes || 0));
    const s = metrics.find((x) => x.name === "target_size_bytes" && x.database === d.source_name);
    const r = s ? rateFrom(s.points) : undefined;
    if (r && r > 0) rate += r;
  }
  if (!rate) return undefined;
  return (remaining / rate) * 1000;
}

export function readyForCutover(v: MigrationView): boolean {
  const inc = v.databases.filter((d) => d.include);
  return inc.length > 0 && inc.every((d) => d.state === "in_sync") && !v.migration.flags.verdict;
}

export function verdictOf(v: MigrationView): string | undefined { return v.migration.flags.verdict || undefined; }
