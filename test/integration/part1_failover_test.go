//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/orch"
)

// 9. PgBouncer (session mode) in front of the target drops every connection
// while changes stream, and comes back 10 s later: the apply resumes from
// the target's origin, and the cutover ends GO with identical data.
func TestP1PoolerDrop(t *testing.T) {
	for i, p := range plugins {
		p, port := p, 56550+i
		t.Run(p, func(t *testing.T) {
			_, b := startPgBouncerTo(t, "session", port, dstPort)
			runScenario(t, p, scenario{name: "pooldrop", wantGO: true, dstPort: port,
				schema: `CREATE TABLE orders(id bigserial PRIMARY KEY, customer int NOT NULL, amount numeric(12,2), note text);
					INSERT INTO orders(customer, amount, note) SELECT g % 100, g, md5(g::text) FROM generate_series(1, 5000) g;`,
				streaming: func(t *testing.T, e *Env, id, db string) func() {
					stop := background(t, db, 20*time.Millisecond, func(ctx context.Context, c *pgx.Conn, i int) error {
						_, err := c.Exec(ctx, `INSERT INTO orders(customer, amount, note) VALUES ($1, $1, 'during the drop'); UPDATE orders SET amount = amount + 1 WHERE id = $1`, i%5000+1)
						return err
					})
					time.Sleep(5 * time.Second)
					b.kill()
					t.Log("PgBouncer killed")
					time.Sleep(10 * time.Second)
					b.start()
					t.Log("PgBouncer back")
					time.Sleep(5 * time.Second)
					stop()
					e.waitDBState(id, db, 10*time.Minute, orch.DInSync)
					return nil
				}})
		})
	}
}

// failoverTarget is a dedicated target cluster with a physical standby,
// built for one test and removed after it.
type failoverTarget struct {
	root           string
	primary, stdby int
}

func runPG(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("runuser", append([]string{"-u", "postgres", "--"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func newFailoverTarget(t *testing.T, primary, standby int) *failoverTarget {
	t.Helper()
	bin := "/usr/lib/postgresql/16/bin"
	root, err := os.MkdirTemp("/var/tmp", "upwell-failover-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(root, 0o755)
	exec.Command("chown", "postgres", root).Run()
	f := &failoverTarget{root: root, primary: primary, stdby: standby}
	t.Cleanup(func() {
		for _, d := range []string{"target", "standby"} {
			exec.Command("runuser", "-u", "postgres", "--", bin+"/pg_ctl", "-D", filepath.Join(root, d), "-m", "immediate", "stop").Run()
		}
		os.RemoveAll(root)
	})
	cmd := exec.Command("bash", "../../spikes/env/local-clusters.sh", "up")
	cmd.Env = append(os.Environ(), "TARGET_ONLY=1", "UPWELL_SPIKE_ROOT="+root, fmt.Sprintf("DST_PORT=%d", primary))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("primary: %v\n%s", err, out)
	}
	// Let the standby's base backup and stream connect as postgres locally.
	hba := filepath.Join(root, "target", "pg_hba.conf")
	b, _ := os.ReadFile(hba)
	os.WriteFile(hba, append([]byte("local replication postgres trust\n"), b...), 0o600)
	runPG(t, bin+"/pg_ctl", "-D", filepath.Join(root, "target"), "reload")
	sb := filepath.Join(root, "standby")
	runPG(t, bin+"/pg_basebackup", "-h", root, "-p", fmt.Sprint(primary), "-U", "postgres", "-D", sb, "-R", "-X", "stream")
	conf, _ := os.ReadFile(filepath.Join(sb, "postgresql.conf"))
	conf = append(conf, []byte(fmt.Sprintf("\nport = %d\n", standby))...)
	os.WriteFile(filepath.Join(sb, "postgresql.conf"), conf, 0o600)
	runPG(t, bin+"/pg_ctl", "-D", sb, "-l", filepath.Join(root, "standby.log"), "-w", "start")
	return f
}

// failover stops the primary at once (no shutdown checkpoint, as a crash)
// and promotes the standby.
func (f *failoverTarget) failover(t *testing.T) {
	bin := "/usr/lib/postgresql/16/bin"
	runPG(t, bin+"/pg_ctl", "-D", filepath.Join(f.root, "target"), "-m", "immediate", "stop")
	runPG(t, bin+"/pg_ctl", "-D", filepath.Join(f.root, "standby"), "-w", "promote")
}

// 5. Target failover: the target's primary dies while changes stream and its
// asynchronous standby is promoted behind the same address (PgBouncer here,
// a DNS name on a managed cluster). The replication origin's progress is
// replayed on the standby with the data it describes, so the apply resumes
// from what the new primary actually has: GO with identical data, nothing
// applied twice or skipped.
func TestP1TargetFailover(t *testing.T) {
	for i, p := range plugins {
		p := p
		primary, standby, port := 55450+2*i, 55451+2*i, 56560+i
		t.Run(p, func(t *testing.T) {
			f := newFailoverTarget(t, primary, standby)
			_, b := startPgBouncerTo(t, "session", port, primary)
			runScenario(t, p, scenario{name: "tfailover", wantGO: true, dstPort: port,
				schema: `CREATE TABLE orders(id bigserial PRIMARY KEY, customer int NOT NULL, amount numeric(12,2), note text);
					INSERT INTO orders(customer, amount, note) SELECT g % 100, g, md5(g::text) FROM generate_series(1, 5000) g;`,
				streaming: func(t *testing.T, e *Env, id, db string) func() {
					stop := background(t, db, 10*time.Millisecond, func(ctx context.Context, c *pgx.Conn, i int) error {
						_, err := c.Exec(ctx, `INSERT INTO orders(customer, amount, note) VALUES ($1, $1, 'around the failover'); UPDATE orders SET amount = amount + 1 WHERE id = $1`, i%5000+1)
						return err
					})
					time.Sleep(8 * time.Second)
					f.failover(t)
					b.kill()
					b.backend = f.stdby
					b.start()
					t.Log("target failed over to the standby")
					time.Sleep(10 * time.Second)
					stop()
					e.waitDBState(id, db, 10*time.Minute, orch.DInSync)
					return nil
				}})
		})
	}
}
