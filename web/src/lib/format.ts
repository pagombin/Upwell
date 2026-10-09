// Numbers always with units, human-scaled; exact values go in title attributes.
const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

export function bytes(n?: number | null, digits = 1): string {
  if (n === undefined || n === null || isNaN(n)) return "—";
  let v = Math.abs(n);
  let i = 0;
  while (v >= 1024 && i < BYTE_UNITS.length - 1) { v /= 1024; i++; }
  return `${n < 0 ? "-" : ""}${v.toFixed(i === 0 ? 0 : digits)} ${BYTE_UNITS[i]}`;
}
export function rate(n?: number | null): string { return n === undefined || n === null ? "—" : `${bytes(n)}/s`; }
export function exactBytes(n?: number | null): string { return n === undefined || n === null ? "" : `${n.toLocaleString("en-US")} bytes`; }

export function count(n?: number | null): string {
  if (n === undefined || n === null || isNaN(n)) return "—";
  const a = Math.abs(n);
  if (a >= 1e12) return (n / 1e12).toFixed(1) + " T";
  if (a >= 1e9) return (n / 1e9).toFixed(1) + " B";
  if (a >= 1e6) return (n / 1e6).toFixed(1) + " M";
  if (a >= 1e4) return (n / 1e3).toFixed(1) + " K";
  return Math.round(n).toLocaleString("en-US");
}
export function exact(n?: number | null): string { return n === undefined || n === null ? "" : n.toLocaleString("en-US"); }

export function duration(ms?: number | null): string {
  if (ms === undefined || ms === null || isNaN(ms)) return "—";
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

export type TimePref = "utc" | "local";
let pref: TimePref = (typeof localStorage !== "undefined" && (safeGet("upwell.time") as TimePref)) || "utc";
function safeGet(k: string): string | null { try { return localStorage.getItem(k); } catch { return null; } }
export function setTimePref(p: TimePref) { pref = p; try { localStorage.setItem("upwell.time", p); } catch { /* ignore */ } }
export function timePref(): TimePref { return pref; }

export function time(ms?: number | null, withDate = true): string {
  if (!ms) return "—";
  const d = new Date(ms);
  const opts: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false, timeZone: pref === "utc" ? "UTC" : undefined };
  if (withDate) { opts.year = "numeric"; opts.month = "short"; opts.day = "2-digit"; }
  const zone = pref === "utc" ? "UTC" : Intl.DateTimeFormat().resolvedOptions().timeZone;
  return `${new Intl.DateTimeFormat("en-GB", opts).format(d)} ${zone}`;
}
export function clock(ms?: number | null): string { return time(ms, false); }
export function ago(ms?: number | null, now = Date.now()): string {
  if (!ms) return "—";
  return `${duration(now - ms)} ago`;
}
export function pct(n?: number | null, digits = 0): string { return n === undefined || n === null || isNaN(n) ? "—" : `${n.toFixed(digits)}%`; }
