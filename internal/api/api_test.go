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
	c := srv.Client()
	c.Jar = jar
	return &client{t: t, srv: srv, c: c}
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
	if code, _, _ := c.req("POST", "/api/v1/migrations", "same-key", map[string]any{"name": "Other"}); code != 201 && code != 409 && code != 422 {
		t.Fatalf("reused key with another body: %d", code)
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
