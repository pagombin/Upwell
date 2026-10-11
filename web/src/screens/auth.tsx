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
    <main className="login">
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
    </main>
  );
}

export function LoginScreen() {
  const { reload, expired } = useSession();
  const nav = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [otp, setOtp] = useState("");
  const [needOtp, setNeedOtp] = useState(false);
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(undefined);
    try {
      await api.post("/api/v1/auth/login", { username, password, otp: needOtp ? otp : undefined });
      await reload();
      const next = new URLSearchParams(window.location.search).get("next");
      nav(next && next.startsWith("/") && !next.startsWith("//") ? next : "/");
    } catch (e2) {
      const ae = e2 as ApiError;
      if (ae.code === "mfa_required") { setNeedOtp(true); } else { setErr(ae); }
    } finally { setBusy(false); }
  };
  return (
    <main className="login">
      <div className="card">
        <div className="cardhead"><h1>Sign in</h1></div>
        <div className="cardbody">
          <form className="stack" onSubmit={submit}>
            <Brand />
            {expired && <div className="callout warn" role="status"><div className="body">Your session ended after a period of inactivity. Sign in again to continue; running migrations were not affected.</div></div>}
            <label className="field"><span className="lbl">Username</span><input autoFocus value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" /></label>
            <label className="field"><span className="lbl">Password</span><input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" /></label>
            {needOtp && <label className="field"><span className="lbl">Authentication code</span><input autoFocus value={otp} onChange={(e) => setOtp(e.target.value)} autoComplete="one-time-code" inputMode="text" spellCheck={false} className="mono" /><span className="hint">The 6-digit code from your authenticator app, or one of your recovery codes.</span></label>}
            {err && <div className="callout crit" role="alert"><div className="body"><strong>{err.status === 401 ? "Sign-in failed" : "Upwell could not sign you in"}</strong><span>{err.message}</span></div></div>}
            <button className="primary" type="submit" disabled={!username || !password || (needOtp && !otp) || busy}>{busy && <span className="spin" />}Sign in</button>
            <p className="small faint">Sessions end after inactivity. Ten failed attempts lock the account for 15 minutes.</p>
          </form>
        </div>
      </div>
    </main>
  );
}

// SecondFactorCard sets up, replaces and turns off the TOTP second factor.
export function SecondFactorCard({ enforced }: { enforced?: boolean }) {
  const { me, reload } = useSession();
  const [enroll, setEnroll] = useState<{ secret: string; otpauth_uri: string } | null>(null);
  const [code, setCode] = useState("");
  const [codes, setCodes] = useState<string[] | null>(null);
  const [err, setErr] = useState<ApiError>();
  const [busy, setBusy] = useState(false);
  const on = !!me?.user.totp_enabled;
  const act = async (f: () => Promise<void>) => {
    setBusy(true); setErr(undefined);
    try { await f(); } catch (e) { setErr(e as ApiError); } finally { setBusy(false); }
  };
  const copy = (s: string) => { navigator.clipboard?.writeText(s).catch(() => {}); };
  return (
    <div className="card">
      <div className="cardhead"><h2>Second factor (TOTP)</h2>{on ? <span className="pill ok">On</span> : <span className="pill neutral">Off</span>}</div>
      <div className="cardbody stack">
        {enforced && <div className="callout warn" role="status"><div className="body"><strong>Set up the second factor to continue</strong><span>Admins must use an authenticator app (mfa_required_for_admins). Nothing else is available until it is on.</span></div></div>}
        {codes ? (
          <div className="stack" data-testid="recovery-codes">
            <strong>Recovery codes</strong>
            <span className="small">Each code signs you in once if you lose your device. Save them somewhere safe now: they are not shown again.</span>
            <pre className="cmd">{codes.join("\n")}</pre>
            <div className="row"><button onClick={() => copy(codes.join("\n"))}>Copy</button><button className="primary" onClick={async () => { setCodes(null); setEnroll(null); setCode(""); await reload(); }}>I saved them</button></div>
          </div>
        ) : enroll ? (
          <div className="stack">
            <span>Add this account to an authenticator app (1Password, Google Authenticator, Authy and others): enter the secret by hand, or paste the URI into an app that accepts it.</span>
            <label className="field"><span className="lbl">Secret</span><div className="row"><code className="mono wrapany" data-testid="totp-secret">{enroll.secret}</code><button className="ghost" onClick={() => copy(enroll.secret)}>Copy</button></div></label>
            <label className="field"><span className="lbl">URI</span><div className="row"><code className="mono wrapany small">{enroll.otpauth_uri}</code><button className="ghost" onClick={() => copy(enroll.otpauth_uri)}>Copy</button></div></label>
            <label className="field"><span className="lbl">Code from the app</span><input value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" className="mono" /></label>
            <div className="row"><button className="primary" disabled={code.length < 6 || busy} onClick={() => act(async () => { const r = await api.post<{ recovery_codes: string[] }>("/api/v1/auth/totp/confirm", { code }); setCodes(r.recovery_codes); })}>{busy && <span className="spin" />}Turn on</button><button onClick={() => setEnroll(null)}>Cancel</button></div>
          </div>
        ) : on ? (
          <div className="stack">
            <span>Sign-in asks for a code from your authenticator app. {me?.recovery_codes_left ?? 0} recovery code(s) left.</span>
            <label className="field"><span className="lbl">Current code (or a recovery code)</span><input value={code} onChange={(e) => setCode(e.target.value)} autoComplete="one-time-code" className="mono" /></label>
            <div className="row">
              <button disabled={!code || busy} onClick={() => act(async () => { const r = await api.post<{ recovery_codes: string[] }>("/api/v1/auth/totp/recovery-codes", { code }); setCode(""); setCodes(r.recovery_codes); })}>Replace recovery codes</button>
              <button className="danger" disabled={!code || busy} onClick={() => act(async () => { await api.post("/api/v1/auth/totp/disable", { code }); setCode(""); await reload(); })}>Turn off</button>
            </div>
          </div>
        ) : (
          <div className="stack">
            <span>Protect this account with a code from an authenticator app in addition to the password.</span>
            <div className="row"><button className="primary" disabled={busy} onClick={() => act(async () => { setEnroll(await api.post<{ secret: string; otpauth_uri: string }>("/api/v1/auth/totp/enroll")); })}>{busy && <span className="spin" />}Set up</button></div>
          </div>
        )}
        {err && <ErrorState error={err} />}
        {enforced && <button className="ghost" onClick={async () => { await api.post("/api/v1/auth/logout").catch(() => {}); window.location.href = "/login"; }}>Sign out</button>}
      </div>
    </div>
  );
}

export function AccountScreen() {
  const { me } = useSession();
  return (
    <div className="page">
      <div className="pagehead"><div className="titles"><h1>Account</h1><span className="subtitle">{me?.user.username} · {me?.user.role}</span></div></div>
      <SecondFactorCard />
    </div>
  );
}
