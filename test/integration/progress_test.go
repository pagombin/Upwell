//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/pgpack"
)

// slowText feeds a text-format COPY FROM STDIN (as pgcopydb does) one row at
// a time with a pause, as the engine's COPY looks to the target while a large
// table streams.
type slowText struct {
	n, max int
	pause  time.Duration
}

func (s *slowText) Read(p []byte) (int, error) {
	if s.n >= s.max {
		return 0, io.EOF
	}
	if s.n > 100 {
		time.Sleep(s.pause)
	}
	s.n++
	return copy(p, fmt.Sprintf("%d\t%s\n", s.n, strings.Repeat("x", 200))), nil
}

// The droplet (2026-10-12): every bar in "Base copy in progress" stayed
// empty. pgcopydb copies with COPY FROM STDIN, for which the target's
// pg_stat_progress_copy reports bytes_total = 0, and the bar was
// bytes / bytes_total. The fraction now comes from the source table's
// estimated row count.
func TestCopyProgressHasAFraction(t *testing.T) {
	db := uniq("it_progress")
	exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+db)
	exec1(t, dstConn, "defaultdb", "CREATE DATABASE "+db)
	defer dropDB(t, db)
	exec1(t, srcConn, db, `CREATE TABLE big(id bigint PRIMARY KEY, v text); INSERT INTO big SELECT g, 'row' FROM generate_series(1,20000) g; ANALYZE big`)
	exec1(t, dstConn, db, `CREATE TABLE big(id bigint PRIMARY KEY, v text)`)

	conn, err := pg.Connect(context.Background(), dstConn.WithDB(db), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done; conn.Close(context.Background()) }()
	go func() {
		defer close(done)
		_, _ = conn.PgConn().CopyFrom(ctx, &slowText{max: 20000, pause: 5 * time.Millisecond}, "COPY big(id, v) FROM STDIN")
	}()

	var got map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		p := pgpack.CopyProgress(context.Background(), srcConn.WithDB(db), dstConn.WithDB(db))
		if cs, _ := p["copies"].([]map[string]any); len(cs) > 0 {
			if n, _ := cs[0]["tuples"].(int64); n > 0 {
				got = cs[0]
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("no COPY seen in pg_stat_progress_copy")
	}
	t.Logf("progress row: %v", got)
	if bt, _ := got["bytes_total"].(int64); bt != 0 {
		t.Logf("bytes_total is %d (expected 0 for COPY FROM STDIN)", bt)
	}
	est, _ := got["rows_estimate"].(int64)
	if est < 19000 || est > 21000 {
		t.Fatalf("rows_estimate %d, want about 20000 from the source's statistics", est)
	}
	f, _ := got["fraction"].(float64)
	if f <= 0 || f >= 1 {
		t.Fatalf("fraction %v, want between 0 and 1", f)
	}
	fmt.Printf("fraction %.3f\n", f)
}

// D78: a table copied in parts (two COPYs into it at once) is one row, with
// its fraction from the target's table size against the source's.
func TestCopyProgressSplitTable(t *testing.T) {
	db := uniq("it_progsplit")
	exec1(t, srcConn, "defaultdb", "CREATE DATABASE "+db)
	exec1(t, dstConn, "defaultdb", "CREATE DATABASE "+db)
	defer dropDB(t, db)
	exec1(t, srcConn, db, `CREATE TABLE big(id bigint PRIMARY KEY, v text); INSERT INTO big SELECT g, repeat('x', 200) FROM generate_series(1,40000) g; ANALYZE big`)
	exec1(t, dstConn, db, `CREATE TABLE big(id bigint, v text)`)
	ctx, cancel := context.WithCancel(context.Background())
	var dones []chan struct{}
	defer func() {
		cancel()
		for _, d := range dones {
			<-d
		}
	}()
	for i := 0; i < 2; i++ {
		conn, err := pg.Connect(context.Background(), dstConn.WithDB(db), 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		dones = append(dones, done)
		go func() {
			defer close(done)
			defer conn.Close(context.Background())
			_, _ = conn.PgConn().CopyFrom(ctx, &slowText{max: 20000, pause: 5 * time.Millisecond}, "COPY big(id, v) FROM STDIN")
		}()
	}
	var got map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && got == nil {
		p := pgpack.CopyProgress(context.Background(), srcConn.WithDB(db), dstConn.WithDB(db))
		if cs, _ := p["copies"].([]map[string]any); len(cs) == 1 {
			if n, _ := cs[0]["parts"].(int64); n == 2 {
				if f, _ := cs[0]["fraction"].(float64); f > 0 {
					got = cs[0]
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("no single row with 2 parts and a size-based fraction")
	}
	t.Logf("split progress row: %v", got)
	if f := got["fraction"].(float64); f >= 1 {
		t.Fatalf("fraction %v", f)
	}
}
