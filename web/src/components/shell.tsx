import { ReactNode, useEffect, useMemo, useRef, useState } from "react";
import { Link, NavLink, useLocation, useMatch, useNavigate } from "react-router-dom";
import { api } from "../lib/api";
import { useApi, useStream } from "../lib/hooks";
import { useSession } from "../lib/session";
import type { Alert, MigrationView } from "../lib/types";
import { Dialog, Icon, Trunc, useToast } from "./ui";

function Logo() {
  return (
    <svg width="22" height="22" viewBox="0 0 32 32" aria-hidden="true">
      <rect width="32" height="32" rx="7" fill="#0f1a24" />
      <path d="M7 21c3-4 6-4 9 0s6 4 9 0" stroke="#5ec2d4" strokeWidth="2.4" fill="none" strokeLinecap="round" />
      <path d="M16 18V8m0 0-4 4m4-4 4 4" stroke="#e8eef3" strokeWidth="2.4" fill="none" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

const NAV = [
  { to: "/", label: "Home", icon: "home", end: true },
  { to: "/logs", label: "Logs", icon: "logs" },
  { to: "/events", label: "Events and audit", icon: "events" },
  { to: "/alerts", label: "Alerts", icon: "alert" },
  { to: "/host", label: "Host", icon: "host" },
  { to: "/settings", label: "Settings", icon: "settings" },
  { to: "/system", label: "System", icon: "system" },
];

export const MIG_TABS = [
  { to: "", label: "Overview" },
  { to: "replication", label: "Replication" },
  { to: "source-health", label: "Source health" },
  { to: "preflight", label: "Preflight" },
  { to: "cutover", label: "Cutover" },
  { to: "verification", label: "Verification" },
  { to: "report", label: "Report" },
  { to: "logs", label: "Logs" },
  { to: "events", label: "Events" },
];

export function Shell({ children }: { children: ReactNode }) {
  const { me, theme, setTheme, density, setDensity, tz, setTz, can } = useSession();
  const nav = useNavigate();
  const toast = useToast();
  const migs = useApi<MigrationView[]>("/api/v1/migrations", 15000);
  const alerts = useApi<Alert[]>("/api/v1/alerts?state=firing", 15000);
  useStream("/api/v1/stream/system", (e) => { if (e.topic === "alerts") alerts.refresh(); });
  const [palette, setPalette] = useState(false);
  const [pw, setPw] = useState(false);
  const m = useMatch("/migrations/:id/*");
  const current = m?.params.id;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); setPalette((p) => !p); }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  const firing = (alerts.data || []).filter((a) => a.state === "firing");
  const critical = firing.filter((a) => a.severity === "critical");
  const logout = async () => {
    try { await api.post("/api/v1/auth/logout"); } catch { /* already signed out */ }
    window.location.assign("/login");
  };
  const active = migs.data || [];
  return (
    <div className="shell">
      <a href="#main" className="sr-only" onFocus={(e) => e.currentTarget.classList.remove("sr-only")} onBlur={(e) => e.currentTarget.classList.add("sr-only")}>Skip to content</a>
      <aside className="rail" aria-label="Main navigation">
        <Link to="/" className="brand"><Logo /><span className="txt">Upwell</span></Link>
        <nav>
          {NAV.map((n) => (
            <NavLink key={n.to} to={n.to} end={n.end} className="navlink" title={n.label}>
              <Icon name={n.icon} /><span className="txt">{n.label}</span>
              {n.to === "/alerts" && firing.length > 0 && <span className="pill crit nodot txt" style={{ marginLeft: "auto" }}>{firing.length}</span>}
            </NavLink>
          ))}
          <div className="navsec">Migrations</div>
          {active.slice(0, 12).map((v) => (
            <NavLink key={v.migration.id} to={`/migrations/${v.migration.short_id}`} className="navlink" title={v.migration.name}>
              <Icon name="migration" /><span className="txt trunc">{v.migration.name}</span>
            </NavLink>
          ))}
          {can("operator") && <NavLink to="/migrations/new" className="navlink" title="New migration"><Icon name="plus" /><span className="txt">New migration</span></NavLink>}
        </nav>
        <div className="foot">Upwell {me?.version}</div>
      </aside>
      <header className="topbar">
        <label className="sr-only" htmlFor="mig-switch">Switch migration</label>
        <select id="mig-switch" value={current || ""} onChange={(e) => e.target.value && nav(`/migrations/${e.target.value}`)} style={{ maxWidth: 320 }}>
          <option value="">{active.length ? "Switch migration…" : "No migrations yet"}</option>
          {active.map((v) => <option key={v.migration.id} value={v.migration.short_id}>{v.migration.name} ({v.migration.short_id})</option>)}
        </select>
        <button className="ghost" onClick={() => setPalette(true)} aria-label="Search and commands (Ctrl+K)"><Icon name="search" /><span>Search</span><kbd className="small faint">Ctrl K</kbd></button>
        <span className="spacer" />
        <Link to="/alerts" className="btn ghost" aria-label={`${firing.length} firing alerts`} style={{ border: "1px solid transparent", color: firing.length ? "var(--crit)" : "var(--muted)" }}>
          <Icon name="bell" /><span>{firing.length}</span>
        </Link>
        <button className="ghost" onClick={() => setTz(tz === "utc" ? "local" : "utc")} title="Show times in UTC or local time">{tz === "utc" ? "UTC" : "Local"}</button>
        <button className="ghost" onClick={() => setDensity(density === "compact" ? "comfortable" : "compact")} title="Table density">{density === "compact" ? "Compact" : "Comfortable"}</button>
        <button className="ghost icon" onClick={() => setTheme(theme === "dark" ? "light" : "dark")} aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}><Icon name={theme === "dark" ? "sun" : "moon"} /></button>
        <UserMenu name={me?.user.username || ""} role={me?.user.role || ""} onLogout={logout} onPassword={() => setPw(true)} />
      </header>
      <main className="main" id="main" tabIndex={-1}>
        {critical.length > 0 && (
          <div className="banner" role="alert">
            <Icon name="alert" />
            <span className="trunc" style={{ flex: 1 }} title={critical[0].message}>{critical.length === 1 ? critical[0].message : `${critical.length} critical alerts are firing. ${critical[0].message}`}</span>
            <Link to="/alerts" style={{ color: "inherit" }}>Open alerts</Link>
          </div>
        )}
        {children}
      </main>
      {palette && <Palette onClose={() => setPalette(false)} migrations={active} current={current} onLogout={logout} />}
      {pw && <PasswordDialog onClose={() => setPw(false)} onDone={() => toast("ok", "Password changed")} />}
    </div>
  );
}

