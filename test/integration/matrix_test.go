//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// TestVersionMatrix (bug bash 11): targets on PostgreSQL 14, 15, 16 and 18.
// 14 gets the "before PostgreSQL 15" blocker (no parameter grants exist, so
// a non-superuser cannot set session_replication_role); 15 to 18 reach GO.
// A source may not be newer than its target, so 14 and 15 are fed from a
// PostgreSQL 14 source.
func TestVersionMatrix(t *testing.T) {
	cases := []struct {
		name             string
		srcPort, dstPort int
		wantGO           bool
	}{
		{"pg14", 55437, 55436, false},
		{"pg15", 55437, 55435, true},
		{"pg16", srcPort, dstPort, true},
		{"pg18", srcPort, 55434, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := pg.Conn{Host: "127.0.0.1", Port: c.srcPort, User: "doadmin", Password: password, DBName: "defaultdb", SSLMode: "disable"}
			dst := pg.Conn{Host: "127.0.0.1", Port: c.dstPort, User: "doadmin", Password: password, DBName: "defaultdb", SSLMode: "disable"}
			if _, err := pg.Connect(context.Background(), dst, 5*time.Second); err != nil {
				t.Skipf("no %s target on port %d (spikes/env/local-clusters.sh): %v", c.name, c.dstPort, err)
			}
			t.Logf("source %s, target %s", q(t, src, "defaultdb", "SELECT current_setting('server_version')"), q(t, dst, "defaultdb", "SELECT current_setting('server_version')"))
			db := uniq("it_ver_" + c.name)
			exec1(t, src, "defaultdb", "CREATE DATABASE "+db)
			t.Cleanup(func() {
				for _, cn := range []pg.Conn{src, dst} {
					exec1(t, cn, "defaultdb", "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
				}
			})
			exec1(t, src, db, fmt.Sprintf(`CREATE TABLE orders(id bigserial PRIMARY KEY, customer int NOT NULL, amount numeric(12,2), note text);
				INSERT INTO orders(customer, amount, note) SELECT g%%100, g/10.0, md5(g::text) FROM generate_series(1, %d) g;`, 2000))
			e := newEnv(t, "", orch.TestHooks{})
			id := e.defineMigration("Version "+c.name, []string{db}, map[string]any{"_source_port": c.srcPort, "_target_port": c.dstPort})
			pv := e.preflight(id)
			var apply string
			var hard []string
			for _, r := range pv.Results {
				if r.CheckID == "target_apply_privileges" {
					apply = r.Level + ": " + r.Message
				}
				if r.Level == "blocker" && r.Hard {
					hard = append(hard, r.CheckID)
				}
				if r.Level == "blocker" && !r.Hard {
					t.Logf("acceptable blocker %s: %s", r.CheckID, r.Message)
				}
			}
			t.Logf("target_apply_privileges %s; hard blockers %v", apply, hard)
			if !c.wantGO {
				if !strings.HasPrefix(apply, "blocker") || !strings.Contains(apply, "before PostgreSQL 15") {
					t.Fatalf("no clear 'before PostgreSQL 15' blocker: %s", apply)
				}
				if code := e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil); code != 409 {
					t.Fatalf("a PostgreSQL 14 target started: %d", code)
				}
				return
			}
			if len(hard) > 0 {
				t.Fatalf("hard blockers on %s: %v", c.name, hard)
			}
			e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
			e.waitDBState(id, db, 5*time.Minute, orch.DInSync)
			exec1(t, src, db, `INSERT INTO orders(customer, amount, note) VALUES (1, 1, 'after the copy'); UPDATE orders SET amount = amount + 1 WHERE id <= 10`)
			time.Sleep(3 * time.Second)
			e.mustGO(id)
			for _, qy := range []string{"SELECT count(*)::text FROM orders", "SELECT sum(amount)::text FROM orders"} {
				if s, d := q(t, src, db, qy), q(t, dst, db, qy); s != d {
					t.Fatalf("%s: source %s, target %s", qy, s, d)
				}
			}
			e.cleanup(id, "Version "+c.name)
		})
	}
}
