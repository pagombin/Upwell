// Package runner starts and stops engine units. Production uses instances of
// the systemd template upwell-eng@.service (PC1). The local runner mirrors it
// for development and tests: detached process, own cgroup v2 group when
// available, SIGTERM then SIGKILL of the whole group after 10 s (PC2).
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Spec is what an engine unit runs.
type Spec struct {
	Instance string            // e.g. m42-orders
	Binary   string            // engine binary (fixed by the template in production)
	Args     []string          // engine arguments; no element contains whitespace
	Env      map[string]string // e.g. PGPASSFILE
	RunDir   string            // per-run directory; engine.log and engine.env live here
	LogFile  string
}

// Status is a unit's state.
type Status struct {
	Active   bool  `json:"active"`
	MainPID  int   `json:"main_pid"`
	MainLive bool  `json:"main_live"`
	Procs    []int `json:"procs"`
	ExitCode *int  `json:"exit_code,omitempty"`
	Known    bool  `json:"known"`
}

// Runner manages units.
type Runner interface {
	Name() string
	Start(ctx context.Context, s Spec) error
	Stop(ctx context.Context, instance string, timeout time.Duration) error
	Status(ctx context.Context, instance string) (Status, error)
	Forget(ctx context.Context, instance string) error
	UnitName(instance string) string
}

// StopTimeout is the SIGTERM-to-SIGKILL delay (PC2).
const StopTimeout = 10 * time.Second

// New picks a runner.
func New(kind, stateDir string) (Runner, error) {
	if kind == "auto" {
		if b, err := os.ReadFile("/proc/1/comm"); err == nil && strings.TrimSpace(string(b)) == "systemd" {
			kind = "systemd"
		} else {
			kind = "local"
		}
	}
	switch kind {
	case "systemd":
		return &Systemd{}, nil
	case "local":
		return NewLocal(stateDir)
	}
	return nil, fmt.Errorf("unknown runner %q", kind)
}

// WriteEnvFile writes engine.env as the template reads it.
func WriteEnvFile(s Spec) (string, error) {
	for _, a := range s.Args {
		if strings.ContainsAny(a, " \t\n") {
			return "", fmt.Errorf("engine argument %q contains whitespace, which the unit template cannot pass", a)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "UPWELL_ENGINE_ARGS=%s\n", strings.Join(s.Args, " "))
	for k, v := range s.Env {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	p := filepath.Join(s.RunDir, "engine.env")
	return p, os.WriteFile(p, []byte(b.String()), 0o600)
}

// Systemd runs instances of upwell-eng@.service.
type Systemd struct{}

func (*Systemd) Name() string                { return "systemd" }
func (*Systemd) UnitName(inst string) string { return "upwell-eng@" + inst + ".service" }
func systemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Start writes engine.env and starts the instance. The template fixes the
// binary, user and kill settings.
func (s *Systemd) Start(ctx context.Context, sp Spec) error {
	if _, err := WriteEnvFile(sp); err != nil {
		return err
	}
	_, _ = systemctl(ctx, "reset-failed", s.UnitName(sp.Instance))
	_, err := systemctl(ctx, "start", "--no-block", s.UnitName(sp.Instance))
	return err
}

// Stop stops the instance; the template sends SIGTERM and SIGKILL after 10 s.
func (s *Systemd) Stop(ctx context.Context, inst string, _ time.Duration) error {
	_, err := systemctl(ctx, "stop", s.UnitName(inst))
	return err
}

// Status reads ActiveState, MainPID, ExecMainStatus and the cgroup's processes.
func (s *Systemd) Status(ctx context.Context, inst string) (Status, error) {
	out, err := systemctl(ctx, "show", "-p", "ActiveState,SubState,MainPID,ExecMainStatus,ExecMainCode,ControlGroup,LoadState", s.UnitName(inst))
	if err != nil {
		return Status{}, err
	}
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	st := Status{Known: kv["LoadState"] == "loaded"}
	st.Active = kv["ActiveState"] == "active" || kv["ActiveState"] == "activating" || kv["ActiveState"] == "deactivating"
	st.MainPID, _ = strconv.Atoi(kv["MainPID"])
	st.MainLive = st.MainPID > 0 && alive(st.MainPID)
	if cg := kv["ControlGroup"]; cg != "" {
		st.Procs = readProcs(filepath.Join("/sys/fs/cgroup", cg, "cgroup.procs"))
	}
	if !st.Active && kv["ExecMainCode"] != "" && kv["ExecMainCode"] != "0" {
		if c, err := strconv.Atoi(kv["ExecMainStatus"]); err == nil {
			st.ExitCode = &c
		}
	}
	return st, nil
}

// Forget resets a failed instance.
func (s *Systemd) Forget(ctx context.Context, inst string) error {
	_, _ = systemctl(ctx, "reset-failed", s.UnitName(inst))
	return nil
}

func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// state is the field after the parenthesised command name
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return false
	}
	st := s[i+2]
	return st != 'Z' && st != 'X'
}

func readProcs(path string) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if p, err := strconv.Atoi(f); err == nil && alive(p) {
			out = append(out, p)
		}
	}
	return out
}

// Local runs engines as detached process groups, in a cgroup v2 group when
// one can be created.
type Local struct {
	stateDir string
	cgRoot   string
}

