// Package pgcopydb implements the engine interface with pgcopydb.
package pgcopydb

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/runner"
)

// Engine wraps the pgcopydb binary.
type Engine struct {
	Binary string
	Runner runner.Runner
	mu     sync.Mutex
	ver    string
	help   string
}

// New returns the engine.
func New(binary string, r runner.Runner) *Engine {
	if binary == "" {
		binary = "pgcopydb"
	}
	return &Engine{Binary: binary, Runner: r}
}

func (e *Engine) Name() string { return "pgcopydb" }

// Version returns "0.18" style versions.
func (e *Engine) Version(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ver != "" {
		return e.ver, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, e.Binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("pgcopydb --version failed: %w", err)
	}
	m := regexp.MustCompile(`pgcopydb version (\S+)`).FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unexpected pgcopydb --version output")
	}
	e.ver = string(m[1])
	if h, err := exec.CommandContext(cctx, e.Binary, "clone", "--help").CombinedOutput(); err == nil {
		e.help = string(h)
	}
	return e.ver, nil
}

func (e *Engine) supports(flag string) bool { return strings.Contains(e.help, flag) }

// Capabilities of pgcopydb 0.18 as proven in Phase 0.
func (e *Engine) Capabilities() engine.Capabilities {
	return engine.Capabilities{Online: true, Offline: true, ResumeDuringCDC: true, ResumeDuringBaseCopy: false,
		GracefulStop: false, TableSplitting: true, DecodingPlugins: []string{"pgoutput", "test_decoding", "wal2json"},
		CopiesRoles: true, CopiesExtensions: true}
}

// Plan builds the exact command. It never uses --host/--port (PC12).
func (e *Engine) Plan(d engine.DatabaseSpec, resume bool) (engine.RunPlan, error) {
	if !pg.ValidDBName(d.Source) || !pg.ValidDBName(d.Target) {
		return engine.RunPlan{}, fmt.Errorf("database name %q or %q is not allowed", d.Source, d.Target)
	}
	work := filepath.Join(d.RunDir, "clone")
	args := []string{"clone", "--follow",
		"--source", d.SourceConn.WithDB(d.Source).URI(),
		"--target", d.TargetConn.WithDB(d.Target).URI(),
		"--dir", work,
		"--slot-name", d.SlotName,
		"--origin", d.OriginName,
		"--table-jobs", strconv.Itoa(max(d.TableJobs, 1)),
		"--index-jobs", strconv.Itoa(max(d.IndexJobs, 1)),
		"--skip-extensions", "--no-owner",
	}
	if d.SplitLarger != "" && d.SplitLarger != "off" && d.SplitLarger != "auto" {
		args = append(args, "--split-tables-larger-than", d.SplitLarger)
	}
	if e.supports("--skip-ext-comments") {
		args = append(args, "--skip-ext-comments")
	}
	if resume {
		// pgcopydb reads the plugin from its own catalog on resume: never pass --plugin.
		args = append(args, "--resume", "--not-consistent")
	} else if d.Plugin != "" {
		args = append(args, "--plugin", d.Plugin)
	}
	args = append(args, d.ExtraArgs...)
	for _, a := range args {
		if strings.Contains(a, "--host") || a == "--port" {
			return engine.RunPlan{}, fmt.Errorf("the pgcopydb coordinator is never used (PC12)")
		}
	}
	pass := filepath.Join(d.RunDir, "pgpass")
	env := map[string]string{"PGPASSFILE": pass, "PGAPPNAME": "upwell-engine"}
	if d.SyncCommitOff {
		env["PGOPTIONS"] = "-c synchronous_commit=off"
	}
	return engine.RunPlan{Instance: d.Instance, Unit: e.Runner.UnitName(d.Instance), Binary: e.Binary, Args: args, Env: env,
		RunDir: d.RunDir, WorkDir: work, LogFile: filepath.Join(d.RunDir, "engine.log"), PassFile: pass, Resume: resume,
		Display: e.Binary + " " + strings.Join(args, " ")}, nil
}

