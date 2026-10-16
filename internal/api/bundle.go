package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/report"
	"github.com/pagombin/upwell/internal/store"
)

// Support bundle (overnight item 6, spec line 459): everything support needs
// to diagnose a migration, with secrets removed, previewed before download.

type bundleFile struct {
	Name       string `json:"name"`
	Size       int    `json:"size"`
	Redactions int    `json:"redactions"`
	Binary     bool   `json:"binary,omitempty"` // packet captures: headers only, not redacted
	data       []byte
}

// RedactionRules are what the bundle removes, in order; the preview lists them.
var RedactionRules = []string{
	"every stored password of the migration's connections, wherever it appears",
	"values of password, secret, token and key fields (password=..., \"password\": \"...\")",
	"the password in postgres:// URIs (user:password@host)",
	"Upwell API tokens (upw_...) and bearer tokens",
	"the setup token",
}

var (
	reKV     = regexp.MustCompile(`(?i)((?:pass(?:word|wd)?|pwd|secret|token|api_?key|private_?key)["']?\s*[=:]\s*["']?)([^\s"',&;]+)`)
	reURI    = regexp.MustCompile(`(postgres(?:ql)?://[^:/@\s"']+):[^@\s"']+@`)
	reToken  = regexp.MustCompile(`upw_[0-9a-fA-F]{6,}`)
	reBearer = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
)

// redact removes secrets from b and counts what it removed.
func redact(b []byte, literals []string) ([]byte, int) {
	n := 0
	s := string(b)
	for _, l := range literals {
		if len(l) >= 4 {
			n += strings.Count(s, l)
			s = strings.ReplaceAll(s, l, "[redacted]")
		}
	}
	for _, re := range []*regexp.Regexp{reURI, reKV, reBearer} {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if strings.Contains(m, "[redacted]") {
				return m
			}
			n++
			return sub[1] + "[redacted]" + map[bool]string{true: "@", false: ""}[re == reURI]
		})
	}
	s = reToken.ReplaceAllStringFunc(s, func(string) string { n++; return "upw_[redacted]" })
	return []byte(s), n
}

