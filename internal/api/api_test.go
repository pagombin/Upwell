package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/auth"
	"github.com/pagombin/upwell/internal/bus"
	"github.com/pagombin/upwell/internal/config"
	"github.com/pagombin/upwell/internal/engine/pgcopydb"
	"github.com/pagombin/upwell/internal/logx"
	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/runner"
	"github.com/pagombin/upwell/internal/secrets"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
)

type client struct {
	t    *testing.T
	srv  *httptest.Server
	c    *http.Client
	csrf string
}

// newServer starts the API with no users, like a fresh install.
func newServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir, cfg.LogDir = filepath.Join(dir, "data"), filepath.Join(dir, "log")
	cfg.MasterKey, cfg.SetupToken = filepath.Join(dir, "key"), filepath.Join(dir, "setup-token")
	cfg.Dev = true
	os.MkdirAll(cfg.RunsDir(), 0o750)
	st, err := store.Open(cfg.StorePath())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := secrets.LoadOrCreateKey(cfg.MasterKey, true)
	box, _ := secrets.New(key)
	b := bus.New(256)
	lg, _ := logx.New(cfg.LogDir, st, b, "info", false)
	set := &settings.Service{Store: st}
	// TOTP for Admins is tested on its own (TestTOTP*); the other tests sign
	// in with a password.
	set.Set(t.Context(), "mfa_required_for_admins", false, "test")
	r, _ := runner.New("local", filepath.Join(cfg.DataDir, "units"))
	au := &audit.Log{Store: st}
	vault := &secrets.Vault{Box: box, Store: st}
	o := orch.New(orch.Deps{Config: cfg, Store: st, Vault: vault, Engine: pgcopydb.New("pgcopydb", r), Runner: r, Logger: lg, Bus: b, Audit: au, Settings: set})
	as := auth.NewService(st)
	as.Vault = vault
	as.FastHashForTests()
	h := NewHealth()
	srv := New(&Server{Cfg: cfg, Store: st, Auth: as, Audit: au, Orch: o, Settings: set, Bus: b, Log: lg, Version: "test", Health: h, TLSInfo: func() TLSInfo { return TLSInfo{Fingerprint: "AA:BB"} }})
	ts := httptest.NewTLSServer(srv.Handler())
	os.WriteFile(cfg.SetupToken, []byte("token-123\n"), 0o600)
	t.Cleanup(func() { ts.Close(); lg.Close(); st.Close() })
	return ts, "token-123"
}

func newClient(t *testing.T, srv *httptest.Server) *client {
	jar, _ := cookiejar.New(nil)
	c := *srv.Client() // a copy: srv.Client() is shared, and each client needs its own cookie jar
	c.Jar = jar
	return &client{t: t, srv: srv, c: &c}
}

func (c *client) req(method, path, key string, body any) (int, map[string]any, string) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, c.srv.URL+path, rd)
	r.Header.Set("Content-Type", "application/json")
	if method != "GET" {
		r.Header.Set("X-CSRF-Token", c.csrf)
		if key != "-" {
			r.Header.Set("Idempotency-Key", key)
		}
	}
	resp, err := c.c.Do(r)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

var n int

func k() string { n++; return fmt.Sprintf("key-%d", n) }

func (c *client) login(user, pw string) {
	code, m, raw := c.req("POST", "/api/v1/auth/login", k(), map[string]any{"username": user, "password": pw})
	if code != 200 {
		c.t.Fatalf("login %s: %d %s", user, code, raw)
	}
	c.csrf = m["csrf_token"].(string)
}

const pw = "correct-horse-battery"

