import { type FormEvent, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, ApiError } from "../lib/api";
import { time } from "../lib/format";
import { useApi } from "../lib/hooks";
import { useSession } from "../lib/session";
import { ComingSoon, ErrorState, Skeleton } from "../components/ui";

interface SetupStatus { needs_setup: boolean; tls: { fingerprint: string; not_after: number; subject: string; self_signed: boolean }; setup_token_file: string; version: string }

function Brand() {
  return <div className="row" style={{ gap: 8 }}><strong style={{ fontSize: 20 }}>Upwell</strong><span className="muted">Migration Console</span></div>;
}

export function SetupScreen() {
  const st = useApi<SetupStatus>("/api/v1/setup");
  const { reload } = useSession();
  const nav = useNavigate();
  const [token, setToken] = useState("");
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [tz, setTz] = useState(Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC");
  const [verified, setVerified] = useState(false);
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState(false);
  const ok = token && username && password.length >= 14 && password === confirm && verified;
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(undefined);
    try {
      await api.post("/api/v1/setup", { setup_token: token.trim(), username: username.trim().toLowerCase(), password, time_zone: tz });
      await api.post("/api/v1/auth/login", { username: username.trim().toLowerCase(), password });
      await reload();
      nav("/");
    } catch (e2) { setErr(e2 as ApiError); } finally { setBusy(false); }
  };
  return (
    <div className="login">
      <div className="card" style={{ width: "min(560px, 100%)" }}>
        <div className="cardhead"><h1>Finish setting up Upwell</h1></div>
        <div className="cardbody">
          {st.error ? <ErrorState error={st.error} retry={st.refresh} /> : !st.data ? <Skeleton lines={6} /> : !st.data.needs_setup ? (
            <div className="stack"><p>Upwell is already set up.</p><a className="btn" href="/login">Sign in</a></div>
          ) : (
            <form className="stack" onSubmit={submit}>
              <Brand />
              <div className="callout info">
                <div className="body">
                  <strong>Check the certificate fingerprint</strong>
                  <span className="small">Compare it with the fingerprint install.sh printed on the droplet. If they differ, stop: someone may be intercepting this connection.</span>
                  <code className="wrapany" data-testid="tls-fingerprint">{st.data.tls.fingerprint || "No certificate information"}</code>
                  <span className="small muted">{st.data.tls.subject} · valid until {time(st.data.tls.not_after)}{st.data.tls.self_signed ? " · self-signed" : ""}</span>
                  <label className="check"><input type="checkbox" checked={verified} onChange={(e) => setVerified(e.target.checked)} />The fingerprint matches what install.sh printed</label>
                </div>
              </div>
              <label className="field"><span className="lbl">Setup token</span>
                <input value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" spellCheck={false} className="mono" />
                <span className="hint">Printed by install.sh, and stored in <code className="wrapany">{st.data.setup_token_file}</code> until setup finishes.</span>
              </label>
              <div className="fieldgrid">
                <label className="field"><span className="lbl">Admin username</span><input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" /></label>
                <label className="field"><span className="lbl">Time zone for reports</span><input value={tz} onChange={(e) => setTz(e.target.value)} /></label>
                <label className="field"><span className="lbl">Password</span><input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" /><span className="hint">At least 14 characters.</span></label>
                <label className="field"><span className="lbl">Confirm password</span><input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" />{confirm && confirm !== password && <span className="hint" style={{ color: "var(--crit)" }}>The passwords differ.</span>}</label>
              </div>
              <div className="row small muted"><span>DigitalOcean API token</span><ComingSoon what="DigitalOcean API discovery" /><span>Notification channel test</span><ComingSoon what="Notifications" /></div>
              {err && <ErrorState error={err} />}
              <div className="row"><button className="primary" type="submit" disabled={!ok || busy}>{busy && <span className="spin" />}Create the Admin and sign in</button></div>
            </form>
          )}
        </div>
      </div>
    </div>
  );
}

export function LoginScreen() {
  const { reload, expired } = useSession();
  const nav = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(undefined);
    try {
      await api.post("/api/v1/auth/login", { username, password });
      await reload();
      const next = new URLSearchParams(window.location.search).get("next");
      nav(next && next.startsWith("/") && !next.startsWith("//") ? next : "/");
    } catch (e2) { setErr(e2 as ApiError); } finally { setBusy(false); }
  };
  return (
    <div className="login">
      <div className="card">
        <div className="cardhead"><h1>Sign in</h1></div>
        <div className="cardbody">
          <form className="stack" onSubmit={submit}>
            <Brand />
            {expired && <div className="callout warn" role="status"><div className="body">Your session ended after a period of inactivity. Sign in again to continue; running migrations were not affected.</div></div>}
            <label className="field"><span className="lbl">Username</span><input autoFocus value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" /></label>
            <label className="field"><span className="lbl">Password</span><input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" /></label>
            <div className="row small muted"><span>Second factor (TOTP)</span><ComingSoon what="TOTP" /></div>
            {err && <div className="callout crit" role="alert"><div className="body"><strong>{err.status === 401 ? "Sign-in failed" : "Upwell could not sign you in"}</strong><span>{err.message}</span></div></div>}
            <button className="primary" type="submit" disabled={!username || !password || busy}>{busy && <span className="spin" />}Sign in</button>
            <p className="small faint">Sessions end after inactivity. Ten failed attempts lock the account for 15 minutes.</p>
          </form>
        </div>
      </div>
    </div>
  );
}
