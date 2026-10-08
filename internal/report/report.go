// Package report renders a migration's shareable record as HTML or Markdown.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/store"
)

// Attempt is one engine launch.
type Attempt struct {
	Database  string `json:"database"`
	Number    int    `json:"number"`
	Kind      string `json:"kind"`
	Unit      string `json:"unit"`
	Command   string `json:"command"`
	StartedAt int64  `json:"started_at"`
	EndedAt   *int64 `json:"ended_at,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Reason    string `json:"end_reason,omitempty"`
}

// Report is everything the report shows.
type Report struct {
	GeneratedAt   int64                  `json:"generated_at"`
	Migration     orch.Migration         `json:"migration"`
	Permission    *orch.Permission       `json:"permission"`
	Source        *orch.Connection       `json:"source"`
	Target        *orch.Connection       `json:"target"`
	Databases     []orch.Database        `json:"databases"`
	Attempts      []Attempt              `json:"attempts"`
	Preflight     []check.Result         `json:"preflight"`
	Accepted      []map[string]any       `json:"accepted_risks"`
	Verification  []orch.VerificationRow `json:"verification"`
	Events        []map[string]any       `json:"events"`
	EngineVersion string                 `json:"engine_version"`
	AppVersion    string                 `json:"app_version"`
}

// Build gathers the report.
func Build(ctx context.Context, o *orch.Orchestrator, st *store.Store, id, appVersion string) (*Report, error) {
	m, err := o.GetMigration(ctx, id)
	if err != nil {
		return nil, err
	}
	r := &Report{GeneratedAt: store.Now(), Migration: m, AppVersion: appVersion}
	r.Permission, _ = o.GetPermission(ctx, m.ID)
	if c, err := o.GetConnection(ctx, m.SourceConnID); err == nil {
		r.Source = &c
	}
	if c, err := o.GetConnection(ctx, m.TargetConnID); err == nil {
		r.Target = &c
	}
	r.Databases, _ = o.ListDatabases(ctx, m.ID)
	rows, err := st.DB.QueryContext(ctx, `SELECT d.source_name, a.number, a.kind, a.unit_name, a.command, a.started_at, a.ended_at, a.exit_code, COALESCE(a.end_reason,'')
		FROM attempts a JOIN migration_databases d ON d.id=a.database_id WHERE d.migration_id=? ORDER BY d.source_name, a.number`, m.ID)
	if err == nil {
		for rows.Next() {
			var a Attempt
			var ended, code *int64
			rows.Scan(&a.Database, &a.Number, &a.Kind, &a.Unit, &a.Command, &a.StartedAt, &ended, &code, &a.Reason)
			a.EndedAt = ended
			if code != nil {
				c := int(*code)
				a.ExitCode = &c
			}
			r.Attempts = append(r.Attempts, a)
		}
		rows.Close()
	}
	_, r.Preflight, _ = o.LatestPreflight(ctx, m.ID)
	r.Accepted = o.AcceptedRisks(ctx, m.ID)
	_, r.Verification, _ = o.Verification(ctx, m.ID)
	ev, err := st.DB.QueryContext(ctx, `SELECT ts, COALESCE(database,''), type, severity, message FROM events WHERE migration_id=? ORDER BY ts`, m.ID)
	if err == nil {
		for ev.Next() {
			var ts int64
			var db, typ, sev, msg string
			ev.Scan(&ts, &db, &typ, &sev, &msg)
			r.Events = append(r.Events, map[string]any{"ts": ts, "database": db, "type": typ, "severity": sev, "message": msg})
		}
		ev.Close()
	}
	r.EngineVersion, _ = o.Engine().Version(ctx)
	return r, nil
}

func ts(ms any) string {
	var v int64
	switch x := ms.(type) {
	case int64:
		v = x
	case *int64:
		if x == nil {
			return "—"
		}
		v = *x
	case float64:
		v = int64(x)
	}
	if v == 0 {
		return "—"
	}
	return time.UnixMilli(v).UTC().Format("2006-01-02 15:04:05 UTC")
}

func bytesH(b int64) string {
	f := float64(b)
	u := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

func dur(ms int64) string { return (time.Duration(ms) * time.Millisecond).Round(time.Second).String() }

func detailMsg(raw json.RawMessage) string {
	var d struct {
		Message string `json:"message"`
	}
	json.Unmarshal(raw, &d)
	return d.Message
}

var funcs = template.FuncMap{"ts": ts, "bytes": bytesH, "dur": dur, "msg": detailMsg, "label": orch.Label, "upper": strings.ToUpper}

const htmlTpl = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Upwell report: {{.Migration.Name}}</title>
<style>
:root{--bg:#fff;--fg:#14212b;--muted:#5a6874;--rule:#dbe2e7;--go:#17653a;--nogo:#a1261d;--warn:#8a5300}
body{font:15px/1.55 system-ui,-apple-system,"Segoe UI",sans-serif;color:var(--fg);background:var(--bg);max-width:1100px;margin:0 auto;padding:32px 24px}
h1{font-size:28px;margin:0 0 4px}h2{font-size:20px;margin:36px 0 10px;border-bottom:1px solid var(--rule);padding-bottom:6px}
.muted{color:var(--muted)}.badge{display:inline-block;padding:2px 10px;border-radius:4px;font-weight:600;font-size:13px}
.go{background:#e3f3e8;color:var(--go)}.nogo{background:#fbe6e3;color:var(--nogo)}.pending{background:#eef1f4;color:var(--muted)}
table{border-collapse:collapse;width:100%;font-size:13.5px;margin:8px 0}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--rule);vertical-align:top}
th{font-size:12px;text-transform:uppercase;letter-spacing:.04em;color:var(--muted)}code,.mono{font-family:ui-monospace,Menlo,monospace;font-size:12.5px;word-break:break-all}
.lv-blocker{color:var(--nogo);font-weight:600}.lv-warning{color:var(--warn)}.lv-ok{color:var(--go)}
.conf{font-size:12px;color:var(--muted);border:1px solid var(--rule);padding:6px 10px;border-radius:4px;display:inline-block;margin-top:8px}
</style></head><body>
<p class="muted">Upwell migration report · generated {{ts .GeneratedAt}}</p>
<h1>{{.Migration.Name}}</h1>
<p>Migration <code>{{.Migration.ShortID}}</code> · state {{label .Migration.State}} · verdict
{{if eq .Migration.Flags.Verdict "GO"}}<span class="badge go">GO</span>{{else if eq .Migration.Flags.Verdict "NO-GO"}}<span class="badge nogo">NO-GO</span>{{else}}<span class="badge pending">no verdict yet</span>{{end}}</p>
<p class="conf">Customer-confidential: contains database and table names and sizes.</p>
<h2>Customer permission</h2>
{{with .Permission}}<table><tr><th>Customer</th><td>{{.Customer}}</td><th>Account</th><td>{{.AccountID}}</td></tr><tr><th>Ticket</th><td>{{.Ticket}}</td><th>Granted by</th><td>{{.GrantedBy}} on {{.GrantedAt}}</td></tr><tr><th>Scope</th><td colspan="3">{{.Scope}}</td></tr></table>{{else}}<p>No permission record.</p>{{end}}
<h2>Clusters</h2>
<table><tr><th></th><th>Host</th><th>Port</th><th>User</th><th>SSL</th></tr>
{{with .Source}}<tr><th>Source</th><td>{{.Host}}</td><td>{{.Port}}</td><td>{{.User}}</td><td>{{.SSLMode}}</td></tr>{{end}}
{{with .Target}}<tr><th>Target</th><td>{{.Host}}</td><td>{{.Port}}</td><td>{{.User}}</td><td>{{.SSLMode}}</td></tr>{{end}}</table>
<p class="muted">Engine pgcopydb {{.EngineVersion}} · Upwell {{.AppVersion}}{{with .Migration.Flags.Cutover}} · write pause {{dur .WritePauseMS}}{{end}}</p>
<h2>Databases</h2>
<table><tr><th>Source</th><th>Target</th><th>Size</th><th>State</th><th>Verdict</th><th>Plugin</th><th>Final heartbeat after</th><th>Target origin</th></tr>
{{range .Databases}}{{if .Include}}<tr><td>{{.SourceName}}</td><td>{{.TargetName}}</td><td>{{bytes .SizeBytes}}</td><td>{{label .State}}</td>
<td>{{if eq .Verdict "GO"}}<span class="badge go">GO</span>{{else if eq .Verdict "NO-GO"}}<span class="badge nogo">NO-GO</span>{{else}}—{{end}}</td><td>{{.Plugin}}</td><td class="mono">{{.HBBeforeLSN}}</td><td class="mono">{{.OriginLSN}}</td></tr>{{end}}{{end}}</table>
<h2>Verification</h2>
{{if .Verification}}<table><tr><th>Database</th><th>Check</th><th>Result</th><th>Detail</th></tr>
{{range .Verification}}<tr><td>{{.Database}}</td><td>{{.Title}}</td><td class="lv-{{.Result}}">{{upper .Result}}</td><td>{{msg .Detail}}</td></tr>{{end}}</table>{{else}}<p>Not verified yet.</p>{{end}}
<h2>Accepted risks</h2>
{{if .Accepted}}<table><tr><th>Check</th><th>Scope</th><th>Reason</th><th>Accepted by</th><th>When</th></tr>
{{range .Accepted}}<tr><td>{{index . "check_id"}}</td><td>{{index . "scope"}} {{index . "database"}}</td><td>{{index . "reason"}}</td><td>{{index . "accepted_by"}}</td><td>{{ts (index . "accepted_at")}}</td></tr>{{end}}</table>{{else}}<p>None.</p>{{end}}
<h2>Preflight</h2>
<table><tr><th>Check</th><th>Scope</th><th>Level</th><th>Message</th></tr>
{{range .Preflight}}<tr><td>{{.Title}}</td><td>{{.Scope}} {{.Database}}</td><td class="lv-{{.Level}}">{{.Level}}{{if .Hard}} (hard){{end}}{{if .Accepted}} (accepted){{end}}</td><td>{{.Message}}</td></tr>{{end}}</table>
<h2>Engine attempts</h2>
<table><tr><th>Database</th><th>#</th><th>Kind</th><th>Started</th><th>Ended</th><th>Exit</th><th>Reason</th></tr>
{{range .Attempts}}<tr><td>{{.Database}}</td><td>{{.Number}}</td><td>{{.Kind}}</td><td>{{ts .StartedAt}}</td><td>{{ts .EndedAt}}</td><td>{{with .ExitCode}}{{.}}{{else}}—{{end}}</td><td>{{.Reason}}</td></tr>{{end}}</table>
<h2>Timeline</h2>
<table><tr><th>Time</th><th>Database</th><th>Event</th></tr>
{{range .Events}}<tr><td>{{ts (index . "ts")}}</td><td>{{index . "database"}}</td><td>{{index . "message"}}</td></tr>{{end}}</table>
</body></html>`

var tpl = template.Must(template.New("r").Funcs(funcs).Parse(htmlTpl))

// HTML renders the report.
func (r *Report) HTML() ([]byte, error) {
	var b bytes.Buffer
	err := tpl.Execute(&b, r)
	return b.Bytes(), err
}

// Markdown renders the report.
func (r *Report) Markdown() []byte {
	var b strings.Builder
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ") }
	v := r.Migration.Flags.Verdict
	if v == "" {
		v = "no verdict yet"
	}
	fmt.Fprintf(&b, "# %s\n\nUpwell migration report, generated %s. Migration `%s`, state %s, verdict **%s**.\n\nCustomer-confidential: contains database and table names and sizes.\n\n", r.Migration.Name, ts(r.GeneratedAt), r.Migration.ShortID, orch.Label(r.Migration.State), v)
	if p := r.Permission; p != nil {
		fmt.Fprintf(&b, "## Customer permission\n\n- Customer: %s (account %s)\n- Ticket: %s\n- Granted by %s on %s\n- Scope: %s\n\n", p.Customer, p.AccountID, p.Ticket, p.GrantedBy, p.GrantedAt, esc(p.Scope))
	}
	b.WriteString("## Databases\n\n| Source | Target | Size | State | Verdict |\n| --- | --- | --- | --- | --- |\n")
	for _, d := range r.Databases {
		if d.Include {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", d.SourceName, d.TargetName, bytesH(d.SizeBytes), orch.Label(d.State), d.Verdict)
		}
	}
	b.WriteString("\n## Verification\n\n| Database | Check | Result | Detail |\n| --- | --- | --- | --- |\n")
	for _, x := range r.Verification {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", x.Database, x.Title, x.Result, esc(detailMsg(x.Detail)))
	}
	b.WriteString("\n## Accepted risks\n\n")
	if len(r.Accepted) == 0 {
		b.WriteString("None.\n")
	}
	for _, a := range r.Accepted {
		fmt.Fprintf(&b, "- %v (%v %v): %v, accepted by %v\n", a["check_id"], a["scope"], a["database"], a["reason"], a["accepted_by"])
	}
	b.WriteString("\n## Engine attempts\n\n| Database | # | Kind | Started | Ended | Reason |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, a := range r.Attempts {
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s |\n", a.Database, a.Number, a.Kind, ts(a.StartedAt), ts(a.EndedAt), esc(a.Reason))
	}
	b.WriteString("\n## Timeline\n\n")
	for _, e := range r.Events {
		fmt.Fprintf(&b, "- %s %v %v\n", ts(e["ts"]), e["database"], esc(fmt.Sprint(e["message"])))
	}
	return []byte(b.String())
}