func TestFirstRunAndSessions(t *testing.T) {
	srv, token := newServer(t)
	c := newClient(t, srv)
	// Signed out: the session endpoint answers 200 and says setup is needed.
	code, m, _ := c.req("GET", "/api/v1/auth/session", "", nil)
	if code != 200 || m["needs_setup"] != true || m["user"] != nil {
		t.Fatalf("session before setup: %d %v", code, m)
	}
	if code, _, _ := c.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": "wrong", "username": "admin", "password": pw}); code != 403 {
		t.Fatalf("wrong setup token: %d", code)
	}
	if code, _, raw := c.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin", "password": pw, "time_zone": "UTC"}); code != 201 {
		t.Fatalf("setup: %d %s", code, raw)
	}
	if code, _, _ := c.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin2", "password": pw}); code != 409 {
		t.Fatalf("second setup: %d", code)
	}
	if code, _, _ := c.req("GET", "/api/v1/migrations", "", nil); code != 401 {
		t.Fatalf("signed out list: %d", code)
	}
	c.login("admin", pw)
	code, m, _ = c.req("GET", "/api/v1/auth/session", "", nil)
	if code != 200 || m["user"] == nil {
		t.Fatalf("session after login: %d %v", code, m)
	}
	// Short passwords are refused.
	if code, _, _ := c.req("POST", "/api/v1/users", k(), map[string]any{"username": "v", "password": "short", "role": "viewer"}); code != 400 {
		t.Fatalf("short password: %d", code)
	}
	c.req("POST", "/api/v1/users", k(), map[string]any{"username": "viewer", "password": pw, "role": "viewer"})
	// CSRF and idempotency keys are required on changes.
	good := c.csrf
	c.csrf = "nope"
	if code, _, _ := c.req("POST", "/api/v1/migrations", k(), map[string]any{"name": "x"}); code != 403 {
		t.Fatalf("bad csrf: %d", code)
	}
	c.csrf = good
	if code, _, _ := c.req("POST", "/api/v1/migrations", "-", map[string]any{"name": "x"}); code != 400 {
		t.Fatalf("no idempotency key: %d", code)
	}
	// Coming-soon features answer 501 with a clear message, never a silent no-op.
	// (Out of scope: they need an external service.)
	for _, p := range []string{"/api/v1/do/token"} {
		code, m, _ := c.req("POST", p, k(), map[string]any{})
		if code != 501 || m["code"] != "coming_soon" {
			t.Errorf("%s: %d %v", p, code, m)
		}
	}
	// Viewers cannot change anything.
	v := newClient(t, srv)
	v.login("viewer", pw)
	if code, _, _ := v.req("POST", "/api/v1/migrations", k(), map[string]any{"name": "x"}); code != 403 {
		t.Fatalf("viewer create: %d", code)
	}
	// The audit chain is intact after all of that.
	code, m, _ = c.req("GET", "/api/v1/audit/verify", "", nil)
	if code != 200 || m["ok"] != true {
		t.Fatalf("audit verify: %d %v", code, m)
	}
}

func TestIdempotentReplay(t *testing.T) {
	srv, token := newServer(t)
	c := newClient(t, srv)
	c.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin", "password": pw})
	c.login("admin", pw)
	var wg sync.WaitGroup
	bodies := make([]string, 8)
	codes := make([]int, 8)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, bodies[i] = c.req("POST", "/api/v1/migrations", "same-key", map[string]any{"name": "Replay"})
		}(i)
	}
	wg.Wait()
	for i := range bodies {
		if codes[i] != 201 || bodies[i] != bodies[0] {
			t.Fatalf("replay %d: %d %s (first %s)", i, codes[i], bodies[i], bodies[0])
		}
	}
	code, _, raw := c.req("GET", "/api/v1/migrations", "", nil)
	if code != 200 || strings.Count(raw, `"name":"Replay"`) != 1 {
		t.Fatalf("migrations after 8 submissions with one key: %s", raw)
	}
	// A different body under a used key is refused rather than silently replayed.
	if code, m, _ := c.req("POST", "/api/v1/migrations", "same-key", map[string]any{"name": "Other"}); code != 422 || m["code"] != "idempotency_key_reused" {
		t.Fatalf("reused key with another body: %d %v", code, m)
	}
}

