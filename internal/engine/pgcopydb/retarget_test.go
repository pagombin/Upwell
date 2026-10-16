package pgcopydb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
)

// TestRetarget (D100): pgcopydb refuses to resume when the recorded bare
// URIs differ from the current ones; Retarget rewrites the user, host and
// port and leaves the rest of pgcopydb's form alone.
func TestRetarget(t *testing.T) {
	run := t.TempDir()
	d := engine.DatabaseSpec{Source: "shop", Target: "shop", Instance: "m1-shop", RunDir: run,
		SourceConn: pg.Conn{Host: "private.example", Port: 25060, User: "doadmin", Password: "x"},
		TargetConn: pg.Conn{Host: "10.0.0.5", Port: 6432, User: "upwell", Password: "y"}}
	e := New("pgcopydb", nil)
	// No catalog yet: nothing to do.
	if changed, err := e.Retarget(context.Background(), d); err != nil || changed {
		t.Fatalf("no catalog: %v %v", changed, err)
	}
	dir := filepath.Join(run, "clone", "schema")
	os.MkdirAll(dir, 0o755)
	db, err := sql.Open("sqlite", filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table setup(id integer primary key check (id = 1), source_pg_uri text, target_pg_uri text, snapshot text, split_tables_larger_than integer, split_max_parts integer, filters text);
		insert into setup(id, source_pg_uri, target_pg_uri, snapshot) values (1, 'postgres://doadmin@public.example:25060/shop', 'postgres://doadmin@127.0.0.1:55433/shop', '00000003-1')`); err != nil {
		t.Fatal(err)
	}
	changed, err := e.Retarget(context.Background(), d)
	if err != nil || !changed {
		t.Fatalf("retarget: %v %v", changed, err)
	}
	var s, tg, snap string
	db.QueryRow(`select source_pg_uri, target_pg_uri, snapshot from setup`).Scan(&s, &tg, &snap)
	if s != "postgres://doadmin@private.example:25060/shop" || tg != "postgres://upwell@10.0.0.5:6432/shop" || snap != "00000003-1" {
		t.Fatalf("after: %s %s %s", s, tg, snap)
	}
	if changed, err := e.Retarget(context.Background(), d); err != nil || changed {
		t.Fatalf("second run: %v %v", changed, err)
	}
}
