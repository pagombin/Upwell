//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

const pg18Port = 55434 // the PostgreSQL 18 target, built without ICU

var pg18Conn = pg.Conn{Host: "127.0.0.1", Port: pg18Port, User: "doadmin", Password: password, DBName: "defaultdb", SSLMode: "disable"}

func requirePG18(t *testing.T) {
	t.Helper()
	c, err := pg.Connect(context.Background(), pg18Conn, 10*time.Second)
	if err != nil {
		t.Skipf("the PostgreSQL 18 target on port %d is not running (spikes/env/local-clusters.sh with TARGET_ONLY=1): %v", pg18Port, err)
	}
	c.Close(context.Background())
}

func eventsOf(t *testing.T, e *Env, id string) []map[string]any {
	t.Helper()
	var ev []map[string]any
	e.do("GET", "/api/v1/migrations/"+id+"/events", nil, &ev, 200)
	return ev
}

// TestCollationFallbackWarned (audit finding 6, bug bash 6): the source
// database uses an ICU collation the target cannot provide (this PostgreSQL
// 18 build has no ICU). Upwell used to create the target database with the
// target's default locale and say nothing; it must warn and put it in the
// report.
func TestCollationFallbackWarned(t *testing.T) {
	requirePG18(t)
	db := uniq("it_icu")
	dropDB(t, db)
	exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+db+" TEMPLATE template0 LOCALE_PROVIDER icu ICU_LOCALE 'de-DE'")
	t.Cleanup(func() {
		dropDB(t, db)
		if c, err := pg.Connect(context.Background(), pg18Conn, time.Minute); err == nil {
			c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	exec1(t, srcConn, db, `CREATE TABLE words(id int PRIMARY KEY, w text); INSERT INTO words SELECT g, 'w'||g FROM generate_series(1, 100) g;`)
	e := newEnv(t, "", orch.TestHooks{})
	id := e.defineMigration("ICU fallback", []string{db}, map[string]any{"_target_port": pg18Port})
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	var warned string
	e.waitFor(2*time.Minute, "a collation warning event", func() bool {
		for _, ev := range eventsOf(t, e, id) {
			if ev["type"] == "collation_fallback" {
				warned, _ = ev["message"].(string)
				return true
			}
		}
		return false
	})
	t.Logf("event: %s", warned)
	if !strings.Contains(warned, "de-DE") {
		t.Errorf("the warning does not name the source's collation: %q", warned)
	}
	code, md := e.raw("GET", "/api/v1/migrations/"+id+"/report?format=md")
	if code != 200 || !strings.Contains(md, "does not match the source's ICU collation de-DE") {
		t.Errorf("the report (%d) does not list the collation fallback", code)
	}
	e.do("POST", "/api/v1/migrations/"+id+"/abort", map[string]any{"confirm": "ICU fallback"}, nil)
	var r struct {
		OperationID string `json:"operation_id"`
	}
	if e.do("POST", "/api/v1/migrations/"+id+"/cleanup", map[string]any{"confirm": "ICU fallback"}, &r) == 202 {
		e.waitOp(r.OperationID, 3*time.Minute)
	}
}

// TestTemplate0LaunchExplains (E7, preflight finding 8, bug bash 6): the
// source database's locale differs from the target's template1, so Upwell
// creates the target database from template0, which never has the function
// grants the platform ran in template1. Preflight passed (it checks the
// maintenance database); the launch re-check must block and say why.
func TestTemplate0LaunchExplains(t *testing.T) {
	db := uniq("it_tpl0")
	dropDB(t, db)
	exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+db+" TEMPLATE template0 LOCALE 'C'")
	t.Cleanup(func() { dropDB(t, db) })
	exec1(t, srcConn, db, `CREATE TABLE t(id int PRIMARY KEY); INSERT INTO t SELECT generate_series(1, 10);`)
	e := newEnv(t, "", orch.TestHooks{})
	id := e.defineMigration("Template0", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	var last string
	e.waitFor(2*time.Minute, "the launch to be blocked", func() bool {
		for _, d := range e.migration(id).Databases {
			if d.SourceName == db && d.State == orch.DFailed {
				last = d.LastError
				return true
			}
		}
		return false
	})
	t.Logf("launch: %s", last)
	if !strings.Contains(last, "template0") || !strings.Contains(last, "grant") {
		t.Fatalf("the launch block does not explain the template0 cause: %q", last)
	}
	e.do("POST", "/api/v1/migrations/"+id+"/abort", map[string]any{"confirm": "Template0"}, nil)
	var r struct {
		OperationID string `json:"operation_id"`
	}
	if e.do("POST", "/api/v1/migrations/"+id+"/cleanup", map[string]any{"confirm": "Template0"}, &r) == 202 {
		e.waitOp(r.OperationID, 3*time.Minute)
	}
}

// The droplet (2026-10-14): preflight blocked with "Collations differ
// (source en_US.UTF-8, target en_US.utf-8)", two names Upwell already treats
// as the same locale. The difference was elsewhere (the collation library's
// version, glibc on the source against the Percona image's), the message did
// not say so, and the same difference passed silently when Upwell created the
// target database itself. The engine builds every index on the target, so a
// version difference cannot leave an index out of order there; it can change
// how a few strings sort, which is a warning.
func TestCollationVersionDifferenceWarns(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_collver")
	seedDB(t, db, 100)
	superSQL(t, dstPort, "postgres", "CREATE DATABASE "+db+" OWNER doadmin TEMPLATE template0 LOCALE 'C.UTF-8'")
	superSQL(t, srcPort, "postgres", "UPDATE pg_database SET datcollversion = '2.31-test' WHERE datname = '"+db+"'")
	superSQL(t, dstPort, "postgres", "UPDATE pg_database SET datcollversion = '2.39-test' WHERE datname = '"+db+"'")
	id := e.defineMigration("Collation version", []string{db}, nil)
	pv := e.preflight(id)
	var got string
	for _, r := range pv.Results {
		if r.CheckID == "collation" {
			got = r.Level + ": " + r.Message
		}
	}
	t.Logf("existing target database: %s", got)
	if !strings.HasPrefix(got, "warning: ") || !strings.Contains(got, "2.31-test") || !strings.Contains(got, "2.39-test") {
		t.Fatalf("want a warning naming both collation versions, got %q", got)
	}

	// The same difference when Upwell will create the target database (from
	// template1, whose version the new database takes).
	superSQL(t, dstPort, "postgres", "DROP DATABASE "+db)
	prev := superSQL(t, dstPort, "postgres", "SELECT COALESCE(datcollversion, '') FROM pg_database WHERE datname = 'template1'")
	superSQL(t, dstPort, "postgres", "UPDATE pg_database SET datcollversion = '2.39-test' WHERE datname = 'template1'")
	t.Cleanup(func() {
		v := "NULL"
		if prev != "" {
			v = "'" + prev + "'"
		}
		superSQL(t, dstPort, "postgres", "UPDATE pg_database SET datcollversion = "+v+" WHERE datname = 'template1'")
	})
	pv = e.preflight(id)
	got = ""
	for _, r := range pv.Results {
		if r.CheckID == "collation" {
			got = r.Level + ": " + r.Message
		}
	}
	t.Logf("target database to be created: %s", got)
	if !strings.HasPrefix(got, "warning: ") || !strings.Contains(got, "2.31-test") || !strings.Contains(got, "2.39-test") {
		t.Fatalf("want the same warning when the target database will be created, got %q", got)
	}
	e.cleanup(id, "Collation version")
}
