//go:build integration

package integration

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

var keepaliveTimer = regexp.MustCompile(`timer:\(keepalive,([0-9.]+)(ms|sec|min)`)

// TestEngineConnectionsKeepAlive (D101): every connection the engine holds to
// either cluster probes an idle peer every 10 s. pgcopydb documents keepalive
// defaults but applies them only to the URI it logs, not to the one it
// connects with: on the droplet, CREATE INDEX workers sat 9 m 40 s without a
// packet (the kernel's 2-hour keepalive), and a stateful device on the path
// forgot both connections. Their CREATE INDEX was never answered and the
// base copy of 240 GB failed 600 s later.
func TestEngineConnectionsKeepAlive(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_keepalive")
	seedDB(t, db, 2000)
	id := e.defineMigration("Keepalive", []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	time.Sleep(15 * time.Second) // let the streaming connections go idle
	out, err := exec.Command("ss", "-tinopH", "state", "established").CombinedOutput()
	if err != nil {
		t.Fatalf("ss: %v %s", err, out)
	}
	// ss prints the socket line (with the process) and the timer on it.
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if !strings.Contains(l, `(("pgcopydb`) || !(strings.Contains(l, ":"+strconv.Itoa(srcPort)+" ") || strings.Contains(l, ":"+strconv.Itoa(dstPort)+" ")) {
			continue
		}
		m := keepaliveTimer.FindStringSubmatch(l)
		if m == nil {
			continue // data in flight: a retransmission timer, not keepalive
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		sec := map[string]float64{"ms": v / 1000, "sec": v, "min": v * 60}[m[2]]
		n++
		if sec > 10.5 {
			t.Errorf("an engine connection waits %s%s before its first keepalive: %s", m[1], m[2], strings.Join(strings.Fields(l), " "))
		}
	}
	if n == 0 {
		t.Fatalf("no idle engine connection found:\n%s", out)
	}
	t.Logf("%d idle engine connection(s) checked", n)
	e.abortAndClean(id, "Keepalive")
}
