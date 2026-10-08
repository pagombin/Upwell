import { type ReactNode, useMemo, useState } from "react";
import { Empty } from "./ui";

export interface Column<T> {
  key: string;
  label: string;
  render: (row: T) => ReactNode;
  sort?: (row: T) => number | string;
  csv?: (row: T) => string | number;
  num?: boolean;
  width?: number | string;
  optional?: boolean; // can be hidden from the column chooser
}

function csvCell(v: string | number) {
  const s = String(v ?? "");
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

// DataTable: sorting, a text filter, column choice and CSV export.
export function DataTable<T>({ rows, columns, rowKey, onRow, filterText, emptyTitle, emptyHint, csvName, toolbar, initialSort, label }: {
  rows: T[]; columns: Column<T>[]; rowKey: (r: T) => string; onRow?: (r: T) => void; filterText?: (r: T) => string;
  emptyTitle: string; emptyHint?: ReactNode; csvName?: string; toolbar?: ReactNode; initialSort?: { key: string; dir: 1 | -1 }; label: string;
}) {
  const [sort, setSort] = useState<{ key: string; dir: 1 | -1 } | undefined>(initialSort);
  const [q, setQ] = useState("");
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [chooser, setChooser] = useState(false);
  const cols = columns.filter((c) => !hidden.has(c.key));
  const shown = useMemo(() => {
    let r = rows;
    if (q && filterText) {
      const ql = q.toLowerCase();
      r = r.filter((x) => filterText(x).toLowerCase().includes(ql));
    }
    const c = sort && columns.find((x) => x.key === sort.key);
    if (c?.sort) {
      const f = c.sort;
      r = [...r].sort((a, b) => {
        const va = f(a), vb = f(b);
        return (va < vb ? -1 : va > vb ? 1 : 0) * sort!.dir;
      });
    }
    return r;
  }, [rows, q, sort, columns, filterText]);
  const exportCsv = () => {
    const head = cols.map((c) => csvCell(c.label)).join(",");
    const body = shown.map((r) => cols.map((c) => csvCell(c.csv ? c.csv(r) : c.sort ? c.sort(r) : "")).join(",")).join("\n");
    const blob = new Blob([head + "\n" + body + "\n"], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `${csvName || "upwell"}.csv`;
    a.click();
    URL.revokeObjectURL(a.href);
  };
  return (
    <div className="stack">
      {(filterText || csvName || toolbar || columns.some((c) => c.optional)) && (
        <div className="row">
          {filterText && <input className="search" type="search" placeholder="Filter" aria-label={`Filter ${label}`} value={q} onChange={(e) => setQ(e.target.value)} />}
          {toolbar}
          <span style={{ flex: 1 }} />
          {columns.some((c) => c.optional) && (
            <span style={{ position: "relative" }}>
              <button className="ghost" aria-expanded={chooser} onClick={() => setChooser((v) => !v)}>Columns</button>
              {chooser && (
                <div className="card" style={{ position: "absolute", right: 0, top: 36, zIndex: 10, padding: 12, minWidth: 200, boxShadow: "var(--shadow)" }}>
                  <div className="stack" style={{ gap: 6 }}>
                    {columns.filter((c) => c.optional).map((c) => (
                      <label key={c.key} className="check"><input type="checkbox" checked={!hidden.has(c.key)} onChange={() => setHidden((h) => { const n = new Set(h); if (n.has(c.key)) n.delete(c.key); else n.add(c.key); return n; })} />{c.label}</label>
                    ))}
                  </div>
                </div>
              )}
            </span>
          )}
          {csvName && <button className="ghost" onClick={exportCsv}>Export CSV</button>}
        </div>
      )}
      {shown.length === 0 ? <Empty title={rows.length ? "Nothing matches the filter" : emptyTitle}>{rows.length ? "Clear the filter to see every row." : emptyHint}</Empty> : (
        <div className="tablewrap">
          <table className="data" aria-label={label}>
            <thead>
              <tr>
                {cols.map((c) => (
                  <th key={c.key} className={c.num ? "num" : ""} style={c.width ? { width: c.width } : undefined}
                    aria-sort={sort?.key === c.key ? (sort.dir === 1 ? "ascending" : "descending") : undefined}>
                    {c.sort ? (
                      <button className="sort" onClick={() => setSort((s) => s?.key === c.key ? { key: c.key, dir: (s.dir * -1) as 1 | -1 } : { key: c.key, dir: c.num ? -1 : 1 })}>
                        {c.label}<span aria-hidden="true">{sort?.key === c.key ? (sort.dir === 1 ? "▲" : "▼") : ""}</span>
                      </button>
                    ) : c.label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {shown.map((r) => (
                <tr key={rowKey(r)} className={onRow ? "clickable" : ""} onClick={onRow ? () => onRow(r) : undefined}>
                  {cols.map((c) => <td key={c.key} className={c.num ? "num" : ""} style={c.width ? { maxWidth: c.width } : undefined}>{c.render(r)}</td>)}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