// NewLocal prepares the state directory and finds a writable cgroup v2 root.
func NewLocal(stateDir string) (*Local, error) {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, err
	}
	l := &Local{stateDir: stateDir}
	for _, root := range []string{"/sys/fs/cgroup/unified", "/sys/fs/cgroup"} {
		if _, err := os.Stat(filepath.Join(root, "cgroup.procs")); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
			continue
		}
		dir := filepath.Join(root, "upwell-local")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			l.cgRoot = dir
			break
		}
	}
	return l, nil
}

func (*Local) Name() string                  { return "local" }
func (*Local) UnitName(inst string) string   { return "upwell-eng@" + inst + ".service" }
func (l *Local) pidFile(inst string) string  { return filepath.Join(l.stateDir, inst+".pid") }
func (l *Local) exitFile(inst string) string { return filepath.Join(l.stateDir, inst+".exit") }
func (l *Local) cg(inst string) string {
	if l.cgRoot == "" {
		return ""
	}
	return filepath.Join(l.cgRoot, inst)
}

// Start launches the engine detached so it outlives the app.
func (l *Local) Start(ctx context.Context, sp Spec) error {
	if st, _ := l.Status(ctx, sp.Instance); st.Active {
		return fmt.Errorf("unit %s is already running", l.UnitName(sp.Instance))
	}
	if _, err := WriteEnvFile(sp); err != nil {
		return err
	}
	logf, err := os.OpenFile(sp.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(sp.Binary, sp.Args...)
	cmd.Dir = sp.RunDir
	cmd.Stdout, cmd.Stderr = logf, logf
	env := os.Environ()
	for k, v := range sp.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	attr := &syscall.SysProcAttr{Setsid: true}
	var cgfd *os.File
	if cg := l.cg(sp.Instance); cg != "" {
		os.Remove(cg)
		if err := os.Mkdir(cg, 0o755); err == nil || os.IsExist(err) {
			if f, err := os.Open(cg); err == nil {
				cgfd = f
				attr.UseCgroupFD = true
				attr.CgroupFD = int(f.Fd())
			}
		}
	}
	cmd.SysProcAttr = attr
	os.Remove(l.exitFile(sp.Instance))
	if err := cmd.Start(); err != nil {
		if cgfd != nil {
			cgfd.Close()
		}
		return fmt.Errorf("starting %s: %w", sp.Binary, err)
	}
	if cgfd != nil {
		cgfd.Close()
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(l.pidFile(sp.Instance), []byte(strconv.Itoa(pid)), 0o640); err != nil {
		return err
	}
	// Reap the main process while the app runs, recording its exit code. If
	// the app restarts first, the engine keeps running unparented and its
	// end is inferred from the log.
	go func(inst string) {
		err := cmd.Wait()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					code = 128 + int(ws.Signal())
				} else {
					code = ee.ExitCode()
				}
			} else {
				code = -1
			}
		}
		_ = os.WriteFile(l.exitFile(inst), []byte(strconv.Itoa(code)), 0o640)
	}(sp.Instance)
	return nil
}

func (l *Local) mainPID(inst string) int {
	b, err := os.ReadFile(l.pidFile(inst))
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return p
}

func (l *Local) procs(inst string) []int {
	if cg := l.cg(inst); cg != "" {
		if _, err := os.Stat(cg); err == nil {
			return readProcs(filepath.Join(cg, "cgroup.procs"))
		}
	}
	pid := l.mainPID(inst)
	if pid == 0 {
		return nil
	}
	return groupProcs(pid)
}

// groupProcs lists live processes whose process group is pgid.
func groupProcs(pgid int) []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		g, err := syscall.Getpgid(p)
		if err == nil && g == pgid && alive(p) {
			out = append(out, p)
		}
	}
	return out
}

// Status reports whether any process of the unit is alive.
func (l *Local) Status(_ context.Context, inst string) (Status, error) {
	st := Status{MainPID: l.mainPID(inst)}
	st.Known = st.MainPID != 0
	st.MainLive = st.MainPID > 0 && alive(st.MainPID)
	st.Procs = l.procs(inst)
	st.Active = len(st.Procs) > 0 || st.MainLive
	if !st.Active {
		if b, err := os.ReadFile(l.exitFile(inst)); err == nil {
			if c, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				st.ExitCode = &c
			}
		}
	}
	return st, nil
}

func (l *Local) signal(inst string, sig syscall.Signal) {
	if cg := l.cg(inst); cg != "" && sig == syscall.SIGKILL {
		if os.WriteFile(filepath.Join(cg, "cgroup.kill"), []byte("1"), 0o644) == nil {
			return
		}
	}
	for _, p := range l.procs(inst) {
		_ = syscall.Kill(p, sig)
	}
	if pid := l.mainPID(inst); pid > 0 {
		_ = syscall.Kill(-pid, sig)
	}
}

// Stop sends SIGTERM to every process, then SIGKILL to the group after timeout.
func (l *Local) Stop(ctx context.Context, inst string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = StopTimeout
	}
	st, _ := l.Status(ctx, inst)
	if !st.Active {
		return nil
	}
	l.signal(inst, syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, _ := l.Status(ctx, inst); !st.Active {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	l.signal(inst, syscall.SIGKILL)
	for i := 0; i < 50; i++ {
		if st, _ := l.Status(ctx, inst); !st.Active {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("unit %s still has processes after SIGKILL", l.UnitName(inst))
}

// Forget removes the unit's state.
func (l *Local) Forget(_ context.Context, inst string) error {
	os.Remove(l.pidFile(inst))
	os.Remove(l.exitFile(inst))
	if cg := l.cg(inst); cg != "" {
		os.Remove(cg)
	}
	return nil
}
