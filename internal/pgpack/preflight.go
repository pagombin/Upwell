package pgpack

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/pg"
	"github.com/pagombin/upwell/internal/settings"
)

// DB is one database under preflight.
type DB struct {
	Source, Target   string
	SlotName, Origin string
}

// Permission is the customer permission record summary.
type Permission struct {
	Present  bool
	Missing  []string
	Customer string
	Ticket   string
}

// Env is everything preflight needs.
type Env struct {
	Source, Target  pg.Conn // maintenance databases
	TargetStorageGB float64
	Databases       []DB
	Settings        settings.Values
	EngineVersion   string
	EngineErr       error
	PgBinDir        string
	WorkDir         string
	Permission      Permission
	Resuming        bool
}

func cd(id, scope, title, failsAs string) check.Definition {
	return check.Definition{ID: id, Version: 1, Scope: scope, Title: title, FailsAs: failsAs}
}

// Catalog lists every check this pack runs.
var Catalog = []check.Definition{
	cd("customer_permission", check.ScopeMigration, "Customer permission recorded", "hard"),
	cd("tools", check.ScopeHost, "Engine and client tools installed", "hard"),
	cd("engine_version", check.ScopeHost, "Engine version supports this migration", "hard"),
	cd("client_version", check.ScopeHost, "PostgreSQL client version", "hard"),
	cd("host_resources", check.ScopeHost, "Droplet size", "warning"),
	cd("clock_sync", check.ScopeHost, "System clock synchronized", "warning"),
	cd("work_dir", check.ScopeHost, "Work directory", "hard"),
	cd("staging_space", check.ScopeHost, "Room for change staging", "acceptable"),
	cd("dns", check.ScopeClusters, "Hostnames resolve", "hard"),
	cd("network", check.ScopeClusters, "Network path", "warning"),
	cd("connect", check.ScopeClusters, "Clusters reachable", "hard"),
	cd("trusted_sources", check.ScopeClusters, "Droplet allowed on both clusters", "hard"),
	cd("session_pinning", check.ScopeClusters, "Sessions keep their settings (no transaction pooling)", "hard"),
	cd("same_cluster", check.ScopeClusters, "Source and target are different", "hard"),
	cd("version_order", check.ScopeClusters, "Target version at least source version", "hard"),
	cd("primary", check.ScopeClusters, "Both endpoints are primaries", "hard"),
	cd("connections", check.ScopeClusters, "Free connections", "acceptable"),
	cd("throughput_sample", check.ScopeClusters, "Throughput estimate", "info"),
	cd("apply_rate", check.ScopeDatabase, "Commit rate within the engine's apply rate", "warning"),
	cd("cdc_throughput", check.ScopeClusters, "Source WAL rate against receive throughput", "warning"),
	cd("wal_level", check.ScopeSource, "Logical decoding enabled", "hard"),
	cd("repl_slots", check.ScopeSource, "Free replication slots", "hard"),
	cd("wal_senders", check.ScopeSource, "Free WAL senders", "hard"),
	cd("repl_privilege", check.ScopeSource, "Replication privilege", "hard"),
	cd("prepared_xacts", check.ScopeSource, "No prepared transactions", "acceptable"),
	cd("long_transactions", check.ScopeSource, "No long-running transactions", "warning"),
	cd("maintenance_window", check.ScopeSource, "Maintenance window", "warning"),
	cd("databases", check.ScopeSource, "Databases to migrate", "hard"),
	cd("session_timeouts", check.ScopeSource, "Session timeouts", "acceptable"),
	cd("slot_wal_cap", check.ScopeSource, "Slot WAL retention cap", "warning"),
	cd("target_disk", check.ScopeTarget, "Target storage", "acceptable"),
	cd("repl_protocol", check.ScopeDatabase, "Replication connection", "hard"),
	cd("read_privs", check.ScopeDatabase, "Read access to every table", "hard"),
	cd("publication_privs", check.ScopeDatabase, "Decoding plugin", "hard"),
	cd("replica_identity", check.ScopeDatabase, "Updates and deletes can replay", "hard"),
	cd("row_security", check.ScopeDatabase, "Every row is readable (row-level security)", "hard"),
	cd("identity_full_types", check.ScopeDatabase, "Full replica identity on comparable columns", "hard"),
	cd("identity_full_duplicates", check.ScopeDatabase, "Full replica identity without a unique key", "warning"),
	cd("large_objects", check.ScopeDatabase, "Large objects", "warning"),
	cd("unlogged_tables", check.ScopeDatabase, "Unlogged tables", "warning"),
	cd("materialized_views", check.ScopeDatabase, "Materialized views", "warning"),
	cd("foreign_servers", check.ScopeDatabase, "Foreign servers", "warning"),
	cd("event_triggers", check.ScopeDatabase, "Event triggers", "warning"),
	cd("pg_cron", check.ScopeDatabase, "pg_cron jobs", "warning"),
	cd("pg_partman", check.ScopeDatabase, "pg_partman", "warning"),
	cd("encoding", check.ScopeDatabase, "Same encoding", "hard"),
	cd("collation", check.ScopeDatabase, "Same collation", "acceptable"),
	cd("dst_conflicts", check.ScopeDatabase, "No conflicting objects on the target", "hard"),
	cd("dst_extra_objects", check.ScopeDatabase, "Objects only on the target", "warning"),
	cd("dst_database", check.ScopeDatabase, "Target database", "hard"),
	cd("extensions", check.ScopeDatabase, "Extensions available on the target", "hard"),
	cd("slot_exists", check.ScopeDatabase, "No leftover replication slot", "hard"),
	cd("origin_exists", check.ScopeDatabase, "No leftover replication origin", "hard"),
	cd("publication_exists", check.ScopeDatabase, "No leftover publication", "hard"),
	cd("target_apply_privileges", check.ScopeDatabase, "Target allows the engine to apply changes", "hard"),
	cd("heartbeat_possible", check.ScopeDatabase, "Heartbeat table can be created", "hard"),
	cd("write_freeze_available", check.ScopeDatabase, "Write freeze available", "info"),
}

func def(id string) check.Definition {
	for _, d := range Catalog {
		if d.ID == id {
			return d
		}
	}
	return check.Definition{ID: id, Version: 1, Title: id}
}

// Runner executes the catalog.
type Runner struct {
	Env  Env
	Emit func(check.Result)

	src, dst         *pgx.Conn
	srcVer, dstVer   int
	srcErr, dstErr   error
	walRateBps       float64
	totalSourceBytes int64
}

func (r *Runner) emit(id, scope, db, level string, hard bool, msg, remediation string, ev map[string]any, start time.Time) {
	d := def(id)
	if scope == "" {
		scope = d.Scope
	}
	res := check.Result{CheckID: id, Version: d.Version, Scope: scope, Database: db, Level: level, Hard: hard && level == check.Blocker,
		Title: d.Title, Message: msg, Evidence: ev, Remediation: remediation, DurationMS: time.Since(start).Milliseconds()}
	res.Finalize()
	r.Emit(res)
}

func ok(r *Runner, id, scope, db, msg string, ev map[string]any, t time.Time) {
	r.emit(id, scope, db, check.OK, false, msg, "", ev, t)
}

// Run executes every check, emitting results as they finish.
func (r *Runner) Run(ctx context.Context) {
	e := r.Env
	if e.Settings == nil {
		e.Settings = settings.Values{}
	}
	r.migrationChecks()
	r.hostChecks(ctx)
	r.clusterChecks(ctx)
	r.clientVersionCheck()
	defer func() {
		if r.src != nil {
			r.src.Close(ctx)
		}
		if r.dst != nil {
			r.dst.Close(ctx)
		}
	}()
	if r.src != nil {
		r.sourceChecks(ctx)
	}
	if r.dst != nil {
		r.targetChecks(ctx)
	}
	if r.src != nil && r.dst != nil {
		for _, d := range e.Databases {
			r.databaseChecks(ctx, d)
		}
		r.samplingChecks(ctx)
	}
}

func (r *Runner) migrationChecks() {
	t := time.Now()
	p := r.Env.Permission
	ev := map[string]any{"customer": p.Customer, "ticket": p.Ticket, "missing": p.Missing}
	if !p.Present || len(p.Missing) > 0 {
		r.emit("customer_permission", "", "", check.Blocker, true, "The customer permission record is incomplete, so Upwell will not touch the clusters on this customer's behalf.",
			"Fill in the customer name, account ID, ticket, who granted permission and when, and the scope.", ev, t)
		return
	}
	ok(r, "customer_permission", "", "", fmt.Sprintf("Permission recorded for %s under %s.", p.Customer, p.Ticket), ev, t)
}

