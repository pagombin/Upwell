package pgcopydb

import (
	"strings"
	"testing"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/runner"
)

func spec() engine.DatabaseSpec {
	c := pg.Conn{Host: "db.example.com", Port: 25060, User: "doadmin", Password: "secret-pw", DBName: "defaultdb", SSLMode: "require"}
	return engine.DatabaseSpec{Source: "orders", Target: "orders", SourceConn: c, TargetConn: c, Instance: "m1-orders", SlotName: "upwell_m1_orders", OriginName: "upwell_m1_orders", Plugin: "pgoutput", RunDir: "/tmp/run", TableJobs: 4, IndexJobs: 2}
}

func TestPlan(t *testing.T) {
	r, err := runner.New("local", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Binary: "pgcopydb", Runner: r}
	p, err := e.Plan(spec(), false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(p.Args, " ")
	for _, want := range []string{"clone --follow", "--slot-name upwell_m1_orders", "--origin upwell_m1_orders", "--plugin pgoutput", "--table-jobs 4", "--index-jobs 2"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("plan lacks %q: %s", want, cmd)
		}
	}
	if strings.Contains(cmd, "secret-pw") || strings.Contains(p.Display, "secret-pw") {
		t.Fatal("the password reached the command line")
	}
	for _, a := range p.Args {
		if strings.ContainsAny(a, " \t\n") {
			t.Errorf("argument %q contains whitespace (the env file splits on spaces)", a)
		}
	}
	// Resume never passes --plugin and always continues the existing snapshot.
	rp, _ := e.Plan(spec(), true)
	rc := strings.Join(rp.Args, " ")
	if !strings.Contains(rc, "--resume --not-consistent") || strings.Contains(rc, "--plugin") {
		t.Fatalf("resume plan: %s", rc)
	}
	// The coordinator is never used (PC12).
	s := spec()
	s.ExtraArgs = []string{"--host", "x"}
	if _, err := e.Plan(s, false); err == nil {
		t.Fatal("--host accepted")
	}
	s = spec()
	s.Source = "bad;name"
	if _, err := e.Plan(s, false); err == nil {
		t.Fatal("an invalid database name was accepted")
	}
}

func TestClassify(t *testing.T) {
	e := &Engine{}
	cases := []struct{ line, want string }{
		{"2026-10-07 10:00:00.123 4242 ERROR  pgsql.c:123  ERROR:  can no longer get changes from replication slot \"upwell_m1_orders\"", engine.FailSlotLost},
		// Slot loss is decided by the line even when it is not logged as an error.
		{"2026-10-07 10:00:00.123 4242 WARN   ld_stream.c:1  slot has been invalidated because it exceeded the maximum reserved size", engine.FailSlotLost},
		{"2026-10-07 10:00:00.123 4242 ERROR  cli_clone_follow.c:1  clone process 4243 has terminated", engine.FailBaseCopy},
		{"2026-10-07 10:00:00.123 4242 ERROR  pgsql.c:1  FATAL:  password authentication failed for user \"doadmin\"", engine.FailPermanent},
		{"2026-10-07 10:00:00.123 4242 ERROR  pgsql.c:1  could not connect to server: Connection refused", engine.FailTransient},
		{"2026-10-07 10:00:00.123 4242 INFO   follow.c:1  Follow mode is now done, reached endpos 0/5A3B2C10", engine.FailEndposReached},
		{"2026-10-07 10:00:00.123 4242 INFO   copy.c:1  COPY \"public\".\"orders\" done", engine.FailNone},
	}
	for _, c := range cases {
		if got := e.Classify(e.ParseLog(c.line)); got != c.want {
			t.Errorf("%q: %s, want %s", c.line, got, c.want)
		}
	}
	r := e.ParseLog(`2026-10-07 10:00:00.123 4242 INFO   copy.c:1  COPY "public"."orders" done`)
	if r.Table != "public.orders" || r.Level != "info" || r.PID != 4242 {
		t.Fatalf("parse: %+v", r)
	}
}

func TestPlanRoleStatements(t *testing.T) {
	dump := `--
-- PostgreSQL database cluster dump
--
\restrict abc123
SET default_transaction_read_only = off;
CREATE ROLE app_rw;
ALTER ROLE app_rw WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB LOGIN NOREPLICATION NOBYPASSRLS;
CREATE ROLE reporting;
ALTER ROLE reporting WITH SUPERUSER INHERIT NOCREATEROLE NOCREATEDB NOLOGIN REPLICATION BYPASSRLS;
CREATE ROLE doadmin;
ALTER ROLE doadmin WITH NOSUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION NOBYPASSRLS;
CREATE ROLE postgres;
ALTER ROLE postgres WITH SUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION BYPASSRLS;
CREATE ROLE "Mixed Case";
ALTER ROLE "Mixed Case" WITH LOGIN;
CREATE ROLE already_there;
ALTER ROLE already_there WITH LOGIN;
ALTER ROLE app_rw SET statement_timeout TO '30s';
GRANT reporting TO app_rw WITH INHERIT TRUE GRANTED BY doadmin;
GRANT pg_monitor TO doadmin WITH INHERIT TRUE GRANTED BY postgres;
GRANT pg_read_all_data TO reporting GRANTED BY postgres;
\unrestrict abc123
`
	p := PlanRoleStatements(dump, map[string]bool{"already_there": true, "doadmin": true}, "doadmin")
	got := strings.Join(p.Statements, "\n")
	for _, want := range []string{
		"CREATE ROLE app_rw;",
		"ALTER ROLE reporting WITH NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB NOLOGIN NOREPLICATION NOBYPASSRLS;",
		`CREATE ROLE "Mixed Case";`,
		"ALTER ROLE app_rw SET statement_timeout TO '30s';",
		"GRANT reporting TO app_rw WITH INHERIT TRUE;",
		"GRANT pg_read_all_data TO reporting;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"doadmin WITH", "ROLE postgres", "already_there", "restrict", "SET default", "GRANTED BY", " SUPERUSER", " REPLICATION", " BYPASSRLS"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, got)
		}
	}
	if p.Existing != 1 || strings.Join(p.New, ",") != "app_rw,Mixed Case" {
		t.Errorf("existing %d new %v", p.Existing, p.New)
	}
}