// Start writes the pgpass file and starts the unit.
func (e *Engine) Start(ctx context.Context, p engine.RunPlan, d engine.DatabaseSpec) error {
	if err := os.MkdirAll(p.RunDir, 0o750); err != nil {
		return err
	}
	if err := pg.WritePassFile(p.PassFile, d.SourceConn, d.TargetConn); err != nil {
		return err
	}
	return e.Runner.Start(ctx, runner.Spec{Instance: p.Instance, Binary: p.Binary, Args: p.Args, Env: p.Env, RunDir: p.RunDir, LogFile: p.LogFile})
}

func (e *Engine) run(ctx context.Context, timeout time.Duration, p engine.RunPlan, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.Binary, args...)
	cmd.Env = append(os.Environ(), "PGPASSFILE="+p.PassFile)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("pgcopydb %s: %v: %s", args[0]+" "+args[1], err, lastErrorLine(errb.String()))
	}
	return out.String(), nil
}

func lastErrorLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "ERROR") || strings.Contains(lines[i], "FATAL") {
			return strings.TrimSpace(lines[i])
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return ""
}

// Observe reads the sentinel with a direct read (--dir, plain text). It never
// talks to the coordinator.
func (e *Engine) Observe(ctx context.Context, p engine.RunPlan, d engine.DatabaseSpec) (engine.Progress, error) {
	pr := engine.Progress{Phase: engine.PhaseUnknown}
	if _, err := os.Stat(filepath.Join(p.WorkDir, "schema", "source.db")); err != nil {
		return pr, nil
	}
	out, err := e.run(ctx, 8*time.Second, p, "stream", "sentinel", "get", "--source", d.SourceConn.WithDB(d.Source).URI(), "--dir", p.WorkDir)
	if err != nil {
		return pr, err
	}
	return parseSentinel(out), nil
}

func parseSentinel(out string) engine.Progress {
	pr := engine.Progress{Phase: engine.PhaseBaseCopy}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "startpos":
			pr.StartPos = f[1]
		case "endpos":
			pr.EndPos = f[1]
		case "apply":
			pr.ApplyEnabled = f[1] == "enabled"
		case "write_lsn":
			pr.WriteLSN = f[1]
		case "flush_lsn":
			pr.FlushLSN = f[1]
		case "replay_lsn":
			pr.ReplayLSN = f[1]
		}
	}
	if pr.ApplyEnabled {
		pr.Phase = engine.PhaseCDC
	}
	return pr
}

// SetEndPosition sets endpos at the current source position (idempotent:
// an already-set end position is kept).
func (e *Engine) SetEndPosition(ctx context.Context, p engine.RunPlan, d engine.DatabaseSpec) (string, error) {
	if pr, err := e.Observe(ctx, p, d); err == nil && pr.EndPos != "" && pr.EndPos != "0/0" {
		return pr.EndPos, nil
	}
	out, err := e.run(ctx, 30*time.Second, p, "stream", "sentinel", "set", "endpos", "--current", "--source", d.SourceConn.WithDB(d.Source).URI(), "--dir", p.WorkDir)
	if err != nil {
		return "", err
	}
	lsn := strings.TrimSpace(out)
	if _, err := pg.ParseLSN(lsn); err != nil {
		pr, err2 := e.Observe(ctx, p, d)
		if err2 != nil || pr.EndPos == "" {
			return "", fmt.Errorf("end position was not reported (%q)", lsn)
		}
		lsn = pr.EndPos
	}
	return lsn, nil
}