func (r *Runner) bin(name string) string {
	if r.Env.PgBinDir != "" {
		return r.Env.PgBinDir + "/" + name
	}
	return name
}

var majorRe = regexp.MustCompile(`\) (\d+)(\.\d+)?`)

func toolMajor(path string) (int, string, error) {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return 0, "", err
	}
	m := majorRe.FindStringSubmatch(string(out))
	if m == nil {
		return 0, strings.TrimSpace(string(out)), fmt.Errorf("unexpected version output")
	}
	n, _ := strconv.Atoi(m[1])
	return n, strings.TrimSpace(string(out)), nil
}

// clientVersionCheck: pg_dump and pg_restore must be at least the source's
// major version (a dump tool cannot read a newer server's catalog).
func (r *Runner) clientVersionCheck() {
	if r.srcVer == 0 {
		return
	}
	t := time.Now()
	srcMajor := r.srcVer / 10000
	ev := map[string]any{"source_major": srcMajor, "target_major": r.dstVer / 10000}
	low := []string{}
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		maj, out, err := toolMajor(r.bin(tool))
		ev[tool] = out
		if err != nil {
			low = append(low, fmt.Sprintf("%s (%v)", tool, err))
		} else if maj < srcMajor {
			low = append(low, fmt.Sprintf("%s %d", tool, maj))
		}
	}
	if len(low) > 0 {
		r.emit("client_version", "", "", check.Blocker, true, fmt.Sprintf("The source runs PostgreSQL %d but this droplet has %s.", srcMajor, strings.Join(low, " and ")),
			"Run install.sh again; it installs the newest PostgreSQL client, which reads every older server.", ev, t)
		return
	}
	ok(r, "client_version", "", "", fmt.Sprintf("pg_dump and pg_restore can read the source's PostgreSQL %d.", srcMajor), ev, t)
}

