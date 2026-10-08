import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react";
import { api, setCsrf, setUnauthenticatedHandler } from "./api";
import { setTimePref, timePref, type TimePref } from "./format";
import type { Role, User } from "./types";

export interface Me { user: User; csrf_token: string; expires_at: number; idle_minutes: number; time_zone: string; version: string }

interface Session {
  me: Me | null;
  status: "loading" | "anonymous" | "setup" | "ready";
  reload: () => Promise<void>;
  setMe: (m: Me | null) => void;
  can: (role: Role) => boolean;
  theme: "dark" | "light";
  setTheme: (t: "dark" | "light") => void;
  density: "comfortable" | "compact";
  setDensity: (d: "comfortable" | "compact") => void;
  tz: TimePref;
  setTz: (t: TimePref) => void;
  expired: boolean;
}

const RANK: Record<Role, number> = { viewer: 0, operator: 1, admin: 2 };
const Ctx = createContext<Session>(null as unknown as Session);
export function useSession() { return useContext(Ctx); }

function load(k: string, d: string) { try { return localStorage.getItem(k) || d; } catch { return d; } }
function save(k: string, v: string) { try { localStorage.setItem(k, v); } catch { /* storage may be unavailable */ } }

export function SessionProvider({ children }: { children: ReactNode }) {
  const [me, setMeState] = useState<Me | null>(null);
  const [status, setStatus] = useState<Session["status"]>("loading");
  const [expired, setExpired] = useState(false);
  const [theme, setThemeState] = useState<"dark" | "light">(load("upwell.theme", "dark") === "light" ? "light" : "dark");
  const [density, setDensityState] = useState<"comfortable" | "compact">(load("upwell.density", "comfortable") === "compact" ? "compact" : "comfortable");
  const [tz, setTzState] = useState<TimePref>(timePref());

  useEffect(() => { document.documentElement.dataset.theme = theme; save("upwell.theme", theme); }, [theme]);
  useEffect(() => { document.documentElement.dataset.density = density; save("upwell.density", density); }, [density]);

  const setMe = useCallback((m: Me | null) => {
    setMeState(m);
    if (m) { setCsrf(m.csrf_token); setStatus("ready"); setExpired(false); }
  }, []);

  const reload = useCallback(async () => {
    try {
      const m = await api.get<Me | { user: null; needs_setup: boolean }>("/api/v1/auth/session");
      if (m.user) { setMe(m as Me); return; }
      setStatus((m as { needs_setup: boolean }).needs_setup ? "setup" : "anonymous");
    } catch {
      setStatus("anonymous");
    }
    setMeState(null);
  }, [setMe]);

  useEffect(() => {
    setUnauthenticatedHandler(() => {
      setMeState((cur) => { if (cur) setExpired(true); return null; });
      setStatus("anonymous");
    });
    reload();
  }, [reload]);

  const can = useCallback((r: Role) => !!me && RANK[me.user.role] >= RANK[r], [me]);
  const setTz = (t: TimePref) => { setTimePref(t); setTzState(t); };

  return (
    <Ctx.Provider value={{ me, status, reload, setMe, can, theme, setTheme: setThemeState, density, setDensity: setDensityState, tz, setTz, expired }}>
      {children}
    </Ctx.Provider>
  );
}