// Nudge emits a transactional logical decoding message with no data change so
// an idle stream passes its end position (PC3).
func (e *Engine) Nudge(ctx context.Context, d engine.DatabaseSpec) error {
	c, err := pg.Connect(ctx, d.SourceConn.WithDB(d.Source), 15*time.Second)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	// A real transaction in Upwell's heartbeat table decodes with every plugin.
	// pgcopydb 0.18 cannot parse a logical decoding message under
	// test_decoding and its receive process fails on it (F14), so the message
	// is only a fallback for pgoutput when the heartbeat table is off.
	var hb *string
	_ = c.QueryRow(ctx, `SELECT to_regclass('upwell.heartbeat')::text`).Scan(&hb)
	if hb != nil {
		_, err = c.Exec(ctx, `INSERT INTO upwell.heartbeat(kind, token) VALUES ('nudge', 'drain')`)
		return err
	}
	if d.Plugin == "test_decoding" {
		return fmt.Errorf("no safe drain nudge for %s: the heartbeat table is off and pgcopydb cannot parse logical messages with test_decoding", d.Source)
	}
	_, err = c.Exec(ctx, `SELECT pg_logical_emit_message(true, 'upwell', 'drain nudge')`)
	return err
}

// Stop kills the unit's whole cgroup (SIGTERM, then SIGKILL after 10 s) and
// waits for the slot to be released, terminating a stale walsender after 15 s.
func (e *Engine) Stop(ctx context.Context, p engine.RunPlan, d engine.DatabaseSpec) (engine.StopResult, error) {
	start := time.Now()
	var res engine.StopResult
	if err := e.Runner.Stop(ctx, p.Instance, runner.StopTimeout); err != nil {
		return res, err
	}
	w0 := time.Now()
	released, killed, err := ReleaseSlot(ctx, d.SourceConn.WithDB(d.Source), d.SlotName, 15*time.Second, 60*time.Second)
	res.SlotReleased, res.WalsenderKilled = released, killed
	res.SlotReleaseWaited = time.Since(w0)
	res.Duration = time.Since(start)
	return res, err
}

// ReleaseSlot implements engine.Engine.
func (e *Engine) ReleaseSlot(ctx context.Context, d engine.DatabaseSpec) (bool, bool, error) {
	return ReleaseSlot(ctx, d.SourceConn.WithDB(d.Source), d.SlotName, 15*time.Second, 60*time.Second)
}