func jsonBytes(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

func tailFile(path string, max int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	if st.Size() > max {
		f.Seek(st.Size()-max, io.SeekStart) //nolint:errcheck
	}
	b, _ := io.ReadAll(io.LimitReader(f, max))
	return b
}

func (s *Server) buildBundle(ctx context.Context, id, netMode string) (orch.Migration, []bundleFile, error) {
	m, err := s.Orch.GetMigration(ctx, id)
	if err != nil {
		return m, nil, err
	}
	var files []bundleFile
	add := func(name string, data []byte) { files = append(files, bundleFile{Name: name, data: data}) }
	var conns []any
	for _, cid := range []string{m.SourceConnID, m.TargetConnID} {
		if c, err := s.Orch.GetConnection(ctx, cid); err == nil {
			conns = append(conns, c) // the password is never in the record (json:"-")
		}
	}
	dbs, _ := s.Orch.ListDatabases(ctx, m.ID)
	add("migration.json", jsonBytes(map[string]any{"migration": m, "connections": conns, "settings_effective": s.Orch.Settings(ctx, m)}))
	add("databases.json", jsonBytes(dbs))
	run, results, _ := s.Orch.LatestPreflight(ctx, m.ID)
	add("preflight.json", jsonBytes(map[string]any{"run": run, "results": results}))
	vrun, ver, _ := s.Orch.Verification(ctx, m.ID)
	add("verification.json", jsonBytes(map[string]any{"run_id": vrun, "results": ver}))
	ops, _ := s.Orch.ListOperations(ctx, m.ID, 200)
	add("operations.json", jsonBytes(ops))
	var events []map[string]any
	if rows, err := s.Store.DB.QueryContext(ctx, `SELECT ts, COALESCE(database,''), type, severity, message FROM events WHERE migration_id=? ORDER BY ts`, m.ID); err == nil {
		for rows.Next() {
			var ts int64
			var db, typ, sev, msg string
			rows.Scan(&ts, &db, &typ, &sev, &msg)
			events = append(events, map[string]any{"ts": ts, "database": db, "type": typ, "severity": sev, "message": msg})
		}
		rows.Close()
	}
	add("events.json", jsonBytes(events))
	alerts, _ := s.Orch.ListAlerts(ctx, "", 1000)
	var mine []orch.Alert
	for _, a := range alerts {
		if a.MigrationID == m.ID || a.MigrationID == "" {
			mine = append(mine, a)
		}
	}
	add("alerts.json", jsonBytes(mine))
	now := time.Now().UnixMilli()
	series, _ := s.Orch.Metrics(ctx, m.ID, nil, now-24*3600*1000, now)
	add("metrics-24h.json", jsonBytes(series))
	if rep, err := report.Build(ctx, s.Orch, s.Store, m.ID, s.Version); err == nil {
		add("report.md", rep.Markdown())
	}
	var logs bytes.Buffer
	if rows, err := s.Store.DB.QueryContext(ctx, `SELECT ts, level, component, COALESCE(database,''), COALESCE(op,''), msg FROM logs WHERE migration_id=? ORDER BY seq DESC LIMIT 50000`, m.ID); err == nil {
		var lines []string
		for rows.Next() {
			var ts int64
			var lvl, comp, db, op, msg string
			rows.Scan(&ts, &lvl, &comp, &db, &op, &msg)
			b, _ := json.Marshal(map[string]any{"ts": ts, "level": lvl, "component": comp, "database": db, "op": op, "msg": msg})
			lines = append(lines, string(b))
		}
		rows.Close()
		for i := len(lines) - 1; i >= 0; i-- {
			logs.WriteString(lines[i] + "\n")
		}
	}
	add("logs/upwell.jsonl", logs.Bytes())
	for _, d := range dbs {
		if !d.Include {
			continue
		}
		if b := tailFile(s.Orch.EngineLogPath(d), 2<<20); len(b) > 0 {
			add("logs/engine-"+d.SourceName+".log", b)
		}
	}
	// D97, D101: network diagnostics, limited to this migration's failures.
	files = append(files, s.bundleNetwork(ctx, m, events, netMode)...)
	engVer, _ := s.Orch.Engine().Version(ctx)
	pgDump := ""
	if out, err := exec.CommandContext(ctx, filepath.Join(s.Cfg.Engine.PgBinDir, "pg_dump"), "--version").Output(); err == nil {
		pgDump = strings.TrimSpace(string(out))
	}
	add("versions.json", jsonBytes(map[string]any{"upwell": s.Version, "engine": engVer, "pg_dump": pgDump, "go": runtime.Version()}))
	add("host.json", jsonBytes(hostFacts(s.Cfg.DataDir)))

	literals := s.Orch.MigrationSecrets(ctx, m)
	if b, err := os.ReadFile(s.Cfg.SetupToken); err == nil {
		literals = append(literals, strings.TrimSpace(string(b)))
	}
	sort.Slice(literals, func(i, j int) bool { return len(literals[i]) > len(literals[j]) })
	for i := range files {
		if !files[i].Binary {
			files[i].data, files[i].Redactions = redact(files[i].data, literals)
		}
		files[i].Size = len(files[i].data)
	}
	manifest := map[string]any{"generated_at": store.Now(), "upwell": s.Version, "migration": m.ShortID, "name": m.Name, "redaction_rules": RedactionRules, "files": files}
	files = append([]bundleFile{{Name: "manifest.json", data: jsonBytes(manifest)}}, files...)
	files[0].Size = len(files[0].data)
	return m, files, nil
}

func hostFacts(dataDir string) map[string]any {
	h := map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU()}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "PRETTY_NAME=") {
				h["os_release"] = strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), `"`)
			}
		}
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h["kernel"] = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "MemTotal:") {
				h["memory"] = strings.TrimSpace(strings.TrimPrefix(l, "MemTotal:"))
			}
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		var up float64
		if _, err := fmt.Sscanf(string(b), "%f", &up); err == nil {
			h["uptime_seconds"] = int64(up)
		}
	}
	var st syscall.Statfs_t
	if syscall.Statfs(dataDir, &st) == nil {
		h["data_volume_free_bytes"] = int64(st.Bavail) * int64(st.Bsize)
		h["data_volume_size_bytes"] = int64(st.Blocks) * int64(st.Bsize)
	}
	return h
}

func (s *Server) bundlePreview(w http.ResponseWriter, r *http.Request) error {
	_, files, err := s.buildBundle(r.Context(), r.PathValue("id"), r.URL.Query().Get("network"))
	if err != nil {
		return err
	}
	total := 0
	for _, f := range files {
		total += f.Size
	}
	return writeJSON(w, 200, map[string]any{"files": files, "total_size": total, "redaction_rules": RedactionRules})
}

func (s *Server) bundleDownload(w http.ResponseWriter, r *http.Request) error {
	m, files, err := s.buildBundle(r.Context(), r.PathValue("id"), r.URL.Query().Get("network"))
	if err != nil {
		return err
	}
	// Streamed: the archive is compressed straight into the response
	// instead of being built in memory first (D101).
	prefix := "upwell-support-" + m.ShortID + "-" + time.Now().UTC().Format("20060102T150405Z")
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+prefix+`.tar.gz"`)
	w.WriteHeader(200)
	cw := &countWriter{w: w}
	gz := gzip.NewWriter(cw)
	tw := tar.NewWriter(gz)
	werr := func() error {
		for _, f := range files {
			if err := tw.WriteHeader(&tar.Header{Name: prefix + "/" + f.Name, Mode: 0o600, Size: int64(len(f.data)), ModTime: time.Now()}); err != nil {
				return err
			}
			if _, err := tw.Write(f.data); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}()
	red := 0
	for _, f := range files {
		red += f.Redactions
	}
	after := map[string]any{"files": len(files), "bytes": cw.n, "redactions": red, "network": r.URL.Query().Get("network")}
	if werr != nil {
		after["error"] = werr.Error()
	}
	s.Audit.Append(r.Context(), actor(r), "migration.support_bundle", m.ID, nil, after)
	if werr != nil {
		s.Log.Logf("warn", "api", m.ID, "support bundle download ended early: %v", werr)
	}
	return nil // the status is sent; an error now cannot be reported to the client
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
