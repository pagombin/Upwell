//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// TestNetworkIncidentOnClosedConnection (D97): the droplet's base copies kept
// failing with "SSL error: unexpected eof while reading" from the target and
// nothing said what closed the connection. Here the capture unit's helper
// runs (as root, on the loopback addresses of the local target), the
// target's COPY session is ended by the server mid-copy, and Upwell must
// write an incident report naming the target's address as the side that
// closed first, with the kernel's socket samples, and put it in the bundle.
func TestNetworkIncidentOnClosedConnection(t *testing.T) {
	if _, err := exec.LookPath("tcpdump"); err != nil {
		t.Skip("tcpdump not installed")
	}
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss (iproute2) not installed")
	}
	e := newEnv(t, "", orch.TestHooks{})
	diag := filepath.Join(e.Cfg.DataDir, "diag")
	fw := t.TempDir()
	os.WriteFile(filepath.Join(fw, "clusters"), []byte("127.0.0.1 55433\n"), 0o644)
	helper := exec.Command("../../deploy/netcapture/upwell-netcapture")
	helper.Env = append(os.Environ(), "UPWELL_FIREWALL_DIR="+fw, "UPWELL_CAPTURE_DIR="+filepath.Join(diag, "capture"), "UPWELL_CAPTURE_USER=root")
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-helper.Process.Pid, syscall.SIGTERM); helper.Wait() })
	e.waitFor(30*time.Second, "the capture to run", func() bool {
		b, _ := os.ReadFile(filepath.Join(diag, "capture", "status"))
		return strings.Contains(string(b), "running")
	})

	db := uniq("it_netdiag")
	seedDB(t, db, 1500000)
	name := "Network incident"
	id := e.defineMigration(name, []string{db}, nil)
	e.preflight(id)
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, time.Minute, orch.DBaseCopy)
	e.waitFor(3*time.Minute, "a COPY on the target", func() bool {
		return q(t, dstConn, db, `SELECT count(*)::text FROM pg_stat_progress_copy`) != "0"
	})
	superSQL(t, dstPort, db, `SELECT pg_terminate_backend(pid) FROM pg_stat_progress_copy`)
	d := e.waitDBState(id, db, 3*time.Minute, orch.DRestartRequired, orch.DFailed)
	t.Logf("%s: %s", d.State, d.LastError)

	var msg string
	e.waitFor(3*time.Minute, "the network incident event", func() bool {
		var ev []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		e.do("GET", "/api/v1/migrations/"+id+"/events?limit=500", nil, &ev, 200)
		for _, x := range ev {
			if x.Type == "network_incident" {
				msg = x.Message
				return true
			}
		}
		return false
	})
	t.Logf("event: %s", msg)
	if !strings.Contains(msg, "The target's address closed") {
		t.Fatalf("the verdict does not name the target as the side that closed first: %s", msg)
	}
	files, _ := filepath.Glob(filepath.Join(diag, "incident-*.txt"))
	if len(files) == 0 {
		t.Fatal("no incident file")
	}
	rep, _ := os.ReadFile(files[0])
	for _, want := range []string{"Flags [F", "127.0.0.1:55433", "ESTAB", "== Verdict =="} {
		if !strings.Contains(string(rep), want) {
			t.Errorf("the report lacks %q", want)
		}
	}
	var prev struct {
		Files []struct {
			Name   string `json:"name"`
			Binary bool   `json:"binary"`
		} `json:"files"`
	}
	e.do("GET", "/api/v1/migrations/"+id+"/support-bundle", nil, &prev, 200)
	var inc, pcap, socks bool
	for _, f := range prev.Files {
		inc = inc || strings.HasPrefix(f.Name, "network/incident-")
		pcap = pcap || (strings.HasSuffix(f.Name, ".pcap") && f.Binary)
		socks = socks || f.Name == "network/sockets.jsonl"
	}
	if !inc || !pcap || !socks {
		t.Fatalf("bundle: incident %v, capture %v, sockets %v (%+v)", inc, pcap, socks, prev.Files)
	}
	e.abortAndClean(id, name)
}