// ReleaseSlot waits for slot to be inactive, terminating its walsender after
// terminateAfter. A missing slot counts as released.
func ReleaseSlot(ctx context.Context, c pg.Conn, slot string, terminateAfter, giveUp time.Duration) (released, terminated bool, err error) {
	conn, err := pg.Connect(ctx, c, 10*time.Second)
	if err != nil {
		return false, false, err
	}
	defer conn.Close(ctx)
	start := time.Now()
	for {
		var active *bool
		var pid *int32
		err := conn.QueryRow(ctx, `SELECT active, active_pid FROM pg_replication_slots WHERE slot_name=$1`, slot).Scan(&active, &pid)
		if err != nil && strings.Contains(err.Error(), "no rows") {
			return true, terminated, nil
		}
		if err != nil {
			return false, terminated, err
		}
		if active == nil || !*active {
			return true, terminated, nil
		}
		if time.Since(start) > terminateAfter && pid != nil && !terminated {
			conn.Exec(ctx, `SELECT pg_terminate_backend($1)`, *pid)
			terminated = true
		}
		if time.Since(start) > giveUp {
			holder := "an unknown process"
			if pid != nil {
				holder = fmt.Sprintf("process %d", *pid)
			}
			return false, terminated, fmt.Errorf("replication slot %s is still held by %s after %s", slot, holder, giveUp)
		}
		select {
		case <-ctx.Done():
			return false, terminated, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// Cleanup removes everything the engine created: pgcopydb's own cleanup when a
// work directory exists, then direct drops of the slot and publication on the
// source and the origin, by name, on the target (PC6).
func (e *Engine) Cleanup(ctx context.Context, d engine.DatabaseSpec, opts engine.CleanupOptions) ([]string, error) {
	var done []string
	p, _ := e.Plan(d, false)
	if st, _ := e.Runner.Status(ctx, d.Instance); st.Active {
		if _, err := e.Stop(ctx, p, d); err != nil {
			return done, fmt.Errorf("stopping the engine before cleanup: %w", err)
		}
		done = append(done, "stopped engine unit")
	}
	if _, err := os.Stat(filepath.Join(p.WorkDir, "schema", "source.db")); err == nil {
		if err := pg.WritePassFile(p.PassFile, d.SourceConn, d.TargetConn); err != nil {
			return done, fmt.Errorf("writing the password file for cleanup: %w", err)
		}
		if _, err := e.run(ctx, 2*time.Minute, p, "stream", "cleanup", "--source", d.SourceConn.WithDB(d.Source).URI(), "--target", d.TargetConn.WithDB(d.Target).URI(), "--dir", p.WorkDir); err == nil {
			done = append(done, "pgcopydb stream cleanup")
		}
	}
	src, err := pg.Connect(ctx, d.SourceConn.WithDB(d.Source), 30*time.Second)
	if err != nil && !strings.Contains(err.Error(), "does not exist") {
		return done, fmt.Errorf("source: %w", err)
	}
	if src != nil {
		_, _, _ = ReleaseSlot(ctx, d.SourceConn.WithDB(d.Source), d.SlotName, 2*time.Second, 20*time.Second)
		var n int
		src.QueryRow(ctx, `SELECT count(*) FROM pg_replication_slots WHERE slot_name=$1`, d.SlotName).Scan(&n)
		if n > 0 {
			if _, err := src.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, d.SlotName); err != nil {
				src.Close(ctx)
				return done, fmt.Errorf("dropping slot %s: %w", d.SlotName, err)
			}
			done = append(done, "dropped slot "+d.SlotName)
		}
		src.QueryRow(ctx, `SELECT count(*) FROM pg_publication WHERE pubname=$1`, d.SlotName).Scan(&n)
		if n > 0 {
			if _, err := src.Exec(ctx, `DROP PUBLICATION IF EXISTS `+pg.QuoteIdent(d.SlotName)); err != nil {
				src.Close(ctx)
				return done, fmt.Errorf("dropping publication %s: %w", d.SlotName, err)
			}
			done = append(done, "dropped publication "+d.SlotName)
		}
		if opts.DropHeartbeat {
			if _, err := src.Exec(ctx, `DROP SCHEMA IF EXISTS upwell CASCADE`); err == nil {
				done = append(done, "dropped heartbeat schema on source")
			}
		}
		src.Close(ctx)
	}
	// Origins live in a shared catalog: connect to the target cluster's
	// maintenance database so this works even when the target database is gone.
	dst, err := pg.Connect(ctx, d.TargetConn, 30*time.Second)
	if err != nil {
		return done, fmt.Errorf("target: %w", err)
	}
	var n int
	dst.QueryRow(ctx, `SELECT count(*) FROM pg_replication_origin WHERE roname=$1`, d.OriginName).Scan(&n)
	if n > 0 {
		if _, err := dst.Exec(ctx, `SELECT pg_replication_origin_drop($1)`, d.OriginName); err != nil {
			dst.Close(ctx)
			return done, fmt.Errorf("dropping origin %s: %w", d.OriginName, err)
		}
		done = append(done, "dropped origin "+d.OriginName)
	}
	dst.Close(ctx)
	if opts.DropHeartbeat {
		if t, err := pg.Connect(ctx, d.TargetConn.WithDB(d.Target), 30*time.Second); err == nil {
			if _, err := t.Exec(ctx, `DROP SCHEMA IF EXISTS upwell CASCADE`); err == nil {
				done = append(done, "dropped heartbeat schema on target")
			}
			t.Close(ctx)
		}
	}
	if opts.RemoveWorkDir {
		os.RemoveAll(p.WorkDir)
		os.Remove(p.PassFile)
		done = append(done, "removed work directory")
	}
	_ = e.Runner.Forget(ctx, d.Instance)
	return done, nil
}

var (
	lineRe  = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d+) (\d+) ([A-Z]+)\s+(\S+)\s+(.*)$`)
	stepRe  = regexp.MustCompile(`STEP (\d+):`)
	tableRe = regexp.MustCompile(`"([^"]+)"\."([^"]+)"`)
)

// ParseLog turns one raw pgcopydb line into a record.
func (e *Engine) ParseLog(line string) engine.LogRecord {
	m := lineRe.FindStringSubmatch(line)
	if m == nil {
		return engine.LogRecord{Level: "info", Msg: strings.TrimRight(line, "\r")}
	}
	ts, _ := time.ParseInLocation("2006-01-02 15:04:05.000", m[1], time.Local)
	pid, _ := strconv.Atoi(m[2])
	lvl := strings.ToLower(m[3])
	switch lvl {
	case "warn", "warning":
		lvl = "warn"
	case "notice", "info", "sql":
		lvl = "info"
	case "debug", "trace":
	case "error", "fatal":
	default:
		lvl = "info"
	}
	r := engine.LogRecord{TS: ts, PID: pid, Level: lvl, Msg: strings.TrimSpace(m[5])}
	if s := stepRe.FindStringSubmatch(r.Msg); s != nil {
		r.Step = "STEP " + s[1]
	} else if strings.Contains(r.Msg, "COPY") {
		r.Step = "COPY"
	}
	if t := tableRe.FindStringSubmatch(r.Msg); t != nil {
		r.Table = t[1] + "." + t[2]
	}
	return r
}

var (
	slotLost  = regexp.MustCompile(`can no longer get changes from replication slot|has been invalidated because it exceeded the maximum reserved size|wal_status.*lost`)
	baseFail  = regexp.MustCompile(`clone process \d+ has terminated|Failed to clone source database|Failed to create indexes|Failed to copy table|Failed to restore`)
	permanent = regexp.MustCompile(`password authentication failed|permission denied|no pg_hba\.conf entry|No space left on device|out of memory|must be owner of|syntax error|does not exist`)
	transient = regexp.MustCompile(`could not connect|Connection refused|server closed the connection|terminating connection|connection to server .* failed|timeout|too many connections|the database system is (starting up|shutting down)|could not receive data|SSL SYSCALL|broken pipe|Connection reset`)
	cleanEnd  = regexp.MustCompile(`Follow mode is now done, reached endpos|Current endpos .* was previously reached`)
)

// Classify returns the failure class implied by one record. Slot loss is
// decided by the log line, never by the exit code (PC10).
func (e *Engine) Classify(r engine.LogRecord) string {
	switch {
	case cleanEnd.MatchString(r.Msg):
		return engine.FailEndposReached
	case slotLost.MatchString(r.Msg):
		return engine.FailSlotLost
	case r.Level != "error" && r.Level != "fatal":
		return engine.FailNone
	case baseFail.MatchString(r.Msg):
		return engine.FailBaseCopy
	case permanent.MatchString(r.Msg):
		return engine.FailPermanent
	case transient.MatchString(r.Msg):
		return engine.FailTransient
	}
	return engine.FailTransient
}

// CopyRoles copies cluster roles once (best effort) without passwords, as a
// non-superuser admin can: see PlanRoleStatements.
func (e *Engine) CopyRoles(ctx context.Context, src, dst pg.Conn, runDir string) (string, error) {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return "", err
	}
	pass := filepath.Join(runDir, "pgpass")
	if err := pg.WritePassFile(pass, src, dst); err != nil {
		return "", err
	}
	defer os.Remove(pass)
	// pgcopydb copy roles reads pg_authid, which only a superuser may read, so
	// it always fails as DigitalOcean's doadmin. Dump the roles without
	// passwords (pg_roles is readable) and apply what a non-superuser may.
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "pg_dumpall", "--roles-only", "--no-role-passwords", "--dbname", src.URI())
	cmd.Env = append(os.Environ(), "PGPASSFILE="+pass)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pg_dumpall --roles-only: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	_ = os.WriteFile(filepath.Join(runDir, "roles.sql"), out.Bytes(), 0o600)
	conn, err := pg.Connect(ctx, dst, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("target: %w", err)
	}
	defer conn.Close(ctx)
	existing := map[string]bool{}
	rows, err := conn.Query(ctx, `SELECT rolname FROM pg_roles`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			existing[n] = true
		}
	}
	rows.Close()
	plan := PlanRoleStatements(out.String(), existing, src.User)
	created, failed := 0, []string{}
	for _, st := range plan.Statements {
		if _, err := conn.Exec(ctx, st); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", firstWords(st, 4), err))
			continue
		}
		if strings.HasPrefix(st, "CREATE ROLE") {
			created++
		}
	}
	msg := fmt.Sprintf("Copied roles to the target without passwords: %d created, %d already existed", created, plan.Existing)
	if len(plan.New) > 0 {
		msg += ". Set passwords for " + strings.Join(limitStrings(plan.New, 10), ", ") + " on the target (for example in the DigitalOcean control panel) before switching applications"
	}
	if len(failed) > 0 {
		msg += fmt.Sprintf(". %d statement(s) could not be applied, first: %s", len(failed), failed[0])
	}
	return msg + ".", nil
}

// RoleStatements is the filtered role script for a non-superuser target admin.
type RoleStatements struct {
	Statements []string
	New        []string // login roles that will be created without a password
	Existing   int
}

// reservedRole reports roles owned by the platform or the server.
func reservedRole(name, admin string) bool {
	return name == admin || name == "postgres" || name == "doadmin" || strings.HasPrefix(name, "pg_") || strings.HasPrefix(name, "_")
}

var (
	roleNameRe  = regexp.MustCompile(`^(?:CREATE|ALTER) ROLE ("(?:[^"]|"")+"|[^\s;]+)`)
	grantRe     = regexp.MustCompile(`^GRANT ("(?:[^"]|"")+"|\S+) TO ("(?:[^"]|"")+"|\S+)`)
	grantedByRe = regexp.MustCompile(` GRANTED BY ("(?:[^"]|"")+"|[^\s;]+)`)
	privAttrRe  = regexp.MustCompile(`\b(SUPERUSER|REPLICATION|BYPASSRLS)\b`)
)