// Idempotency keys are private to each user: the same key from another user
// runs that user's own request instead of replaying someone else's answer.
func TestIdempotencyKeysArePerUser(t *testing.T) {
	srv, token := newServer(t)
	a := newClient(t, srv)
	a.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin", "password": pw})
	a.login("admin", pw)
	a.req("POST", "/api/v1/users", k(), map[string]any{"username": "op", "password": pw, "role": "operator"})
	o := newClient(t, srv)
	o.login("op", pw)
	codeA, _, rawA := a.req("POST", "/api/v1/migrations", "shared-key", map[string]any{"name": "From admin"})
	codeO, _, rawO := o.req("POST", "/api/v1/migrations", "shared-key", map[string]any{"name": "From operator"})
	if codeA != 201 || codeO != 201 || rawA == rawO {
		t.Fatalf("same key, two users: %d %s / %d %s", codeA, rawA, codeO, rawO)
	}
	_, _, raw := a.req("GET", "/api/v1/migrations", "", nil)
	if !strings.Contains(raw, "From admin") || !strings.Contains(raw, "From operator") {
		t.Fatalf("migrations: %s", raw)
	}
	idemLocks.Lock()
	left := len(idemLocks.m)
	idemLocks.Unlock()
	if left != 0 {
		t.Fatalf("%d idempotency lock entries left behind", left)
	}
}

func TestUserPatchSemantics(t *testing.T) {
	srv, token := newServer(t)
	a := newClient(t, srv)
	a.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin", "password": pw})
	a.login("admin", pw)
	_, u, _ := a.req("POST", "/api/v1/users", k(), map[string]any{"username": "eve", "password": pw, "role": "viewer"})
	id := u["id"].(string)
	path := "/api/v1/users/" + id
	if code, m, _ := a.req("PATCH", path, k(), map[string]any{"disabled": true}); code != 200 || m["disabled"] != true || m["role"] != "viewer" {
		t.Fatalf("disable: %d %v", code, m)
	}
	// A PATCH without "disabled" leaves the user disabled.
	if code, m, _ := a.req("PATCH", path, k(), map[string]any{"role": "operator"}); code != 200 || m["disabled"] != true || m["role"] != "operator" {
		t.Fatalf("role only: %d %v", code, m)
	}
	// A bad password is refused before the role is applied.
	if code, _, _ := a.req("PATCH", path, k(), map[string]any{"role": "admin", "password": "short"}); code != 400 {
		t.Fatalf("short password: %d", code)
	}
	if _, m, _ := a.req("PATCH", path, k(), map[string]any{}); m["role"] != "operator" {
		t.Fatalf("role changed by a refused PATCH: %v", m)
	}
	// Removing your own Admin role is refused; resetting your own password keeps you signed in.
	_, me, _ := a.req("GET", "/api/v1/auth/me", "", nil)
	selfPath := "/api/v1/users/" + me["user"].(map[string]any)["id"].(string)
	if code, _, _ := a.req("PATCH", selfPath, k(), map[string]any{"role": "viewer"}); code != 400 {
		t.Fatalf("self demotion: %d", code)
	}
	if code, _, raw := a.req("PATCH", selfPath, k(), map[string]any{"password": "a-new-long-password"}); code != 200 {
		t.Fatalf("self password reset: %d %s", code, raw)
	}
	if code, _, _ := a.req("GET", "/api/v1/users", "", nil); code != 200 {
		t.Fatalf("signed out by own reset: %d", code)
	}
	_, _, raw := a.req("GET", "/api/v1/audit?action=user.update", "", nil)
	if n := strings.Count(raw, `"action":"user.update"`); n != 4 {
		t.Fatalf("user.update audit entries: %d (%s)", n, raw)
	}
}

