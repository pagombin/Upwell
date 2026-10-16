import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../lib/api";
import { bytes, count, duration, rate, time } from "../lib/format";
import { useApi } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Run } from "../lib/types";
import { Card, ConfirmDialog, Empty, ErrorState, Pill, Skeleton, Tile, Trunc } from "../components/ui";

// D105: every finished run (a verdict, an abort, or a run replaced by Run
// again), with the numbers that tell its story; the report opens as it was.
export function RunsScreen() {
  const runs = useApi<Run[]>("/api/v1/runs", 30000);
  const { can } = useSession();
  const [q, setQ] = useState("");
  const [del, setDel] = useState<Run | null>(null);
  const rows = (runs.data || []).filter((r) => !q || `${r.summary.name} ${r.summary.short_id} ${r.outcome} ${r.summary.databases.map((d) => d.name).join(" ")}`.toLowerCase().includes(q.toLowerCase()));
  return (
    <div className="page">
      <div className="pagehead"><div className="titles"><h1>Run history</h1><span className="subtitle">Each finished run of every migration, newest first. Re-running a migration keeps the run it replaces here.</span></div></div>
      {runs.error ? <ErrorState error={runs.error} retry={runs.refresh} /> : !runs.data ? <Skeleton lines={6} /> : runs.data.length === 0 ? (
        <Empty title="No finished runs yet">A run is recorded at its verdict, when it is aborted, or when the migration is run again.</Empty>
      ) : (
        <div className="stack" style={{ gap: 16 }}>
          <input type="search" aria-label="Search runs" placeholder="Search by migration, outcome or database" value={q} onChange={(e) => setQ(e.target.value)} style={{ maxWidth: 420 }} />
          {rows.map((r) => <RunCard key={r.id} r={r} canDelete={can("operator")} onDelete={() => setDel(r)} />)}
          {rows.length === 0 && <Empty title="No matching runs">Change the search.</Empty>}
        </div>
      )}
      {del && (
        <ConfirmDialog title="Delete this run from the history" danger confirmLabel="Delete run" onClose={() => setDel(null)}
          onConfirm={async () => { await api.del(`/api/v1/runs/${del.id}`); runs.refresh(); }}>
          <p>Run {del.number} of {del.summary.name}, ended {time(del.ended_at)}. Its numbers and stored report are removed; the migration itself is not touched. The deletion is recorded in the audit log.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}

function outcomeKind(o: string) { return o === "GO" ? "ok" : o === "NO-GO" ? "crit" : o === "aborted" ? "warn" : "neutral"; }

function RunCard({ r, canDelete, onDelete }: { r: Run; canDelete: boolean; onDelete: () => void }) {
  const s = r.summary;
  const headline = [
    s.outcome === "GO" ? "Reached GO" : s.outcome === "NO-GO" ? "Ended NO-GO" : s.outcome === "aborted" ? "Aborted" : "Replaced by a new run",
    `in ${duration(s.duration_ms)}`,
    `${bytes(s.total_bytes)} in ${s.databases.length} database${s.databases.length === 1 ? "" : "s"}`,
    s.base_copy_ms ? `base copy ${duration(s.base_copy_ms)} (${rate(s.copy_bytes_per_s)})` : "",
    s.write_pause_ms ? `write pause ${duration(s.write_pause_ms)}` : "",
  ].filter(Boolean).join(" · ");
  return (
    <Card title={`${s.name} — run ${r.number}`} actions={<div className="row" style={{ gap: 8 }}>
      <Pill kind={outcomeKind(r.outcome)} text={r.outcome === "replaced" ? "Replaced" : r.outcome} />
      {r.has_report && <a className="btn" href={`/api/v1/runs/${r.id}/report`} target="_blank" rel="noreferrer">Report</a>}
      {r.has_report && <a className="btn ghost" href={`/api/v1/runs/${r.id}/report?format=md`} download>Markdown</a>}
      {canDelete && <button className="ghost" onClick={onDelete}>Delete</button>}
    </div>}>
      <div className="stack">
        <p style={{ margin: 0 }}><strong>{headline}</strong></p>
        <p className="small muted" style={{ margin: 0 }}>
          {time(r.started_at)} to {time(r.ended_at)} · <Link to={`/migrations/${s.short_id}`}>{s.short_id}</Link> · {s.mode || "online"} · <Trunc className="mono">{s.source}</Trunc> → <Trunc className="mono">{s.target}</Trunc>
        </p>
        <div className="grid" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(170px, 1fr))" }}>
          <Tile label="Rows (estimate)" value={count(s.total_rows)} />
          <Tile label="Apply, average" value={s.apply_avg_bytes_per_s ? rate(s.apply_avg_bytes_per_s) : "—"} sub={s.apply_peak_bytes_per_s ? `peak ${rate(s.apply_peak_bytes_per_s)}` : undefined} />
          <Tile label="Source WAL, average" value={s.source_wal_avg_bytes_per_s ? rate(s.source_wal_avg_bytes_per_s) : "—"} />
          <Tile label="Peak backlog" value={s.peak_backlog_bytes ? bytes(s.peak_backlog_bytes) : "—"} sub={s.peak_lag_seconds ? `peak lag ${duration(s.peak_lag_seconds * 1000)}` : undefined} />
          <Tile label="Events" value={`${s.critical_events} critical`} sub={`${s.warning_events} warnings`} />
          {s.unverified_count ? <Tile label="Not fully verified" value={`${s.unverified_count} table(s)`} /> : null}
        </div>
        <div className="tablewrap">
          <table className="data" aria-label={`Databases of run ${r.number}`}>
            <thead><tr><th>Database</th><th className="num">Size</th><th className="num">Tables</th><th className="num">Base copy</th><th className="num">Rate</th><th className="num">Engine launches</th><th>Verdict</th></tr></thead>
            <tbody>{s.databases.map((d) => (
              <tr key={d.name}>
                <td className="mono">{d.name}{d.target !== d.name ? ` → ${d.target}` : ""}</td>
                <td className="num">{bytes(d.size_bytes)}</td>
                <td className="num">{d.tables}</td>
                <td className="num">{d.base_copy_ms ? duration(d.base_copy_ms) : "—"}</td>
                <td className="num">{d.base_copy_ms ? rate(d.size_bytes / (d.base_copy_ms / 1000)) : "—"}</td>
                <td className="num">{d.attempts}</td>
                <td>{d.verdict ? <Pill kind={d.verdict === "GO" ? "ok" : "crit"} text={d.verdict} /> : <span className="muted">{d.state.replace(/_/g, " ")}</span>}</td>
              </tr>
            ))}</tbody>
          </table>
        </div>
      </div>
    </Card>
  );
}
