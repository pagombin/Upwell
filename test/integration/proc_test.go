//go:build integration

package integration

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// upwellProc runs Upwell as its own process, so a test can kill it with
// SIGKILL and start it again: every goroutine dies, as in a real restart,
// and the engines (each in its own process group) keep running, as under
// systemd.
type upwellProc struct {
	cmd  *exec.Cmd
	cfg  string
	log  string
	port int
}

var buildOnce sync.Once
var upwellBin string
var buildErr error

func buildUpwell(t *testing.T) string {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "upwell-bin-")
		if err != nil {
			buildErr = err
			return
		}
		upwellBin = filepath.Join(dir, "upwell")
		out, err := exec.Command("go", "build", "-o", upwellBin, "../../cmd/upwell").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return upwellBin
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// newProcEnv prepares a store as newEnv does (users, fast settings), closes
// the in-process server, and serves the same data directory from an upwell
// process.
func newProcEnv(t *testing.T) *Env {
	buildUpwell(t)
	e := newEnv(t, "", orch.TestHooks{})
	e.Close()
	p := &upwellProc{port: freePort(t), log: filepath.Join(e.Dir, "serve.log"), cfg: filepath.Join(e.Dir, "config.yaml")}
	pgcopydb := envStr("UPWELL_IT_PGCOPYDB", "pgcopydb")
	if lp, err := exec.LookPath(pgcopydb); err == nil {
		pgcopydb = lp
	}
	cfg := fmt.Sprintf(`listen_address: 127.0.0.1
https_port: %d
http_redirect: false
data_dir: %s
log_dir: %s
tls_cert: %s
tls_key: %s
master_key: %s
setup_token_file: %s
dev: true
engine:
  runner: local
  pgcopydb: %s
  pg_bin_dir: /usr/lib/postgresql/16/bin
`, p.port, e.Cfg.DataDir, e.Cfg.LogDir, filepath.Join(e.Dir, "tls", "cert.pem"), filepath.Join(e.Dir, "tls", "key.pem"), e.Cfg.MasterKey, e.Cfg.SetupToken, pgcopydb)
	if err := os.WriteFile(p.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	e.proc = p
	e.Base = fmt.Sprintf("https://127.0.0.1:%d", p.port)
	e.Srv = nil
	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 120 * time.Second}
	e.startProc()
	t.Cleanup(func() { e.killProc() })
	return e
}

func (e *Env) startProc() {
	e.t.Helper()
	p := e.proc
	lf, err := os.OpenFile(p.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	p.cmd = exec.Command(upwellBin, "serve", "--config", p.cfg)
	p.cmd.Stdout, p.cmd.Stderr = lf, lf
	p.cmd.Env = append(os.Environ(), "UPWELL_LOG_STDERR=1")
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := p.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	go p.cmd.Wait()
	e.waitFor(30*time.Second, "upwell to answer", func() bool {
		r, err := e.client.Get(e.Base + "/readyz")
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	e.login("admin")
}

// killProc ends the upwell process with SIGKILL (its engines keep running).
func (e *Env) killProc() {
	if e.proc == nil || e.proc.cmd == nil || e.proc.cmd.Process == nil {
		return
	}
	e.proc.cmd.Process.Signal(syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	e.proc.cmd = nil
}

// restartProc kills upwell and starts it again on the same data directory.
func (e *Env) restartProc() {
	e.killProc()
	e.startProc()
}