func TestLoginFailedAuditAndRateLimit(t *testing.T) {
	srv, token := newServer(t)
	a := newClient(t, srv)
	a.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin", "password": pw})
	x := newClient(t, srv)
	x.req("POST", "/api/v1/auth/login", "-", map[string]any{"username": "my-secret-password", "password": "x"})
	x.req("POST", "/api/v1/auth/login", "-", map[string]any{"username": "admin", "password": "wrong"})
	a.login("admin", pw)
	_, _, raw := a.req("GET", "/api/v1/audit?action=auth.login_failed", "", nil)
	if strings.Contains(raw, "my-secret-password") || !strings.Contains(raw, `"target":"unknown:`) || !strings.Contains(raw, `"target":"admin"`) {
		t.Fatalf("login_failed entries: %s", raw)
	}
	// 4 attempts so far from this address (setup, 2 failures, 1 sign-in); the 11th is refused.
	for i := 0; i < 6; i++ {
		x.req("POST", "/api/v1/auth/login", "-", map[string]any{"username": "admin", "password": "wrong"})
	}
	if code, m, _ := x.req("POST", "/api/v1/auth/login", "-", map[string]any{"username": "admin", "password": pw}); code != 429 {
		t.Fatalf("11th attempt: %d %v", code, m)
	}
}

func TestOpenAPIListsEveryRoute(t *testing.T) {
	srv, _ := newServer(t)
	c := newClient(t, srv)
	code, m, _ := c.req("GET", "/api/v1/openapi.json", "", nil)
	if code != 200 {
		t.Fatal(code)
	}
	paths := m["paths"].(map[string]any)
	for _, p := range []string{"/api/v1/migrations/{id}/cutover", "/api/v1/migrations/{id}/verification", "/api/v1/stream/logs", "/api/v1/auth/session"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("openapi lacks %s", p)
		}
	}
}

