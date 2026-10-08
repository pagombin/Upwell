import { createContext, ReactNode, useContext, useEffect, useMemo, useRef, useState } from "react";
import { clock } from "../lib/format";
import type { Series } from "../lib/types";

// Charts are plain SVG with fixed heights so live updates never shift layout.
// Charts inside one CrosshairProvider share a crosshair timestamp.

const PALETTE = ["var(--info)", "var(--ok)", "var(--warn)", "var(--cutover)", "var(--crit)", "#5fb8b0", "#d48ac2", "#9db86b"];
export function color(i: number) { return PALETTE[i % PALETTE.length]; }

interface Cross { ts: number | null; set: (t: number | null) => void }
const CrossCtx = createContext<Cross | null>(null);
export function CrosshairProvider({ children }: { children: ReactNode }) {
  const [ts, set] = useState<number | null>(null);
  return <CrossCtx.Provider value={{ ts, set }}>{children}</CrossCtx.Provider>;
}

function useWidth<T extends HTMLElement>(): [React.RefObject<T>, number] {
  const ref = useRef<T>(null);
  const [w, setW] = useState(600);
  useEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver((e) => setW(Math.max(120, Math.floor(e[0].contentRect.width))));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, w];
}

function niceMax(v: number) {
  if (v <= 0 || !isFinite(v)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

export interface ChartSeries { name: string; points: { ts: number; v: number }[] }

export function TimeChart({ series, height = 180, fmt, title, from, to, empty, threshold }: {
  series: ChartSeries[]; height?: number; fmt: (v: number) => string; title: string; from?: number; to?: number;
  empty?: string; threshold?: { v: number; label: string };
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const cross = useContext(CrossCtx);
  const [localTs, setLocalTs] = useState<number | null>(null);
  const ts = cross ? cross.ts : localTs;
  const setTs = cross ? cross.set : setLocalTs;
  const padL = 64, padR = 12, padT = 8, padB = 22;
  const all = series.flatMap((s) => s.points);
  const t0 = from ?? (all.length ? Math.min(...all.map((p) => p.ts)) : Date.now() - 3600e3);
  const t1 = to ?? (all.length ? Math.max(...all.map((p) => p.ts)) : Date.now());
  const span = Math.max(1, t1 - t0);
  const vmax = niceMax(Math.max(threshold?.v ?? 0, ...all.map((p) => p.v), 0));
  const iw = Math.max(10, width - padL - padR), ih = height - padT - padB;
  const x = (t: number) => padL + ((t - t0) / span) * iw;
  const y = (v: number) => padT + ih - (Math.max(0, v) / vmax) * ih;
  const paths = useMemo(() => series.map((s) => s.points.map((p, i) => `${i ? "L" : "M"}${x(p.ts).toFixed(1)},${y(p.v).toFixed(1)}`).join("")),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [series, width, vmax, t0, t1]);
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => vmax * f);
  const xticks = [0, 1 / 3, 2 / 3, 1].map((f) => t0 + span * f);
  const nearest = (s: ChartSeries, t: number) => {
    let best: { ts: number; v: number } | undefined;
    for (const p of s.points) if (!best || Math.abs(p.ts - t) < Math.abs(best.ts - t)) best = p;
    return best;
  };
  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const px = e.clientX - r.left;
    if (px < padL || px > padL + iw) { setTs(null); return; }
    setTs(t0 + ((px - padL) / iw) * span);
  };
  const inRange = ts !== null && ts >= t0 && ts <= t1;
  const tipLeft = inRange ? Math.min(width - 200, Math.max(0, x(ts!) + 12)) : 0;
  return (
    <div className="stack" style={{ gap: 6 }}>
      <div className="legend" aria-hidden={series.length === 0}>
        {series.map((s, i) => <span key={s.name} title={s.name}><i style={{ background: color(i) }} /><span className="trunc">{s.name}</span></span>)}
        {threshold && <span><i style={{ background: "var(--crit)" }} />{threshold.label}</span>}
      </div>
      <div className="chartbox" ref={ref} style={{ height }}>
        {all.length === 0 ? (
          <div className="empty" style={{ height }}><h3>No data yet</h3><p>{empty || "Samples appear here once the collector records them."}</p></div>
        ) : (
          <svg className="chart" width={width} height={height} role="img" aria-label={title} onMouseMove={onMove} onMouseLeave={() => setTs(null)}>
            {ticks.map((v) => <g key={v}><line className="gridline" x1={padL} x2={padL + iw} y1={y(v)} y2={y(v)} /><text x={padL - 6} y={y(v) + 4} textAnchor="end">{fmt(v)}</text></g>)}
            {xticks.map((t, i) => <text key={i} x={x(t)} y={height - 6} textAnchor={i === 0 ? "start" : i === 3 ? "end" : "middle"}>{clock(t).replace(/ \S+$/, "")}</text>)}
            <line className="axis" x1={padL} x2={padL + iw} y1={padT + ih} y2={padT + ih} />
            {threshold && <line x1={padL} x2={padL + iw} y1={y(threshold.v)} y2={y(threshold.v)} stroke="var(--crit)" strokeDasharray="4 3" />}
            {paths.map((d, i) => <path key={i} d={d} fill="none" stroke={color(i)} strokeWidth={1.6} />)}
            {inRange && <line className="cross" x1={x(ts!)} x2={x(ts!)} y1={padT} y2={padT + ih} />}
          </svg>
        )}
        {inRange && all.length > 0 && (
          <div className="charttip" style={{ left: tipLeft, top: 8 }}>
            <div className="muted">{clock(ts!)}</div>
            {series.map((s, i) => { const p = nearest(s, ts!); return p ? <div key={s.name} className="row" style={{ gap: 6, flexWrap: "nowrap" }}><i style={{ width: 8, height: 8, borderRadius: 4, background: color(i), flex: "none" }} /><span className="trunc" style={{ maxWidth: 180 }}>{s.name}</span><strong style={{ marginLeft: "auto" }}>{fmt(p.v)}</strong></div> : null; })}
          </div>
        )}
      </div>
    </div>
  );
}

export function Sparkline({ points, width = 96, height = 24, label }: { points: number[]; width?: number; height?: number; label: string }) {
  if (points.length < 2) return <svg className="spark" width={width} height={height} role="img" aria-label={`${label}: not enough samples`} />;
  const max = Math.max(...points, 1e-9), min = Math.min(...points, 0);
  const span = max - min || 1;
  const d = points.map((v, i) => `${i ? "L" : "M"}${((i / (points.length - 1)) * (width - 2) + 1).toFixed(1)},${(height - 2 - ((v - min) / span) * (height - 4)).toFixed(1)}`).join("");
  return <svg className="spark" width={width} height={height} role="img" aria-label={label}><path d={d} fill="none" stroke="var(--info)" strokeWidth={1.4} /></svg>;
}

// seriesFor picks one metric's series per database from an API result.
export function seriesFor(all: Series[] | undefined, name: string, db?: string): ChartSeries[] {
  return (all || []).filter((s) => s.name === name && (db === undefined || (s.database || "") === db))
    .map((s) => ({ name: s.database || name, points: s.points.map((p) => ({ ts: p.ts, v: p.v })) }));
}
