//go:build integration

// Package integration drives Upwell through its HTTP API against the local
// Standard-like source and Advanced-like target from spikes/env/local-clusters.sh.
// Run: go test -tags integration ./test/integration/ (as root, clusters up).
package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/api"
	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/auth"
	"github.com/pagombin/upwell/internal/bus"
	"github.com/pagombin/upwell/internal/config"
	"github.com/pagombin/upwell/internal/engine/pgcopydb"
	"github.com/pagombin/upwell/internal/logx"
	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/runner"
	"github.com/pagombin/upwell/internal/secrets"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
)

const (
	sockDir  = "/var/tmp/upwell-spike"
	srcPort  = 55432
	dstPort  = 55433
	password = "spike-password-not-secret"
)

var srcConn = pg.Conn{Host: "127.0.0.1", Port: srcPort, User: "doadmin", Password: password, DBName: "defaultdb", SSLMode: "disable"}
var dstConn = pg.Conn{Host: "127.0.0.1", Port: dstPort, User: "doadmin", Password: password, DBName: "defaultdb", SSLMode: "disable"}

// Env is one in-process Upwell with its own store.
type Env struct {
	t      *testing.T
	Dir    string
	Cfg    config.Config
	Store  *store.Store
	Orch   *orch.Orchestrator
	Srv    *httptest.Server
	client *http.Client
	csrf   string
	cancel context.CancelFunc
	log    *logx.Logger
}