// totpNow is the current code for a secret.
func totpNow(t *testing.T, secret string, offsetSteps int64) string {
	t.Helper()
	c, err := auth.TOTPCode(secret, auth.TOTPStep(time.Now())+offsetSteps)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestTOTP (overnight item 3, bug bash 8): enrollment, sign-in with the
// second factor, clock skew of one step each way, replay refused, recovery
// codes used once, lockout counting wrong codes, Admin enforcement and the
// Admin reset for a lost device.
func TestTOTP(t *testing.T) {
	srv, token := newServer(t)
	a := newClient(t, srv)
	pw := "correct-horse-battery-staple"
	a.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin", "password": pw})
	a.login("admin", pw)
	a.req("POST", "/api/v1/users", k(), map[string]any{"username": "olga", "password": pw, "role": "operator"})
	o := newClient(t, srv)
	o.login("olga", pw)

	// Enroll: a wrong code is refused, the right one turns it on.
	code, m, raw := o.req("POST", "/api/v1/auth/totp/enroll", k(), map[string]any{})
	if code != 200 || !strings.HasPrefix(fmt.Sprint(m["otpauth_uri"]), "otpauth://totp/Upwell:olga?") {
		t.Fatalf("enroll: %d %s", code, raw)
	}
	secret := fmt.Sprint(m["secret"])
	if code, _, _ := o.req("POST", "/api/v1/auth/totp/confirm", k(), map[string]any{"code": "000000"}); code != 422 {
		t.Fatalf("wrong confirm code: %d", code)
	}
	// One step behind (the phone's clock is slow) is accepted.
	code, m, raw = o.req("POST", "/api/v1/auth/totp/confirm", k(), map[string]any{"code": totpNow(t, secret, -1)})
	if code != 200 {
		t.Fatalf("confirm: %d %s", code, raw)
	}
	var recovery []string
	for _, x := range m["recovery_codes"].([]any) {
		recovery = append(recovery, fmt.Sprint(x))
	}
	if len(recovery) != 10 {
		t.Fatalf("%d recovery codes", len(recovery))
	}

	signIn := func(otp string) (int, string) {
		c := newClient(t, srv)
		code, m, _ := c.req("POST", "/api/v1/auth/login", k(), map[string]any{"username": "olga", "password": pw, "otp": otp})
		return code, fmt.Sprint(m["code"])
	}
	// Password alone: asked for the code, not counted as a failure.
	if code, c := signIn(""); code != 401 || c != "mfa_required" {
		t.Fatalf("no code: %d %s", code, c)
	}
	// One step ahead is accepted too; each step only once.
	if code, c := signIn(totpNow(t, secret, 1)); code != 200 {
		t.Fatalf("one step ahead: %d %s", code, c)
	}
	// Replay: a code at or before the last accepted step is refused.
	if code, c := signIn(totpNow(t, secret, 1)); code != 401 || c != "bad_otp" {
		t.Fatalf("replayed code: %d %s", code, c)
	}
	// Two steps away is refused.
	if code, _ := signIn(totpNow(t, secret, 3)); code != 401 {
		t.Fatalf("three steps ahead accepted: %d", code)
	}
	// A recovery code works once.
	if code, c := signIn(recovery[0]); code != 200 {
		t.Fatalf("recovery code: %d %s", code, c)
	}
	if code, _ := signIn(recovery[0]); code != 401 {
		t.Fatalf("a recovery code worked twice: %d", code)
	}
	// (Wrong codes counting towards the lockout: TestTOTPLockout in
	// internal/auth, past this endpoint's per-address rate limit.)
	// An Admin resets olga's second factor (lost device), and the lockout.
	if code, _, _ := a.req("POST", "/api/v1/users/"+userID(t, a, "olga")+"/totp/reset", k(), map[string]any{}); code != 200 {
		t.Fatalf("reset: %d", code)
	}
	if code, c := signIn(""); code != 200 {
		t.Fatalf("after the reset olga signs in with her password: %d %s", code, c)
	}

	// Admin enforcement: with mfa_required_for_admins on, an Admin without the
	// second factor can only set it up.
	a.req("PATCH", "/api/v1/settings", k(), map[string]any{"mfa_required_for_admins": true})
	if code, m, _ := a.req("GET", "/api/v1/migrations", "", nil); code != 403 || m["code"] != "mfa_enrollment_required" {
		t.Fatalf("enforcement: %d %v", code, m)
	}
	code, m, _ = a.req("GET", "/api/v1/auth/me", "", nil)
	if code != 200 || m["mfa_enrollment_required"] != true {
		t.Fatalf("me: %d %v", code, m)
	}
	code, m, _ = a.req("POST", "/api/v1/auth/totp/enroll", k(), map[string]any{})
	if code != 200 {
		t.Fatalf("admin enroll under enforcement: %d", code)
	}
	as := fmt.Sprint(m["secret"])
	if code, _, raw := a.req("POST", "/api/v1/auth/totp/confirm", k(), map[string]any{"code": totpNow(t, as, 0)}); code != 200 {
		t.Fatalf("admin confirm: %d %s", code, raw)
	}
	if code, _, _ := a.req("GET", "/api/v1/migrations", "", nil); code != 200 {
		t.Fatalf("after enrolling: %d", code)
	}
	// An Admin cannot turn it off while it is required.
	if code, _, _ := a.req("POST", "/api/v1/auth/totp/disable", k(), map[string]any{"code": totpNow(t, as, 1)}); code != 409 {
		t.Fatalf("admin disable under enforcement: %d", code)
	}
	// The audit log records each step.
	code, m, raw = a.req("GET", "/api/v1/audit?limit=200", "", nil)
	for _, want := range []string{"auth.totp_enabled", "auth.recovery_code_used", "auth.totp_reset"} {
		if !strings.Contains(raw, want) {
			t.Errorf("audit log lacks %s", want)
		}
	}
	_ = code
}

func userID(t *testing.T, c *client, name string) string {
	t.Helper()
	_, _, raw := c.req("GET", "/api/v1/users", "", nil)
	var us []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	json.Unmarshal([]byte(raw), &us)
	for _, u := range us {
		if u.Username == name {
			return u.ID
		}
	}
	t.Fatalf("no user %s", name)
	return ""
}

// TestAPITokens (overnight item 4, bug bash 8): read-only, scoped, shown
// once, hashed at rest, and a revoked token fails on its next request.
func TestAPITokens(t *testing.T) {
	srv, token := newServer(t)
	a := newClient(t, srv)
	pw := "correct-horse-battery-staple"
	a.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin", "password": pw})
	a.login("admin", pw)
	code, m, raw := a.req("POST", "/api/v1/api-tokens", k(), map[string]any{"name": "ci reader", "scope": "read", "expires_days": 30})
	if code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	tok := fmt.Sprint(m["token"])
	id := fmt.Sprint(m["record"].(map[string]any)["id"])
	_, _, met := a.req("POST", "/api/v1/api-tokens", k(), map[string]any{"name": "prometheus", "scope": "metrics"})
	var mm map[string]any
	json.Unmarshal([]byte(met), &mm)
	mtok := fmt.Sprint(mm["token"])

	bearer := func(method, path, tk string) int {
		r, _ := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+tk)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "x-"+path)
		res, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := bearer("GET", "/api/v1/migrations", tok); c != 200 {
		t.Fatalf("read with a token: %d", c)
	}
	if c := bearer("POST", "/api/v1/migrations", tok); c != 403 {
		t.Fatalf("a token changed something: %d", c)
	}
	if c := bearer("GET", "/api/v1/users", tok); c != 403 {
		t.Fatalf("a token read an Admin endpoint: %d", c)
	}
	if c := bearer("GET", "/api/v1/migrations", mtok); c != 403 {
		t.Fatalf("a metrics token read the API: %d", c)
	}
	if c := bearer("GET", "/api/v1/migrations", "upw_not-a-real-token"); c != 401 {
		t.Fatalf("an unknown token: %d", c)
	}
	// Listing never shows the token, only its prefix.
	_, _, list := a.req("GET", "/api/v1/api-tokens", "", nil)
	if strings.Contains(list, tok) || !strings.Contains(list, tok[:10]) {
		t.Fatalf("token list: %s", list)
	}
	// Revoked: the very next request fails.
	if code, _, _ := a.req("DELETE", "/api/v1/api-tokens/"+id, k(), nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if c := bearer("GET", "/api/v1/migrations", tok); c != 401 {
		t.Fatalf("a revoked token still works: %d", c)
	}
	_, _, audit := a.req("GET", "/api/v1/audit?limit=100", "", nil)
	if !strings.Contains(audit, "api_token.create") || !strings.Contains(audit, "api_token.revoke") || strings.Contains(audit, tok) {
		t.Fatal("audit log: missing entries, or it contains the token")
	}
}

// TestPrometheusMetrics (overnight item 5): /metrics needs a token (metrics
// or read scope) or an Admin session, and exports no secret.
func TestPrometheusMetrics(t *testing.T) {
	srv, token := newServer(t)
	a := newClient(t, srv)
	pw := "correct-horse-battery-staple"
	a.req("POST", "/api/v1/setup", k(), map[string]any{"setup_token": token, "username": "admin", "password": pw})
	a.login("admin", pw)
	canary := "canary-secret-7f3a9c2e"
	_, m, _ := a.req("POST", "/api/v1/migrations", k(), map[string]any{"name": "Metrics test"})
	id := fmt.Sprint(m["id"])
	a.req("PUT", "/api/v1/migrations/"+id+"/connections/source", k(), map[string]any{"host": "db.internal.example", "port": 25060, "user": "doadmin", "password": canary, "sslmode": "require"})
	_, tm, _ := a.req("POST", "/api/v1/api-tokens", k(), map[string]any{"name": "prom", "scope": "metrics"})
	tok := fmt.Sprint(tm["token"])
	get := func(tk string) (int, string) {
		r, _ := http.NewRequest("GET", srv.URL+"/metrics", nil)
		if tk != "" {
			r.Header.Set("Authorization", "Bearer "+tk)
		}
		res, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if c, _ := get(""); c != 401 {
		t.Fatalf("/metrics without a token: %d", c)
	}
	c, body := get(tok)
	if c != 200 || !strings.Contains(body, `upwell_build_info{version="test"} 1`) || !strings.Contains(body, `upwell_migration_info{`) || !strings.Contains(body, `upwell_alerts_firing{severity="critical"} 0`) {
		t.Fatalf("/metrics: %d\n%s", c, body)
	}
	for _, secret := range []string{canary, tok, "db.internal.example"} {
		if strings.Contains(body, secret) {
			t.Fatalf("/metrics exports %q", secret)
		}
	}
	if code, _, _ := a.req("GET", "/metrics", "", nil); code != 200 {
		t.Fatalf("/metrics with an Admin session: %d", code)
	}
}