function UserMenu({ name, role, onLogout, onPassword }: { name: string; role: string; onLogout: () => void; onPassword: () => void }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false); };
    const esc = (e: KeyboardEvent) => { if (e.key === "Escape") setOpen(false); };
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => { document.removeEventListener("mousedown", close); document.removeEventListener("keydown", esc); };
  }, [open]);
  return (
    <div ref={ref} style={{ position: "relative" }}>
      <button className="ghost" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
        <span className="trunc" style={{ maxWidth: 160 }}>{name}</span><span className="pill neutral nodot">{role}</span>
      </button>
      {open && (
        <div className="card" role="menu" style={{ position: "absolute", right: 0, top: 40, zIndex: 30, padding: 8, minWidth: 200, boxShadow: "var(--shadow)" }}>
          <div className="stack" style={{ gap: 4 }}>
            <button role="menuitem" className="ghost" style={{ justifyContent: "flex-start" }} onClick={() => { setOpen(false); onPassword(); }}>Change password</button>
            <button role="menuitem" className="ghost" style={{ justifyContent: "flex-start" }} onClick={onLogout}>Sign out</button>
          </div>
        </div>
      )}
    </div>
  );
}

function PasswordDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [cur, setCur] = useState("");
  const [next, setNext] = useState("");
  const [err, setErr] = useState("");
  const save = async () => {
    try { await api.post("/api/v1/auth/password", { current: cur, new: next }); onDone(); onClose(); }
    catch (e) { setErr((e as Error).message); }
  };
  return (
    <Dialog title="Change password" onClose={onClose} footer={<><button onClick={onClose}>Cancel</button><button className="primary" disabled={next.length < 14} onClick={save}>Change password</button></>}>
      <label className="field"><span className="lbl">Current password</span><input type="password" autoComplete="current-password" value={cur} onChange={(e) => setCur(e.target.value)} /></label>
      <label className="field"><span className="lbl">New password</span><input type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} /><span className="hint">At least 14 characters.</span></label>
      {err && <div className="callout crit" role="alert"><div className="body">{err}</div></div>}
    </Dialog>
  );
}