func (r *Runner) hostChecks(ctx context.Context) {
	e := r.Env
	t := time.Now()
	missing := []string{}
	if e.EngineErr != nil {
		missing = append(missing, "pgcopydb ("+e.EngineErr.Error()+")")
	}
	for _, tool := range []string{"pg_dump", "pg_restore", "psql"} {
		if _, err := exec.LookPath(r.bin(tool)); err != nil {
			missing = append(missing, tool)
		}
	}
	ev := map[string]any{"missing": missing, "engine_version": e.EngineVersion}
	if len(missing) > 0 {
		r.emit("tools", "", "", check.Blocker, true, "Required tools are missing: "+strings.Join(missing, ", ")+".", "Run install.sh again; it installs the pinned pgcopydb and PostgreSQL client versions.", ev, t)
	} else {
		ok(r, "tools", "", "", "pgcopydb "+e.EngineVersion+", pg_dump, pg_restore and psql are installed.", ev, t)
	}
	t = time.Now()
	if e.EngineErr == nil {
		maj, min, parsed := EngineMajorMinor(e.EngineVersion)
		ev := map[string]any{"version": e.EngineVersion, "major": maj, "minor": min}
		if !parsed {
			r.emit("engine_version", "", "", check.Blocker, true, "Upwell could not read the pgcopydb version from \""+e.EngineVersion+"\".", "Run install.sh again; it installs pgcopydb 0.18.", ev, t)
		} else if maj == 0 && min < 18 {
			r.emit("engine_version", "", "", check.Blocker, true, "pgcopydb "+e.EngineVersion+" has a known change-apply bug (exit 12) and cannot run online migrations.", "Install pgcopydb 0.18 or later with install.sh.", ev, t)
		} else {
			ok(r, "engine_version", "", "", "pgcopydb "+e.EngineVersion+" supports online migrations up to PostgreSQL 18.", ev, t)
		}
	}
	t = time.Now()
	ncpu := runtime.NumCPU()
	var si syscall.Sysinfo_t
	_ = syscall.Sysinfo(&si)
	ram := float64(si.Totalram) * float64(si.Unit) / (1 << 30)
	var free float64
	var st syscall.Statfs_t
	if syscall.Statfs(e.WorkDir, &st) == nil {
		free = float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
	}
	ev = map[string]any{"vcpu": ncpu, "ram_gb": round1(ram), "_work_free_gb": round1(free)}
	if ncpu < 4 || ram < 7.5 {
		r.emit("host_resources", "", "", check.Warning, false, fmt.Sprintf("This droplet has %d vCPU and %.1f GB RAM; the smallest recommended size is 4 vCPU and 8 GB.", ncpu, ram), "Resize the droplet, or expect a slower copy.", ev, t)
	} else {
		ok(r, "host_resources", "", "", fmt.Sprintf("%d vCPU, %.1f GB RAM.", ncpu, ram), ev, t)
	}
	t = time.Now()
	synced := false
	if b, err := exec.Command("timedatectl", "show", "-p", "NTPSynchronized", "--value").Output(); err == nil {
		synced = strings.TrimSpace(string(b)) == "yes"
	} else if _, err := os.Stat("/run/systemd/timesync/synchronized"); err == nil {
		synced = true
	}
	if synced {
		ok(r, "clock_sync", "", "", "The system clock is synchronized.", map[string]any{"synchronized": true}, t)
	} else {
		r.emit("clock_sync", "", "", check.Warning, false, "Upwell could not confirm the clock is synchronized, so log times may not line up with the clusters' logs.", "Enable chrony or systemd-timesyncd (install.sh installs chrony).", map[string]any{"synchronized": false}, t)
	}
	t = time.Now()
	ev = map[string]any{"path": e.WorkDir}
	if err := os.MkdirAll(e.WorkDir, 0o750); err != nil {
		r.emit("work_dir", "", "", check.Blocker, true, "The work directory cannot be created: "+err.Error(), "Create "+e.WorkDir+" and make it writable by the upwell user.", ev, t)
	} else if f, err := os.CreateTemp(e.WorkDir, ".probe"); err != nil {
		r.emit("work_dir", "", "", check.Blocker, true, "The work directory is not writable: "+err.Error(), "Make "+e.WorkDir+" writable by the upwell user.", ev, t)
	} else {
		f.Close()
		os.Remove(f.Name())
		var rootSt syscall.Statfs_t
		_ = syscall.Statfs("/", &rootSt)
		if rootSt.Fsid == st.Fsid {
			r.emit("work_dir", "", "", check.Warning, false, "The work directory is on the root filesystem; a filling disk would affect the whole droplet.", "Mount a dedicated block-storage volume at "+e.WorkDir+".", ev, t)
		} else {
			ok(r, "work_dir", "", "", "The work directory is writable and on its own volume.", ev, t)
		}
	}
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func (r *Runner) clusterChecks(ctx context.Context) {
	e := r.Env
	for _, side := range []struct {
		name string
		c    pg.Conn
	}{{"source", e.Source}, {"target", e.Target}} {
		t := time.Now()
		addrs, err := net.DefaultResolver.LookupHost(ctx, side.c.Host)
		ev := map[string]any{"host": side.c.Host, "addresses": addrs}
		if err != nil {
			r.emit("dns", side.name, "", check.Blocker, true, fmt.Sprintf("The %s hostname %s does not resolve.", side.name, side.c.Host), "Check the hostname; use the private hostname from the cluster's connection details.", ev, t)
			continue
		}
		ok(r, "dns", side.name, "", fmt.Sprintf("%s resolves to %s.", side.c.Host, strings.Join(addrs, ", ")), ev, t)
		t = time.Now()
		private := true
		for _, a := range addrs {
			ip := net.ParseIP(a)
			if ip != nil && !(ip.IsPrivate() || ip.IsLoopback()) {
				private = false
			}
		}
		ev = map[string]any{"private": private, "port": side.c.Port}
		switch {
		case side.c.Port == 25061:
			r.emit("network", side.name, "", check.Warning, false, "Port 25061 is the connection pooler; replication needs a direct connection.", "Use port 25060.", ev, t)
		case !private:
			r.emit("network", side.name, "", check.Warning, false, fmt.Sprintf("The %s connection uses a public address; traffic leaves the VPC and is slower.", side.name), "Use the cluster's private hostname and run Upwell in the same VPC.", ev, t)
		default:
			ok(r, "network", side.name, "", "Private network path.", ev, t)
		}
		t = time.Now()
		conn, err := pg.Connect(ctx, side.c, 30*time.Second)
		if err != nil {
			r.emit("connect", side.name, "", check.Blocker, true, fmt.Sprintf("Upwell cannot connect to the %s cluster: %v", side.name, err), "Check the connection details and that this droplet is a trusted source.", map[string]any{"error": err.Error()}, t)
			r.emit("trusted_sources", side.name, "", check.Blocker, true, "Not confirmed: the connection failed.", "Add this droplet to the cluster's trusted sources in the control panel.", map[string]any{}, t)
			if side.name == "source" {
				r.srcErr = err
			} else {
				r.dstErr = err
			}
			continue
		}
		cp := r.probe(ctx, "connect", side.name, "")
		ver, verr := pg.ServerVersionNum(ctx, conn)
		cp.fail(verr)
		var vs string
		cp.row(conn, `SHOW server_version`).Scan(&vs)
		cp.ok(fmt.Sprintf("Connected to PostgreSQL %s as %s.", vs, side.c.User), map[string]any{"server_version": vs})
		ok(r, "trusted_sources", side.name, "", "Confirmed by connecting (the DigitalOcean API check is coming soon).", map[string]any{"method": "connect"}, t)
		if cp.err != nil {
			// Without the server version nothing after this can be judged.
			conn.Close(ctx)
			if side.name == "source" {
				r.srcErr = cp.err
			} else {
				r.dstErr = cp.err
			}
			continue
		}
		t = time.Now()
		r.sessionCheck(ctx, side.name, conn, side.c, t)
		if side.name == "source" {
			r.src, r.srcVer = conn, ver
		} else {
			r.dst, r.dstVer = conn, ver
		}
	}
	if r.src == nil || r.dst == nil {
		return
	}
	t := time.Now()
	same := strings.EqualFold(e.Source.Host, e.Target.Host) && e.Source.Port == e.Target.Port
	if same {
		r.emit("same_cluster", "", "", check.Blocker, true, "Source and target are the same server.", "Enter the Advanced cluster's details as the target.", map[string]any{"host": e.Source.Host, "port": e.Source.Port}, t)
	} else {
		ok(r, "same_cluster", "", "", "Source and target are different servers.", map[string]any{"source": e.Source.Host, "target": e.Target.Host}, t)
	}
	t = time.Now()
	ev := map[string]any{"source_major": r.srcVer / 10000, "target_major": r.dstVer / 10000}
	if r.dstVer/10000 < r.srcVer/10000 {
		r.emit("version_order", "", "", check.Blocker, true, fmt.Sprintf("The target runs PostgreSQL %d, older than the source's %d.", r.dstVer/10000, r.srcVer/10000), "Create the target with the same or a newer major version.", ev, t)
	} else {
		ok(r, "version_order", "", "", fmt.Sprintf("PostgreSQL %d to %d.", r.srcVer/10000, r.dstVer/10000), ev, t)
	}
	p := r.probe(ctx, "primary", "", "")
	var sRec, dRec bool
	p.row(r.src, `SELECT pg_is_in_recovery()`).Scan(&sRec)
	p.row(r.dst, `SELECT pg_is_in_recovery()`).Scan(&dRec)
	if sRec || dRec {
		p.emit(check.Blocker, true, "One endpoint is a standby; both must be primaries.", "Use the primary's connection details, not a read-only node's.", map[string]any{"source_standby": sRec, "target_standby": dRec})
	} else {
		p.ok("Both endpoints are primaries.", map[string]any{"source_standby": false, "target_standby": false})
	}
	p = r.probe(ctx, "connections", "", "")
	n := max(len(e.Databases), 1)
	conc := min(n, max(e.Settings.Int("max_concurrent_base_copies"), 1))
	tj, ij := settings.Jobs(e.Settings, conc)
	need := conc*(tj+ij+4) + 4
	sFree, dFree := freeConns(p, r.src), freeConns(p, r.dst)
	ev = map[string]any{"needed": need, "source_free": sFree, "target_free": dFree, "table_jobs": tj, "index_jobs": ij, "concurrent": conc}
	if sFree < need || dFree < need {
		p.emit(check.Blocker, false, fmt.Sprintf("The engines will open about %d connections at once; the source has %d free and the target %d.", need, sFree, dFree),
			"Lower table and index jobs or max concurrent base copies in Options, or accept the risk if the estimate is pessimistic.", ev)
	} else {
		p.ok(fmt.Sprintf("About %d connections needed; %d free on the source and %d on the target.", need, sFree, dFree), ev)
	}
}

func freeConns(p *probe, c *pgx.Conn) int {
	var max, used, reserved int
	p.row(c, `SELECT current_setting('max_connections')::int, (SELECT count(*) FROM pg_stat_activity), current_setting('superuser_reserved_connections')::int`).Scan(&max, &used, &reserved)
	return max - used - reserved
}

func (r *Runner) sourceChecks(ctx context.Context) {
	e := r.Env
	c := r.src
	p := r.probe(ctx, "wal_level", "", "")
	var walLevel string
	p.row(c, `SHOW wal_level`).Scan(&walLevel)
	if walLevel != "logical" {
		p.emit(check.Blocker, true, "wal_level is "+walLevel+"; online migration needs logical.", "Set wal_level to logical on the source (on DigitalOcean this is on by default for PostgreSQL clusters).", map[string]any{"wal_level": walLevel})
	} else {
		p.ok("wal_level is logical.", map[string]any{"wal_level": walLevel})
	}
	n := len(e.Databases)
	p = r.probe(ctx, "repl_slots", "", "")
	var maxSlots, usedSlots, maxSenders, usedSenders int
	p.row(c, `SELECT current_setting('max_replication_slots')::int, (SELECT count(*) FROM pg_replication_slots), current_setting('max_wal_senders')::int, (SELECT count(*) FROM pg_stat_replication)`).Scan(&maxSlots, &usedSlots, &maxSenders, &usedSenders)
	ps := r.probe(ctx, "wal_senders", "", "")
	ps.fail(p.err)
	ev := map[string]any{"max": maxSlots, "used": usedSlots, "needed": n}
	if e.Resuming {
		usedSlots -= n
	}
	if maxSlots-usedSlots < n {
		p.emit(check.Blocker, true, fmt.Sprintf("%d replication slots are free and %d databases need one each.", maxSlots-usedSlots, n), "Split the databases into several migrations, or remove unused slots.", ev)
	} else {
		p.ok(fmt.Sprintf("%d of %d slots free; %d needed.", maxSlots-usedSlots, maxSlots, n), ev)
	}
	ev = map[string]any{"max": maxSenders, "used": usedSenders, "needed": n}
	if maxSenders-usedSenders < n {
		ps.emit(check.Blocker, true, fmt.Sprintf("%d WAL senders are free and %d databases need one each.", maxSenders-usedSenders, n), "Split the databases into several migrations.", ev)
	} else {
		ps.ok(fmt.Sprintf("%d of %d WAL senders free; %d needed.", maxSenders-usedSenders, maxSenders, n), ev)
	}
	p = r.probe(ctx, "repl_privilege", "", "")
	var repl, super bool
	p.row(c, `SELECT rolreplication, rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&repl, &super)
	if !repl && !super {
		p.emit(check.Blocker, true, e.Source.User+" has neither REPLICATION nor superuser.", "Use the cluster's admin user (doadmin), which has REPLICATION.", map[string]any{"replication": repl, "superuser": super})
	} else {
		p.ok(e.Source.User+" has the REPLICATION privilege.", map[string]any{"replication": repl, "superuser": super})
	}
	p = r.probe(ctx, "prepared_xacts", "", "")
	var prepared int
	p.row(c, `SELECT count(*) FROM pg_prepared_xacts`).Scan(&prepared)
	if prepared > 0 {
		p.emit(check.Blocker, false, fmt.Sprintf("%d prepared transactions are open; creating a replication slot waits behind them.", prepared), "Commit or roll back the prepared transactions, or accept the risk that slot creation may wait.", map[string]any{"count": prepared})
	} else {
		p.ok("No prepared transactions.", map[string]any{"count": 0})
	}
	p = r.probe(ctx, "long_transactions", "", "")
	mins := max(e.Settings.Int("long_transaction_minutes"), 1)
	var longCount int
	var oldest *float64
	p.row(c, `SELECT count(*), max(extract(epoch FROM now()-xact_start))::float8 FROM pg_stat_activity WHERE xact_start < now() - make_interval(mins => $1) AND backend_type='client backend'`, mins).Scan(&longCount, &oldest)
	if longCount > 0 {
		p.emit(check.Warning, false, fmt.Sprintf("%d transactions have been open longer than %d minutes; the base copy snapshot waits for them.", longCount, mins), "Find them in pg_stat_activity and ask the customer whether they can end.", map[string]any{"count": longCount, "_oldest_seconds": oldest})
	} else {
		p.ok(fmt.Sprintf("No transactions open longer than %d minutes.", mins), map[string]any{"count": 0})
	}
	t := time.Now()
	r.emit("maintenance_window", "", "", check.Info, false, "Not checked: the maintenance window comes from the DigitalOcean API, which is not available in this version. Check the cluster's maintenance window in the control panel.", "", map[string]any{"checked": false}, t)
	t = time.Now()
	names := make([]string, 0, n)
	for _, d := range e.Databases {
		names = append(names, d.Source)
	}
	if n == 0 {
		r.emit("databases", "", "", check.Blocker, true, "No databases are selected.", "Select at least one database in the Databases step.", map[string]any{"databases": names}, t)
	} else {
		ok(r, "databases", "", "", fmt.Sprintf("%d database(s) selected: %s.", n, strings.Join(names, ", ")), map[string]any{"databases": names}, t)
	}
	// Read the timeouts an engine session inherits, on a connection that does
	// not set its own statement timeout, and compare them with the estimated
	// base copy: pgcopydb holds its snapshot in a transaction that stays idle
	// for the whole copy, and a large table's COPY is one statement.
	p = r.probe(ctx, "session_timeouts", "", "")
	ms := map[string]int64{}
	if tc, err := pg.Connect(ctx, e.Source, 0); err != nil {
		p.fail(err)
	} else {
		if rows, err := pg.Query(ctx, tc, `SELECT name, setting::bigint FROM pg_settings WHERE name IN ('statement_timeout','idle_in_transaction_session_timeout','idle_session_timeout')`); err == nil {
			for rows.Next() {
				var n string
				var v int64
				p.fail(rows.Scan(&n, &v))
				ms[n] = v
			}
			p.fail(rows.Err())
			rows.Close()
		} else {
			p.fail(err)
		}
		// ALTER DATABASE ... SET and ALTER ROLE ... IN DATABASE ... SET apply
		// only inside the migrated databases, not the maintenance database.
		for _, kv := range p.strings(tc, `SELECT unnest(s.setconfig) FROM pg_db_role_setting s JOIN pg_database d ON d.oid=s.setdatabase
			WHERE d.datname = ANY($1) AND s.setrole IN (0, (SELECT oid FROM pg_roles WHERE rolname=current_user))`, names) {
			k, v, _ := strings.Cut(kv, "=")
			if k == "idle_session_timeout" {
				if n := settingMillis(v); n > 0 && (ms[k] == 0 || n < ms[k]) {
					ms[k] = n
				}
			}
		}
		tc.Close(ctx)
	}
	var srcBytes int64
	for _, d := range e.Databases {
		var b int64
		p.row(c, `SELECT pg_database_size($1)`, d.Source).Scan(&b)
		srcBytes += b
	}
	mibs := e.Settings.Float("assumed_throughput_mibs")
	if mibs <= 0 {
		mibs = 102
	}
	copySecs := float64(srcBytes) / (mibs * 1024 * 1024)
	need := 3 * copySecs
	if need < 6*3600 {
		need = 6 * 3600
	}
	// pgcopydb sets statement_timeout and idle_in_transaction_session_timeout
	// to 0 in every session it opens (copydb.h COMMON_GUC_SETTINGS and
	// srcSettings), and so do pg_dump and pg_restore, so those two cannot cut
	// the copy. idle_session_timeout is not overridden.
	var cleared []string
	for _, k := range []struct{ name, label string }{{"statement_timeout", "statement"}, {"idle_in_transaction_session_timeout", "idle in transaction"}} {
		if v := ms[k.name]; v > 0 {
			cleared = append(cleared, fmt.Sprintf("%s %s", k.label, humanDuration(float64(v)/1000)))
		}
	}
	note := ""
	if len(cleared) > 0 {
		note = fmt.Sprintf(" The role's other timeouts (%s) do not matter: pgcopydb and pg_dump clear them in their own sessions.", strings.Join(cleared, ", "))
	}
	ev = map[string]any{"timeouts_ms": ms, "_estimated_copy_seconds": copySecs, "_required_seconds": need, "cleared_by_engine": []string{"statement_timeout", "idle_in_transaction_session_timeout"}}
	idleSess := float64(ms["idle_session_timeout"]) / 1000
	switch {
	case idleSess > 0 && idleSess < need:
		p.emit(check.Blocker, false, fmt.Sprintf("Sessions for %s inherit idle_session_timeout %s, shorter than %s (three times the estimated base copy, at least 6 hours). pgcopydb does not override it, so an engine connection waiting for others could be closed.%s", e.Source.User, humanDuration(idleSess), humanDuration(need), note),
			"Clear it for the admin role (ALTER ROLE "+pg.QuoteIdent(e.Source.User)+" RESET idle_session_timeout) or accept the risk.", ev)
	case idleSess > 0:
		p.emit(check.Warning, false, fmt.Sprintf("Sessions for %s inherit idle_session_timeout %s, long enough for the estimated base copy of %s.%s", e.Source.User, humanDuration(idleSess), humanDuration(copySecs), note),
			"Nothing to do unless the copy runs far slower than estimated.", ev)
	default:
		p.ok("No timeout can cut the engine's sessions."+note, ev)
	}
}

func (r *Runner) targetChecks(ctx context.Context) {
	e := r.Env
	p := r.probe(ctx, "target_disk", "", "")
	var used int64
	p.row(r.dst, `SELECT COALESCE(sum(pg_database_size(oid)),0)::bigint FROM pg_database WHERE datallowconn`).Scan(&used)
	var srcBytes int64
	if r.src != nil {
		for _, d := range e.Databases {
			var b int64
			p.row(r.src, `SELECT pg_database_size($1)`, d.Source).Scan(&b)
			srcBytes += b
		}
	}
	r.totalSourceBytes = srcBytes
	factor := e.Settings.Float("target_disk_factor")
	if factor == 0 {
		factor = 1.3
	}
	need := float64(used) + factor*float64(srcBytes)
	ev := map[string]any{"target_used_bytes": used, "source_bytes": srcBytes, "factor": factor, "storage_gb": e.TargetStorageGB}
	switch {
	case e.TargetStorageGB <= 0:
		p.emit(check.Warning, false, fmt.Sprintf("Target storage size is unknown; the migration needs about %s.", humanBytes(need)), "Enter the target's storage size in the Target step so Upwell can check it.", ev)
	case need > e.TargetStorageGB*(1<<30):
		p.emit(check.Blocker, false, fmt.Sprintf("The target has %.0f GB; existing data plus %.1f × the databases being migrated needs %s.", e.TargetStorageGB, factor, humanBytes(need)), "Resize the target's storage before starting, or accept the risk.", ev)
	default:
		p.ok(fmt.Sprintf("Needs about %s of the target's %.0f GB.", humanBytes(need), e.TargetStorageGB), ev)
	}
}

func humanBytes(b float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

func (r *Runner) databaseChecks(ctx context.Context, d DB) {
	e := r.Env
	t := time.Now()
	sc, err := pg.Connect(ctx, e.Source.WithDB(d.Source), 30*time.Second)
	if err != nil {
		r.emit("read_privs", "", d.Source, check.Blocker, true, "Cannot connect to the source database: "+err.Error(), "Check the database name and the user's CONNECT privilege.", map[string]any{"error": err.Error()}, t)
		return
	}
	defer sc.Close(ctx)

	// Replication-protocol connection.
	t = time.Now()
	if err := replicationProbe(ctx, e.Source.WithDB(d.Source)); err != nil {
		r.emit("repl_protocol", "", d.Source, check.Blocker, true, "A replication connection to this database failed: "+err.Error(), "Check that the user has REPLICATION and that replication connections are allowed.", map[string]any{"error": err.Error()}, t)
	} else {
		ok(r, "repl_protocol", "", d.Source, "Replication connection works.", map[string]any{}, t)
	}

	p := r.probe(ctx, "read_privs", "", d.Source)
	noRead, noReadN := p.sample(sc, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p','S') AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
		AND NOT has_table_privilege(c.oid,'SELECT')`, 50)
	if noReadN > 0 {
		p.emit(check.Blocker, true, fmt.Sprintf("%s cannot read %d table(s) or sequence(s), for example %s.", e.Source.User, noReadN, noRead[0]), "Grant SELECT on them to the admin user, or migrate as their owner.", map[string]any{"unreadable": noRead, "count": noReadN})
	} else {
		p.ok("Every table and sequence is readable.", map[string]any{"unreadable": []string{}, "count": 0})
	}

	t = time.Now()
	choice, err := ChoosePlugin(ctx, sc, e.Settings.Str("decoding_plugin"))
	ev := map[string]any{"plugin": choice.Plugin, "reasons": choice.Reasons}
	switch {
	case err != nil:
		r.couldNotCheck("publication_privs", "", d.Source, err, ev, t)
	case !choice.OK:
		r.emit("publication_privs", "", d.Source, check.Blocker, true, "pgoutput is forced but cannot work here: "+strings.Join(choice.Reasons, "; ")+".", "Set the decoding plugin to auto or test_decoding.", ev, t)
	case len(choice.Reasons) > 0:
		r.emit("publication_privs", "", d.Source, check.Warning, false, "This database will use test_decoding instead of pgoutput: "+strings.Join(choice.Reasons, "; ")+".", "No action needed; test_decoding works for every table.", ev, t)
	default:
		ok(r, "publication_privs", "", d.Source, "pgoutput can create its publication.", ev, t)
	}

	p = r.probe(ctx, "replica_identity", "", d.Source)
	// Leaf tables only: PostgreSQL checks the partition a row is in, never
	// its partitioned parent (tested, D59). 'i' whose index was dropped
	// behaves like NOTHING.
	const noIdentWhere = `c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%'
		AND ((c.relreplident='d' AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid=c.oid AND i.indisprimary))
		  OR c.relreplident='n'
		  OR (c.relreplident='i' AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid=c.oid AND i.indisreplident)))`
	noIdent, noIdentN := p.sample(sc, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE `+noIdentWhere, 100)
	if noIdentN > 0 {
		var raw []byte
		p.row(sc, `SELECT coalesce(json_agg(x ORDER BY x."table"), '[]') FROM (
			SELECT format('%I.%I', n.nspname, c.relname) AS "table", pg_get_userbyid(c.relowner) AS owner, pg_has_role(c.relowner, 'USAGE') AS owned_by_you,
			  c.relreplident::text AS identity, coalesce(s.n_tup_upd, 0) AS updates, coalesce(s.n_tup_del, 0) AS deletes, pg_total_relation_size(c.oid) AS size_bytes
			FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace LEFT JOIN pg_stat_user_tables s ON s.relid=c.oid
			WHERE `+noIdentWhere+` ORDER BY 1 LIMIT 100) x`).Scan(&raw)
		var details []identityDetail
		if p.err == nil {
			p.fail(json.Unmarshal(raw, &details))
		}
		plugin := ""
		if err == nil {
			plugin = choice.Plugin
		}
		msg, fix, ev := replicaIdentityAdvice(plugin, e.Source.User, noIdentN, details)
		ev["tables"], ev["count"] = noIdent, noIdentN
		// Hard under every plugin: with pgoutput the source refuses the
		// application's UPDATE and DELETE (tested); with test_decoding they
		// are decoded without the old key and cannot be applied.
		p.emit(check.Blocker, true, msg, fix, ev)
	} else {
		p.ok("Every table can replay updates and deletes.", map[string]any{"tables": []string{}, "count": 0})
	}

	// Row-level security that applies to the admin user: the engine's COPY
	// would return only the rows its policies allow, silently.
	p = r.probe(ctx, "row_security", "", d.Source)
	rls, rlsN := p.sample(sc, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p') AND c.relrowsecurity AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_temp%'
		AND NOT (SELECT rolbypassrls OR rolsuper FROM pg_roles WHERE rolname=current_user)
		AND (c.relforcerowsecurity OR NOT pg_has_role(c.relowner, 'USAGE'))`, 100)
	if rlsN > 0 {
		p.emit(check.Blocker, true, fmt.Sprintf("%d table(s) have row-level security that applies to %s (for example %s): the copy would contain only the rows its policies allow, and verification cannot read them all.", rlsN, e.Source.User, rls[0]),
			"Disable or relax the policies for the migration (ALTER TABLE ... NO FORCE ROW LEVEL SECURITY when the admin user owns the table), or migrate with a user that bypasses row-level security.", map[string]any{"tables": rls})
	} else {
		p.ok("No row-level security hides rows from the migration.", map[string]any{})
	}

	// REPLICA IDENTITY FULL: the engine finds the old row by comparing every
	// column. Types without an equality operator (json, xml, the geometric
	// types) make that comparison fail, so updates and deletes cannot apply.
	p = r.probe(ctx, "identity_full_types", "", d.Source)
	noEq, noEqN := p.sample(sc, `SELECT DISTINCT format('%I.%I', n.nspname, c.relname)||' ('||a.attname||' '||format_type(a.atttypid, a.atttypmod)||')'
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
		JOIN pg_type ty ON ty.oid=a.atttypid LEFT JOIN pg_type el ON el.oid=ty.typelem AND ty.typlen=-1 AND ty.typelem<>0
		WHERE c.relkind = 'r' AND c.relreplident='f' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_temp%'
		AND (ty.typname IN ('json','xml','point','line','lseg','box','path','polygon','circle') OR el.typname IN ('json','xml','point','line','lseg','box','path','polygon','circle'))`, 100)
	if noEqN > 0 {
		p.emit(check.Blocker, true, fmt.Sprintf("%d column(s) in tables with REPLICA IDENTITY FULL have a type with no equality operator (for example %s), so the engine cannot find the old row: updates and deletes on these tables would fail to apply.", noEqN, noEq[0]),
			"Give these tables a primary key (or a unique index used with REPLICA IDENTITY USING INDEX), or change json columns to jsonb, then run preflight again.", map[string]any{"columns": noEq})
	} else {
		p.ok("Tables with full replica identity have only comparable columns.", map[string]any{})
	}
	p = r.probe(ctx, "identity_full_duplicates", "", d.Source)
	fullNoKey, fullNoKeyN := p.sample(sc, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind = 'r' AND c.relreplident='f' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_temp%'
		AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid=c.oid AND i.indisunique AND i.indpred IS NULL)`, 100)
	if fullNoKeyN > 0 {
		p.emit(check.Warning, false, fmt.Sprintf("%d table(s) with REPLICA IDENTITY FULL have no unique key (for example %s). If two rows are exactly identical, an update or delete of one of them is applied to both on the target; verification would then end NO-GO.", fullNoKeyN, fullNoKey[0]),
			"Add a primary key if the table can hold identical rows.", map[string]any{"tables": fullNoKey})
	} else {
		p.ok("Every table with full replica identity has a unique key.", map[string]any{})
	}

	simple := []struct{ id, q, msg, fix, names string }{
		{"large_objects", `SELECT count(*) FROM pg_largeobject_metadata`, "%d large object(s) are copied once during the base copy and not streamed afterwards; verification compares them, so any change during the migration ends NO-GO.", "Do not change large objects while the migration runs.", ""},
		{"unlogged_tables", `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND c.relpersistence='u' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_temp%'`, "%d unlogged table(s) are copied once and not streamed; changes to them after the base copy are lost.", "Make them logged before starting, or accept that their later changes are not migrated.", ""},
		{"materialized_views", `SELECT count(*) FROM pg_matviews`, "%d materialized view(s) are created but must be refreshed on the target after cutover.", "Refresh them after cutover (it is in the post-cutover checklist).", `SELECT format('%I.%I', schemaname, matviewname) FROM pg_matviews`},
		{"foreign_servers", `SELECT count(*) FROM pg_foreign_server`, "%d foreign server(s) need their user mappings and credentials re-created on the target.", "Re-create user mappings on the target after cutover (it is in the post-cutover checklist).", `SELECT srvname::text FROM pg_foreign_server`},
		{"event_triggers", `SELECT count(*) FROM pg_event_trigger`, "%d event trigger(s) are not copied by the engine.", "Re-create them on the target after cutover (it is in the post-cutover checklist).", `SELECT evtname::text FROM pg_event_trigger`},
		{"pg_cron", `SELECT count(*) FROM pg_extension WHERE extname='pg_cron'`, "pg_cron is installed; its jobs live in the cron schema and need review before cutover%.0d.", "Recreate or disable scheduled jobs on the target after cutover.", ""},
		{"pg_partman", `SELECT count(*) FROM pg_extension WHERE extname='pg_partman'`, "pg_partman is installed; partition maintenance must run on only one side at a time%.0d.", "Pause partition maintenance on the source after cutover.", ""},
	}
	for _, s := range simple {
		p = r.probe(ctx, s.id, "", d.Source)
		var n int64
		p.row(sc, s.q).Scan(&n)
		if n > 0 {
			ev := map[string]any{"count": n}
			if s.names != "" {
				names, _ := p.sample(sc, s.names, 100)
				ev["names"] = names
			}
			p.emit(check.Warning, false, fmt.Sprintf(s.msg, n), s.fix, ev)
		} else {
			p.ok("None.", map[string]any{"count": 0})
		}
	}

	// Target side. The source's encoding and locale, and whether the target
	// database exists, feed three checks; an error in either query fails all three.
	src := r.probe(ctx, "dst_database", "", d.Source)
	var srcEnc, srcCollate, srcCtype string
	src.row(sc, `SELECT pg_encoding_to_char(encoding), datcollate, datctype FROM pg_database WHERE datname=current_database()`).Scan(&srcEnc, &srcCollate, &srcCtype)
	var dstEnc, dstCollate, dstCtype, dstCollVer, srcCollVer string
	exists := src.rowMaybe(r.dst, `SELECT pg_encoding_to_char(encoding), datcollate, datctype FROM pg_database WHERE datname=$1`, []any{d.Target}, &dstEnc, &dstCollate, &dstCtype)
	baseErr := src.err
	var createdb, superT bool
	src.row(r.dst, `SELECT rolcreatedb, rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&createdb, &superT)
	ev = map[string]any{"exists": exists, "can_create": createdb || superT, "create_target_databases": e.Settings.Bool("create_target_databases")}
	switch {
	case exists:
		src.ok(fmt.Sprintf("Target database %s exists.", d.Target), ev)
	case (createdb || superT) && e.Settings.Bool("create_target_databases"):
		src.ok(fmt.Sprintf("Target database %s will be created with the source's encoding and locale.", d.Target), ev)
	default:
		src.emit(check.Blocker, true, fmt.Sprintf("Target database %s does not exist and Upwell will not create it.", d.Target), "Create it on the target, or allow Upwell to create target databases (and give the user CREATEDB).", ev)
	}
	p = r.probe(ctx, "encoding", "", d.Source)
	p.fail(baseErr)
	if exists && dstEnc != srcEnc {
		p.emit(check.Blocker, true, fmt.Sprintf("Encodings differ: source %s, target %s.", srcEnc, dstEnc), "Recreate the target database with encoding "+srcEnc+".", map[string]any{"source": srcEnc, "target": dstEnc})
	} else {
		p.ok("Encoding "+srcEnc+" on both sides.", map[string]any{"source": srcEnc, "target": firstNonEmpty(dstEnc, srcEnc)})
	}
	p = r.probe(ctx, "collation", "", d.Source)
	p.fail(baseErr)
	if exists && p.err == nil {
		p.row(sc, `SELECT COALESCE((SELECT datcollversion FROM pg_database WHERE datname=current_database()),'')`).Scan(&srcCollVer)
		if tc, err := pg.Connect(ctx, e.Target.WithDB(d.Target), 30*time.Second); err != nil {
			p.fail(err)
			p.emit(check.Blocker, false, "", "", map[string]any{"source": srcCollate})
		} else {
			p.row(tc, `SELECT COALESCE((SELECT datcollversion FROM pg_database WHERE datname=current_database()),'')`).Scan(&dstCollVer)
			probeS := sortProbe(p, sc)
			probeT := sortProbe(p, tc)
			tc.Close(ctx)
			ev := map[string]any{"source": srcCollate, "target": dstCollate, "source_version": srcCollVer, "target_version": dstCollVer, "sort_match": probeS == probeT}
			if NormalizeLocale(srcCollate) != NormalizeLocale(dstCollate) || probeS != probeT || (srcCollVer != "" && dstCollVer != "" && srcCollVer != dstCollVer) {
				p.emit(check.Blocker, false, fmt.Sprintf("Collations differ (source %s, target %s), so text indexes may sort differently.", srcCollate, dstCollate), "Recreate the target database with the source's locale, or accept the risk and reindex text indexes after cutover.", ev)
			} else {
				p.ok("Same collation ("+srcCollate+") and sort order.", ev)
			}
		}
	} else if p.err != nil {
		p.emit(check.Blocker, false, "", "", map[string]any{"source": srcCollate})
	} else {
		p.ok("The target database will be created with collation "+srcCollate+".", map[string]any{"source": srcCollate})
	}

	var tc *pgx.Conn
	var tcErr error
	if exists {
		tc, tcErr = pg.Connect(ctx, e.Target.WithDB(d.Target), 30*time.Second)
	}
	if tc != nil {
		defer tc.Close(ctx)
	}
	p = r.probe(ctx, "dst_conflicts", "", d.Source)
	p.fail(tcErr)
	objQ := `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p','S','v','m','f') AND n.nspname NOT IN ('pg_catalog','information_schema','upwell') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp%' ORDER BY 1`
	srcObjs := p.strings(sc, objQ)
	var dstObjs []string
	if tc != nil {
		dstObjs = p.strings(tc, objQ)
	}
	srcSet := map[string]bool{}
	for _, o := range srcObjs {
		srcSet[o] = true
	}
	var conflicts, extra []string
	for _, o := range dstObjs {
		if srcSet[o] {
			conflicts = append(conflicts, o)
		} else {
			extra = append(extra, o)
		}
	}
	extraP := r.probe(ctx, "dst_extra_objects", "", d.Source)
	extraP.fail(p.err)
	if len(conflicts) > 0 && !e.Resuming {
		p.emit(check.Blocker, true, fmt.Sprintf("%d object(s) already exist on both sides, for example %s; the copy would fail or mix data.", len(conflicts), conflicts[0]), "Drop them from the target database, or choose a different target database name.", map[string]any{"conflicts": limitList(conflicts, 50)})
	} else {
		p.ok("No object exists on both sides.", map[string]any{"conflicts": []string{}})
	}
	if len(extra) > 0 {
		extraP.emit(check.Warning, false, fmt.Sprintf("%d object(s) exist only on the target (for example %s); Upwell leaves them untouched.", len(extra), extra[0]), "No action needed unless they are left over from an earlier attempt.", map[string]any{"objects": limitList(extra, 50)})
	} else {
		extraP.ok("No objects exist only on the target.", map[string]any{"objects": []string{}})
	}
	p = r.probe(ctx, "extensions", "", d.Source)
	srcExt := p.strings(sc, `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql' ORDER BY 1`)
	avail := map[string]bool{}
	for _, n := range p.strings(r.dst, `SELECT name FROM pg_available_extensions`) {
		avail[n] = true
	}
	var missingExt []string
	for _, x := range srcExt {
		if !avail[x] {
			missingExt = append(missingExt, x)
		}
	}
	if len(missingExt) > 0 {
		p.emit(check.Blocker, true, "These source extensions are not available on the target: "+strings.Join(missingExt, ", ")+".", "Ask for them to be enabled on the Advanced cluster, or remove them from the source.", map[string]any{"missing": missingExt, "source": srcExt})
	} else {
		p.ok(fmt.Sprintf("All %d source extension(s) are available on the target.", len(srcExt)), map[string]any{"source": srcExt})
	}

	for _, x := range []struct {
		id, what, q string
		c           *pgx.Conn
		arg         string
	}{
		{"slot_exists", "replication slot " + d.SlotName, `SELECT count(*) FROM pg_replication_slots WHERE slot_name=$1`, r.src, d.SlotName},
		{"origin_exists", "replication origin " + d.Origin, `SELECT count(*) FROM pg_replication_origin WHERE roname=$1`, r.dst, d.Origin},
		{"publication_exists", "publication " + d.SlotName, `SELECT count(*) FROM pg_publication WHERE pubname=$1`, sc, d.SlotName},
	} {
		p = r.probe(ctx, x.id, "", d.Source)
		var n int
		p.row(x.c, x.q, x.arg).Scan(&n)
		if n > 0 && !e.Resuming {
			p.emit(check.Blocker, true, "A leftover "+x.what+" from an earlier run exists.", "Run Clean up on the earlier migration, or drop it by hand.", map[string]any{"name": x.what})
		} else {
			p.ok("No leftover "+strings.SplitN(x.what, " ", 3)[0]+" "+strings.SplitN(x.what, " ", 3)[1]+".", map[string]any{"name": x.what})
		}
	}

	t = time.Now()
	ap, apErr := TargetApplyPrivileges(ctx, firstConn(tc, r.dst), r.dstVer)
	switch {
	case apErr != nil:
		r.couldNotCheck("target_apply_privileges", "", d.Source, apErr, ap.Evidence(), t)
	case !ap.OK:
		r.emit("target_apply_privileges", "", d.Source, check.Blocker, true, "The target user cannot "+strings.Join(ap.Missing, " or ")+". Without this, pgcopydb 0.18 applies no changes and still reports success (Phase 0 finding F2).",
			ApplyPrivilegeRemediation(e.Target.User, r.dstVer), ap.EvidenceFor(e.Target.User, r.dstVer), t)
	default:
		ok(r, "target_apply_privileges", "", d.Source, "The target user can manage replication origins and set session_replication_role.", ap.EvidenceFor(e.Target.User, r.dstVer), t)
	}

	p = r.probe(ctx, "heartbeat_possible", "", d.Source)
	var canCreate, hbExists bool
	p.row(sc, `SELECT has_database_privilege(current_database(),'CREATE')`).Scan(&canCreate)
	p.row(sc, `SELECT to_regclass('upwell.heartbeat') IS NOT NULL`).Scan(&hbExists)
	hbOn := e.Settings.Bool("heartbeat")
	ev = map[string]any{"heartbeat": hbOn, "can_create": canCreate, "exists": hbExists}
	switch {
	case !hbOn:
		p.emit(check.Warning, false, "The heartbeat is off, so GO will not require proof that the last writes arrived.", "Turn the heartbeat on in Options unless the customer forbids writes to the source.", ev)
	case canCreate || hbExists:
		p.ok("Upwell can create its heartbeat table (schema upwell) on the source.", ev)
	default:
		p.emit(check.Blocker, true, e.Source.User+" cannot create the upwell schema for the heartbeat table, and GO requires the final heartbeat.", "Grant CREATE on the database to the admin user, or turn the heartbeat off in Options (recorded as an accepted risk).", ev)
	}

	p = r.probe(ctx, "write_freeze_available", "", d.Source)
	var owner, sigBackend bool
	p.row(sc, `SELECT pg_has_role((SELECT datdba FROM pg_database WHERE datname=current_database()),'MEMBER'), pg_has_role('pg_signal_backend','MEMBER')`).Scan(&owner, &sigBackend)
	ev = map[string]any{"owner": owner, "pg_signal_backend": sigBackend}
	msg := "Write freeze is coming soon; this database would support it."
	if !owner || !sigBackend {
		msg = "Write freeze is coming soon; this database would not support it (the admin does not own it or cannot end other sessions)."
	}
	p.emit(check.Info, false, msg, "", ev)
}

func firstConn(a, b *pgx.Conn) *pgx.Conn {
	if a != nil {
		return a
	}
	return b
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func limitList(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sortProbe(p *probe, c *pgx.Conn) string {
	var s string
	p.row(c, `SELECT string_agg(x, '|' ORDER BY x) FROM (VALUES ('a'),('B'),('_c'),('ä'),('Z'),('1'),('é'),('e')) v(x)`).Scan(&s)
	return s
}

// ApplyPrivileges is the target_apply_privileges evidence.
type ApplyPrivileges struct {
	OK        bool
	Super     bool
	Functions map[string]bool
	SetSRR    *bool
	Missing   []string
}

// Evidence returns the result evidence map.
func (a ApplyPrivileges) Evidence() map[string]any {
	return map[string]any{"superuser": a.Super, "functions": a.Functions, "set_session_replication_role": a.SetSRR, "missing": a.Missing}
}

var originFuncs = []string{
	"pg_replication_origin_create(text)", "pg_replication_origin_drop(text)", "pg_replication_origin_oid(text)",
	"pg_replication_origin_session_setup(text)", "pg_replication_origin_session_reset()", "pg_replication_origin_session_is_setup()",
	"pg_replication_origin_session_progress(boolean)", "pg_replication_origin_xact_setup(pg_lsn,timestamp with time zone)",
	"pg_replication_origin_xact_reset()", "pg_replication_origin_advance(text,pg_lsn)", "pg_replication_origin_progress(text,boolean)",
}

// TargetApplyPrivileges checks what pgcopydb's apply needs on the target
// (F1). Any failed query is returned as an error: the caller must treat the
// privileges as unconfirmed, never as present or missing.
func TargetApplyPrivileges(ctx context.Context, c *pgx.Conn, verNum int) (ApplyPrivileges, error) {
	a := ApplyPrivileges{Functions: map[string]bool{}}
	if err := pg.QueryRow(ctx, c, `SELECT rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&a.Super); err != nil {
		return a, err
	}
	missingFn := false
	for _, f := range originFuncs {
		var okf bool
		if err := pg.QueryRow(ctx, c, `SELECT has_function_privilege($1,'EXECUTE')`, f).Scan(&okf); err != nil {
			return a, err
		}
		a.Functions[f] = okf
		if !okf {
			missingFn = true
		}
	}
	if missingFn && !a.Super {
		a.Missing = append(a.Missing, "execute the pg_replication_origin_* functions")
	}
	if verNum >= 150000 {
		var s bool
		if err := pg.QueryRow(ctx, c, `SELECT has_parameter_privilege('session_replication_role','SET')`).Scan(&s); err != nil {
			return a, err
		}
		a.SetSRR = &s
		if !s && !a.Super {
			a.Missing = append(a.Missing, "SET session_replication_role")
		}
	} else if !a.Super {
		a.Missing = append(a.Missing, "SET session_replication_role (before PostgreSQL 15, only superusers can; upgrade the target to PostgreSQL 15 or later)")
	}
	a.OK = len(a.Missing) == 0
	return a, nil
}

func replicationProbe(ctx context.Context, c pg.Conn) error {
	cfg, err := pgx.ParseConfig(c.URI())
	if err != nil {
		return err
	}
	cfg.Password = c.Password
	cfg.RuntimeParams["replication"] = "database"
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, cfg)
	if err != nil {
		return pg.Explain(err)
	}
	defer conn.Close(ctx)
	res := conn.PgConn().Exec(cctx, "IDENTIFY_SYSTEM")
	_, err = res.ReadAll()
	return err
}

// samplingChecks measure the source WAL rate and estimate throughput, then
// run the checks that depend on it.
func (r *Runner) samplingChecks(ctx context.Context) {
	e := r.Env
	secs := e.Settings.Int("wal_sample_seconds")
	if secs <= 0 {
		secs = 60
	}
	// One sample feeds several checks: if any of its queries fails, every
	// check that depends on it says it could not check.
	sample := r.probe(ctx, "throughput_sample", "", "")
	var a, b string
	// Commit rate per database, sampled over the same window (F11: the engine
	// applies a limited number of transactions a second).
	commits := func() map[string]int64 {
		out := map[string]int64{}
		rows, err := pg.Query(ctx, r.src, `SELECT datname, xact_commit FROM pg_stat_database WHERE datname IS NOT NULL`)
		if err != nil {
			sample.fail(err)
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			var c int64
			sample.fail(rows.Scan(&n, &c))
			out[n] = c
		}
		sample.fail(rows.Err())
		return out
	}
	c0 := commits()
	sample.row(r.src, `SELECT pg_current_wal_lsn()::text`).Scan(&a)
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(secs) * time.Second):
	}
	sample.row(r.src, `SELECT pg_current_wal_lsn()::text`).Scan(&b)
	c1 := commits()
	r.walRateBps = float64(pg.LSNDiff(b, a)) / float64(secs)
	mibs := e.Settings.Float("assumed_throughput_mibs")
	if mibs <= 0 {
		mibs = 102
	}
	copySecs := float64(r.totalSourceBytes) / (mibs * 1024 * 1024)
	ev := map[string]any{"_wal_rate_bytes_per_s": r.walRateBps, "_sample_seconds": secs, "assumed_mibs": mibs, "_estimated_copy_seconds": copySecs}
	sample.emit(check.Info, false, fmt.Sprintf("At the assumed %.0f MiB/s the base copy of %s takes about %s; the source wrote %s of WAL per second during a %d s sample.",
		mibs, humanBytes(float64(r.totalSourceBytes)), humanDuration(copySecs), humanBytes(r.walRateBps), secs), "", ev)
	p := r.probe(ctx, "cdc_throughput", "", "")
	p.fail(sample.err)
	limit := mibs * 1024 * 1024 * 0.5
	if r.walRateBps > limit {
		p.emit(check.Warning, false, fmt.Sprintf("The source writes %s of WAL per second, more than half the assumed copy rate; streaming may not keep up and the backlog could grow until the slot is lost.", humanBytes(r.walRateBps)),
			"Calibrate with a test migration on this droplet, or migrate during a quieter period.", ev)
	} else {
		p.ok(fmt.Sprintf("The source writes %s of WAL per second, well within the assumed rate.", humanBytes(r.walRateBps)), ev)
	}
	// Transactions per second per database against the engine's apply rate.
	applyTPS := e.Settings.Float("assumed_apply_tps")
	if applyTPS <= 0 {
		applyTPS = 5
	}
	for _, d := range e.Databases {
		p := r.probe(ctx, "apply_rate", "", d.Source)
		p.fail(sample.err)
		// Upwell's own sampling connections commit too; one a second is noise.
		tps := float64(c1[d.Source]-c0[d.Source]) / float64(secs)
		dev := map[string]any{"_commits_per_s": tps, "assumed_apply_tps": applyTPS, "_sample_seconds": secs}
		if tps > applyTPS*0.8 {
			p.emit(check.Warning, false, fmt.Sprintf("This database commits %.1f transactions a second; pgcopydb 0.18 applied about %.0f a second in testing, so the target could fall further and further behind.", tps, applyTPS),
				"Measure on this droplet with a test migration first (the In sync state and the cutover lag gate show whether it keeps up), or migrate during a quieter period.", dev)
		} else {
			p.ok(fmt.Sprintf("This database commits %.1f transactions a second, within the assumed apply rate of %.0f.", tps, applyTPS), dev)
		}
	}
	p = r.probe(ctx, "slot_wal_cap", "", "")
	p.fail(sample.err)
	var capStr string
	p.row(r.src, `SHOW max_slot_wal_keep_size`).Scan(&capStr)
	retained := r.walRateBps * copySecs * 1.5
	ev = map[string]any{"max_slot_wal_keep_size": capStr, "_estimated_retained_bytes": retained}
	capBytes := parsePGSize(capStr)
	switch {
	case capBytes < 0:
		p.emit(check.Warning, false, fmt.Sprintf("max_slot_wal_keep_size is unlimited: slots can retain WAL until the source disk fills (estimated %s during the copy).", humanBytes(retained)), "Watch the source's disk during the base copy.", ev)
	case float64(capBytes) < retained:
		p.emit(check.Warning, false, fmt.Sprintf("max_slot_wal_keep_size is %s but the slots may retain about %s during the copy; the slot would be invalidated and the database restarted.", capStr, humanBytes(retained)), "Raise max_slot_wal_keep_size, or migrate during a quieter period.", ev)
	default:
		p.ok(fmt.Sprintf("max_slot_wal_keep_size %s covers the estimated %s.", capStr, humanBytes(retained)), ev)
	}
	p = r.probe(ctx, "staging_space", "", "")
	p.fail(sample.err)
	staging := retained*e.Settings.Float("staging_factor") + float64(len(e.Databases))*2*(1<<30)
	var st syscall.Statfs_t
	var free float64
	if err := syscall.Statfs(e.WorkDir, &st); err != nil {
		p.fail(fmt.Errorf("reading the work volume's free space: %w", err))
	} else {
		free = float64(st.Bavail) * float64(st.Bsize)
	}
	ev = map[string]any{"_free_bytes": free, "_estimate_bytes": staging, "databases": len(e.Databases)}
	switch {
	case free < staging:
		p.emit(check.Blocker, false, fmt.Sprintf("The work volume has %s free; change staging may need %s (including about 2 GB per database for the file being written).", humanBytes(free), humanBytes(staging)), "Attach a larger volume, or accept the risk; the disk guard stops a database at 95%.", ev)
	case free < 2*staging:
		p.emit(check.Warning, false, fmt.Sprintf("The work volume has %s free, less than twice the %s staging estimate.", humanBytes(free), humanBytes(staging)), "Consider a larger volume.", ev)
	default:
		p.ok(fmt.Sprintf("%s free for an estimated %s of staging.", humanBytes(free), humanBytes(staging)), ev)
	}
}

func parsePGSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "-1" {
		return -1
	}
	m := regexp.MustCompile(`^(\d+)\s*([kMGT]?B)?$`).FindStringSubmatch(s)
	if m == nil {
		return -1
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	switch m[2] {
	case "kB":
		return n << 10
	case "MB", "":
		return n << 20
	case "GB":
		return n << 30
	case "TB":
		return n << 40
	}
	return n
}

func humanDuration(s float64) string {
	d := time.Duration(s * float64(time.Second))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

// sessionCheck: the engine sets session_replication_role and the replication
// origin once per session and holds its snapshot in one transaction. Behind
// a pooler in transaction mode those would land on other server connections,
// and changes would be applied without them (or not at all).
func (r *Runner) sessionCheck(ctx context.Context, side string, conn *pgx.Conn, at pg.Conn, t time.Time) {
	ss, err := pg.ProbeSession(ctx, conn, at)
	ev := map[string]any{"client_addr": ss.ClientAddr, "proxy_likely": ss.ProxyLikely, "backend_pids": ss.PIDs}
	switch {
	case err != nil:
		ev["error"] = err.Error()
		r.emit("session_pinning", side, "", check.Blocker, true, fmt.Sprintf("Upwell could not confirm that %s sessions keep their settings: %v", side, err), "Connect directly to PostgreSQL, not through a connection pooler.", ev, t)
	case !ss.Pinned:
		r.emit("session_pinning", side, "", check.Blocker, true, fmt.Sprintf("Consecutive transactions on one %s connection reached different server sessions (backend PIDs %v): a connection pooler in transaction mode sits in between, and the engine's session settings would be lost.", side, ss.PIDs),
			"Use the cluster's direct port, or a pooler in session mode.", ev, t)
	case ss.ProxyLikely:
		r.emit("session_pinning", side, "", check.Warning, false, fmt.Sprintf("PostgreSQL sees the %s connection coming from %s, not from this droplet: a pooler or proxy probably sits in between. Sessions kept their settings in this test, which is what the engine needs; a pooler in transaction mode under load would not.", side, orUnknown(ss.ClientAddr)),
			"Prefer the cluster's direct port if it has one; otherwise make sure the pooler runs in session mode.", ev, t)
	default:
		ok(r, "session_pinning", side, "", "Sessions keep their settings (no transaction pooling).", ev, t)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown address"
	}
	return s
}

// settingMillis parses a time setting as stored by ALTER ... SET ("5min",
// "1h", "30000"); a bare number is milliseconds.
func settingMillis(v string) int64 {
	v = strings.Trim(strings.TrimSpace(v), "'")
	units := []struct {
		suf string
		ms  float64
	}{{"ms", 1}, {"min", 60000}, {"s", 1000}, {"h", 3600000}, {"d", 86400000}}
	for _, u := range units {
		if strings.HasSuffix(v, u.suf) {
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, u.suf)), 64)
			if err != nil {
				return 0
			}
			return int64(f * u.ms)
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

var engineVersionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

// EngineMajorMinor reads the leading major.minor of a pgcopydb version as
// packages report it: "0.18", "0.18.1", or the PGDG package's
// "0.18-1.pgdg24.04+1" (the Debian revision after the dash is not part of
// the release).
func EngineMajorMinor(v string) (major, minor int, ok bool) {
	m := engineVersionRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}