func TestMain(m *testing.M) {
	if out, err := exec.Command("bash", "../../spikes/env/local-clusters.sh", "up").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "local clusters are not available: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// newEnv starts Upwell on a fresh store under dir (a new temp dir when "").
func newEnv(t *testing.T, dir string, hooks orch.TestHooks) *Env {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.LogDir = filepath.Join(dir, "log")
	cfg.MasterKey = filepath.Join(dir, "master.key")
	cfg.SetupToken = filepath.Join(dir, "setup-token")
	cfg.Engine.Runner = "local"
	cfg.Dev = true
	os.MkdirAll(cfg.RunsDir(), 0o750)
	st, err := store.Open(cfg.StorePath())
	if err != nil {
		t.Fatal(err)
	}
	key, err := secrets.LoadOrCreateKey(cfg.MasterKey, true)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(key)
	b := bus.New(4096)
	lg, err := logx.New(cfg.LogDir, st, b, "debug", os.Getenv("UPWELL_TEST_LOGS") == "1")
	if err != nil {
		t.Fatal(err)
	}
	vault := &secrets.Vault{Box: box, Store: st}
	set := &settings.Service{Store: st}
	r, err := runner.New("local", filepath.Join(cfg.DataDir, "units"))
	if err != nil {
		t.Fatal(err)
	}
	au := &audit.Log{Store: st}
	o := orch.New(orch.Deps{Config: cfg, Store: st, Vault: vault, Engine: pgcopydb.New("pgcopydb", r), Runner: r, Logger: lg, Bus: b, Audit: au, Settings: set})
	o.SetTestHooks(hooks)
	as := auth.NewService(st)
	as.FastHashForTests()
	health := api.NewHealth()
	health.Ready = func() (bool, map[string]any) { return true, map[string]any{} }
	o.Heartbeat = health.Beat
	srv := api.New(&api.Server{Cfg: cfg, Store: st, Auth: as, Audit: au, Orch: o, Settings: set, Bus: b, Log: lg, Version: "test", Health: health,
		TLSInfo: func() api.TLSInfo { return api.TLSInfo{} }})
	ctx, cancel := context.WithCancel(context.Background())
	e := &Env{t: t, Dir: dir, Cfg: cfg, Store: st, Orch: o, cancel: cancel, log: lg}
	// Fast settings for tests.
	for k, v := range map[string]any{"wal_sample_seconds": 10, "cutover_stable_seconds": 5, "cutover_write_sample_seconds": 5, "sample_interval_seconds": 2,
		"alert_disk_crit_pct": 99, "alert_disk_warn_pct": 98, "disk_guard_stop_pct": 99, "cutover_drain_timeout_seconds": 120, "retry_base_seconds": 2} {
		if _, _, err := set.Set(ctx, k, v, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := as.CountUsers(ctx); n == 0 {
		if _, err := as.CreateUser(ctx, "admin", "correct-horse-battery", "admin"); err != nil {
			t.Fatal(err)
		}
		as.CreateUser(ctx, "viewer", "correct-horse-battery", "viewer")
		as.CreateUser(ctx, "operator", "correct-horse-battery", "operator")
	}
	e.Srv = httptest.NewTLSServer(srv.Handler())
	go o.Run(ctx)
	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 120 * time.Second}
	e.login("admin")
	t.Cleanup(func() { e.Close() })
	return e
}

// Close stops the server and orchestrator (engine units keep running, as in production).
func (e *Env) Close() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	e.Srv.Close()
	time.Sleep(300 * time.Millisecond)
	e.log.Close()
	e.Store.Close()
	e.cancel = nil
}

func (e *Env) login(user string) {
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	e.do("POST", "/api/v1/auth/login", map[string]any{"username": user, "password": "correct-horse-battery"}, &out, 200)
	e.csrf = out.CSRF
}

// do sends a request with a fresh idempotency key and decodes the JSON reply.
func (e *Env) do(method, path string, body any, out any, want ...int) int {
	return e.doKey(method, path, fmt.Sprintf("k-%d-%d", time.Now().UnixNano(), rand.Int()), body, out, want...)
}

func (e *Env) doKey(method, path, key string, body any, out any, want ...int) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.Srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if method != "GET" {
		req.Header.Set("X-CSRF-Token", e.csrf)
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if len(want) > 0 {
		ok := false
		for _, w := range want {
			if resp.StatusCode == w {
				ok = true
			}
		}
		if !ok {
			e.t.Fatalf("%s %s: status %d, want %v: %s", method, path, resp.StatusCode, want, raw)
		}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: decoding %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func (e *Env) raw(method, path string) (int, string) {
	req, _ := http.NewRequest(method, e.Srv.URL+path, nil)
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ---------- cluster helpers

func superSQL(t *testing.T, port int, db, sql string) string {
	t.Helper()
	out, err := exec.Command("runuser", "-u", "postgres", "--", "psql", "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1", "-h", sockDir, "-p", fmt.Sprint(port), "-d", db, "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %s: %v: %s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

func q(t *testing.T, c pg.Conn, db, sql string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pg.Connect(ctx, c.WithDB(db), time.Minute)
	if err != nil {
		t.Fatalf("connect %s: %v", db, err)
	}
	defer conn.Close(ctx)
	var s *string
	if err := conn.QueryRow(ctx, sql).Scan(&s); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return ""
		}
		t.Fatalf("%s: %v", sql, err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func exec1(t *testing.T, c pg.Conn, db, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pg.Connect(ctx, c.WithDB(db), 5*time.Minute)
	if err != nil {
		t.Fatalf("connect %s: %v", db, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// seedDB creates a source database with orders and items tables.
func seedDB(t *testing.T, name string, rows int) {
	t.Helper()
	dropDB(t, name)
	exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+name)
	exec1(t, srcConn, name, fmt.Sprintf(`
		CREATE TABLE orders(id bigserial PRIMARY KEY, customer int NOT NULL, amount numeric(12,2), note text, created_at timestamptz DEFAULT now());
		CREATE TABLE items(order_id bigint REFERENCES orders(id), line int, sku text, qty int, PRIMARY KEY(order_id, line));
		INSERT INTO orders(customer, amount, note) SELECT g%%1000, (g%%9999)/100.0, md5(g::text) FROM generate_series(1,%d) g;
		INSERT INTO items SELECT id, l, 'sku-'||(id*l)%%5000, l FROM orders, generate_series(1,2) l;
		CREATE INDEX ON orders(customer);
		ANALYZE orders, items;`, rows))
	t.Cleanup(func() { dropDB(t, name) })
}

func dropDB(t *testing.T, name string) {
	ctx := context.Background()
	for _, c := range []pg.Conn{srcConn, dstConn} {
		if conn, err := pg.Connect(ctx, c, time.Minute); err == nil {
			conn.Exec(ctx, `SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE database=$1 AND active_pid IS NOT NULL`, name)
			time.Sleep(200 * time.Millisecond)
			conn.Exec(ctx, `SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE database=$1`, name)
			conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pg.QuoteIdent(name)+" WITH (FORCE)")
			conn.Close(ctx)
		}
	}
}

// writer commits one INSERT and one UPDATE every ~50 ms until stopped.
type writer struct {
	stop chan struct{}
	done chan struct{}
	n    int
}

func startWriter(t *testing.T, db string) *writer {
	w := &writer{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ctx := context.Background()
		conn, err := pg.Connect(ctx, srcConn.WithDB(db), time.Minute)
		if err != nil {
			return
		}
		defer conn.Close(ctx)
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			if _, err := conn.Exec(ctx, `INSERT INTO orders(customer, amount, note) VALUES ((random()*1000)::int, 1.00, 'w'); UPDATE orders SET amount=amount+1 WHERE id=(SELECT max(id)-5 FROM orders)`); err == nil {
				w.n++
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return w
}

func (w *writer) Stop() { close(w.stop); <-w.done }

// ---------- migration helpers

type migView struct {
	Migration orch.Migration  `json:"migration"`
	Databases []orch.Database `json:"databases"`
}

func (e *Env) migration(id string) migView {
	var v migView
	e.do("GET", "/api/v1/migrations/"+id, nil, &v, 200)
	return v
}

func (e *Env) db(id, name string) orch.Database {
	for _, d := range e.migration(id).Databases {
		if d.SourceName == name {
			return d
		}
	}
	e.t.Fatalf("database %s not in migration", name)
	return orch.Database{}
}

// defineMigration creates a migration for dbs up to Ready (preflight passed).
func (e *Env) defineMigration(name string, dbs []string, overrides map[string]any) string {
	e.t.Helper()
	var m orch.Migration
	e.do("POST", "/api/v1/migrations", map[string]any{"name": name}, &m, 201)
	e.t.Cleanup(func() {
		// Best effort: never leave engines or slots behind when a test fails.
		if e.cancel == nil {
			return
		}
		v := e.migration(m.ID)
		if v.Migration.Flags.CleanedUp || !v.Migration.Flags.Started {
			return
		}
		e.do("POST", "/api/v1/migrations/"+m.ID+"/abort", map[string]any{"confirm": name}, nil)
		var r struct {
			OperationID string `json:"operation_id"`
		}
		if e.do("POST", "/api/v1/migrations/"+m.ID+"/cleanup", map[string]any{"confirm": name}, &r) == 202 {
			e.waitOp(r.OperationID, 3*time.Minute)
		}
	})
	e.do("PUT", "/api/v1/migrations/"+m.ID+"/permission", map[string]any{"customer": "Test Customer", "account_id": "team-1", "ticket": "TEST-1", "granted_by": "tester@example.com", "granted_at": "2026-10-08", "scope": "test databases"}, nil, 200)
	e.do("PUT", "/api/v1/migrations/"+m.ID+"/connections/source", map[string]any{"host": "127.0.0.1", "port": srcPort, "user": "doadmin", "password": password, "sslmode": "disable"}, nil, 200)
	e.do("PUT", "/api/v1/migrations/"+m.ID+"/connections/target", map[string]any{"host": "127.0.0.1", "port": dstPort, "user": "doadmin", "password": password, "sslmode": "disable", "storage_gb": 100}, nil, 200)
	e.do("POST", "/api/v1/migrations/"+m.ID+"/discover", nil, nil, 200)
	var sel []map[string]any
	for _, d := range dbs {
		sel = append(sel, map[string]any{"source_name": d, "target_name": d, "include": true})
	}
	// Exclude everything else discovered.
	all := e.migration(m.ID).Databases
	for _, d := range all {
		found := false
		for _, x := range dbs {
			if x == d.SourceName {
				found = true
			}
		}
		if !found && d.SkipReason == "" {
			sel = append(sel, map[string]any{"source_name": d.SourceName, "target_name": d.SourceName, "include": false})
		}
	}
	e.do("PUT", "/api/v1/migrations/"+m.ID+"/databases", sel, nil, 200)
	if overrides != nil {
		e.do("PATCH", "/api/v1/migrations/"+m.ID, map[string]any{"settings": overrides}, nil, 200)
	}
	return m.ID
}

type preflightView struct {
	Run     *orch.PreflightRun `json:"run"`
	Results []struct {
		ID       string `json:"id"`
		CheckID  string `json:"check_id"`
		Database string `json:"database"`
		Level    string `json:"level"`
		Hard     bool   `json:"hard"`
		Message  string `json:"message"`
		Accepted any    `json:"accepted"`
	} `json:"results"`
}

func (e *Env) preflight(id string) preflightView {
	e.t.Helper()
	e.do("POST", "/api/v1/migrations/"+id+"/preflight", nil, nil, 202)
	var pv preflightView
	e.waitFor(3*time.Minute, "preflight to finish", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/preflight/latest", nil, &pv, 200)
		return pv.Run != nil && pv.Run.State == "done"
	})
	return pv
}

func (e *Env) waitFor(d time.Duration, what string, f func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.t.Fatalf("timed out after %s waiting for %s", d, what)
}

func (e *Env) waitDBState(id, db string, d time.Duration, states ...string) orch.Database {
	e.t.Helper()
	var last orch.Database
	defer func() {
		if e.t.Failed() {
			backlog := int64(-1)
			if last.BacklogBytes != nil {
				backlog = *last.BacklogBytes
			}
			e.t.Logf("%s last seen: state %s, backlog %d bytes, retries %d, error class %q, error %q", db, last.State, backlog, last.RetryCount, last.ErrorClass, last.LastError)
		}
	}()
	e.waitFor(d, db+" to reach "+strings.Join(states, "/"), func() bool {
		last = e.db(id, db)
		for _, s := range states {
			if last.State == s {
				return true
			}
		}
		return false
	})
	return last
}

func (e *Env) waitOp(opID string, d time.Duration) *orch.Operation {
	e.t.Helper()
	var op orch.Operation
	e.waitFor(d, "operation "+opID, func() bool {
		e.do("GET", "/api/v1/operations/"+opID, nil, &op, 200)
		return op.State != "running"
	})
	return &op
}

func (e *Env) startCutover(id string) *orch.Operation {
	e.t.Helper()
	var rd struct {
		Ready      bool             `json:"ready"`
		Conditions []orch.Condition `json:"conditions"`
	}
	defer func() {
		if e.t.Failed() {
			for _, c := range rd.Conditions {
				e.t.Logf("readiness %-9s ok=%v %s: %s", c.Key, c.OK, c.Label, c.Detail)
			}
		}
	}()
	e.waitFor(15*time.Minute, "the readiness gate", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		return rd.Ready
	})
	var r struct {
		OperationID string `json:"operation_id"`
	}
	e.do("POST", "/api/v1/migrations/"+id+"/cutover", map[string]any{"confirm_writers_stopped": true}, &r, 202)
	return e.waitOp(r.OperationID, 25*time.Minute)
}

func (e *Env) cleanup(id, name string) {
	e.t.Helper()
	var r struct {
		OperationID string `json:"operation_id"`
	}
	e.do("POST", "/api/v1/migrations/"+id+"/cleanup", map[string]any{"confirm": name}, &r, 202)
	op := e.waitOp(r.OperationID, 3*time.Minute)
	if op.State != "done" {
		e.t.Fatalf("cleanup failed: %s", op.Error)
	}
	noLeftovers(e.t, e.migration(id).Migration.ShortID)
}

func (e *Env) abortAndClean(id, name string) {
	e.do("POST", "/api/v1/migrations/"+id+"/abort", map[string]any{"confirm": name}, nil)
	e.cleanup(id, name)
}

// noLeftovers asserts that a migration left nothing on the clusters.
func noLeftovers(t *testing.T, short string) {
	t.Helper()
	slots := q(t, srcConn, "defaultdb", fmt.Sprintf(`SELECT string_agg(slot_name, ',') FROM pg_replication_slots WHERE slot_name LIKE 'upwell_%s_%%'`, short))
	origins := q(t, dstConn, "defaultdb", fmt.Sprintf(`SELECT string_agg(roname, ',') FROM pg_replication_origin WHERE roname LIKE 'upwell_%s_%%'`, short))
	if slots != "" || origins != "" {
		t.Fatalf("leftovers after cleanup: slots %q, origins %q", slots, origins)
	}
}

func uniq(prefix string) string { return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()%1_000_000) }