interface Cmd { id: string; label: string; hint?: string; run: () => void }

function Palette({ onClose, migrations, current, onLogout }: { onClose: () => void; migrations: MigrationView[]; current?: string; onLogout: () => void }) {
  const nav = useNavigate();
  const loc = useLocation();
  const { theme, setTheme, can } = useSession();
  const [q, setQ] = useState("");
  const [sel, setSel] = useState(0);
  const cmds = useMemo<Cmd[]>(() => {
    const go = (to: string) => () => { nav(to); onClose(); };
    const out: Cmd[] = NAV.map((n) => ({ id: "nav" + n.to, label: n.label, hint: "Screen", run: go(n.to) }));
    if (can("operator")) out.push({ id: "new", label: "New migration", hint: "Action", run: go("/migrations/new") });
    for (const v of migrations) {
      out.push({ id: "m" + v.migration.id, label: v.migration.name, hint: `Migration ${v.migration.short_id}`, run: go(`/migrations/${v.migration.short_id}`) });
    }
    if (current) {
      for (const t of MIG_TABS) out.push({ id: "t" + t.to, label: `${t.label}`, hint: "This migration", run: go(`/migrations/${current}${t.to ? "/" + t.to : ""}`) });
    }
    out.push({ id: "theme", label: `Switch to ${theme === "dark" ? "light" : "dark"} theme`, hint: "Action", run: () => { setTheme(theme === "dark" ? "light" : "dark"); onClose(); } });
    out.push({ id: "logout", label: "Sign out", hint: "Action", run: onLogout });
    return out;
  }, [migrations, current, theme, can, nav, onClose, onLogout, setTheme]);
  const shown = cmds.filter((c) => (c.label + " " + (c.hint || "")).toLowerCase().includes(q.toLowerCase())).slice(0, 40);
  useEffect(() => setSel(0), [q, loc.pathname]);
  return (
    <div className="overlay" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose(); }}>
      <div className="dialog palette" role="dialog" aria-modal="true" aria-label="Command palette">
        <header>
          <input autoFocus style={{ flex: 1 }} placeholder="Go to a screen, migration or action" aria-label="Search commands" value={q}
            role="combobox" aria-expanded="true" aria-controls="palette-list" aria-activedescendant={shown[sel] ? `pc-${shown[sel].id}` : undefined}
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown") { e.preventDefault(); setSel((s) => Math.min(shown.length - 1, s + 1)); }
              if (e.key === "ArrowUp") { e.preventDefault(); setSel((s) => Math.max(0, s - 1)); }
              if (e.key === "Enter" && shown[sel]) shown[sel].run();
              if (e.key === "Escape") onClose();
            }} />
        </header>
        <ul id="palette-list" role="listbox" aria-label="Commands">
          {shown.length === 0 && <li className="muted" style={{ padding: 8 }}>Nothing matches.</li>}
          {shown.map((c, i) => (
            <li key={c.id} role="presentation">
              <button id={`pc-${c.id}`} role="option" aria-selected={i === sel} onClick={c.run} onMouseEnter={() => setSel(i)}>
                <Trunc>{c.label}</Trunc><span className="small faint" style={{ marginLeft: "auto" }}>{c.hint}</span>
              </button>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
