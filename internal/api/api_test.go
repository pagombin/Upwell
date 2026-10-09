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
	r, _ := runner.New("local", filepath.Join(cfg.DataDir, "units"))
	au := &audit.Log{Store: st}
	vault := &secrets.Vault{Box: box, Store: st}
	o := orch.New(orch.Deps{Config: cfg, Store: st, Vault: vault, Engine: pgcopydb.New("pgcopydb", r), Runner: r, Logger: lg, Bus: b, Audit: au, Settings: set})
	as := auth.NewService(st)
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
	for _, p := range []string{"/api/v1/auth/totp", "/api/v1/do/token", "/api/v1/api-tokens"} {
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