func unquoteIdent(s string) string {
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// PlanRoleStatements turns pg_dumpall --roles-only output into statements a
// non-superuser admin can run: platform roles and roles that already exist
// are left alone, attributes only a superuser may grant are turned off, and
// GRANTED BY clauses are dropped. Role-level settings are kept.
func PlanRoleStatements(dump string, existing map[string]bool, admin string) RoleStatements {
	var out RoleStatements
	seenNew := map[string]bool{}
	for _, raw := range strings.Split(dump, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "--") || strings.HasPrefix(line, "\\") || strings.HasPrefix(line, "SET ") {
			continue
		}
		if m := roleNameRe.FindStringSubmatch(line); m != nil {
			name := unquoteIdent(m[1])
			if reservedRole(name, admin) {
				continue
			}
			if strings.HasPrefix(line, "CREATE ROLE") {
				if existing[name] {
					out.Existing++
					continue
				}
				seenNew[name] = true
				out.Statements = append(out.Statements, line)
				continue
			}
			if !seenNew[name] {
				continue // never change a role that already existed on the target
			}
			line = privAttrRe.ReplaceAllStringFunc(line, func(a string) string { return "NO" + a })
			if strings.Contains(line, " LOGIN") && !strings.Contains(line, "NOLOGIN") {
				out.New = append(out.New, name)
			}
			out.Statements = append(out.Statements, line)
			continue
		}
		if m := grantRe.FindStringSubmatch(line); m != nil {
			role, member := unquoteIdent(m[1]), unquoteIdent(m[2])
			if reservedRole(member, admin) || (reservedRole(role, admin) && !strings.HasPrefix(role, "pg_")) {
				continue
			}
			out.Statements = append(out.Statements, grantedByRe.ReplaceAllString(line, ""))
		}
	}
	return out
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

func limitStrings(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("and %d more", len(s)-n))
	}
	return s
}
