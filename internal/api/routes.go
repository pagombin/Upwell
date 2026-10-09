package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/auth"
	"github.com/pagombin/upwell/internal/bus"
	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/logx"
	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pgpack"
	"github.com/pagombin/upwell/internal/report"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
)

const (
	viewer   = auth.RoleViewer
	operator = auth.RoleOperator
	admin    = auth.RoleAdmin
)

func (s *Server) register() {
	pub := func(r *route) { r.public = true }
	noIdem := func(r *route) { r.noIdem = true }
	pub(s.add("GET", "/healthz", "", "Liveness", s.healthz))
	pub(s.add("GET", "/readyz", "", "Readiness", s.readyz))
	pub(s.add("GET", "/api/v1/openapi.json", "", "OpenAPI document", s.openapi))
	pub(s.add("GET", "/api/v1/setup", "", "First-run status", s.setupStatus))
	r := s.add("POST", "/api/v1/setup", "", "Create the first Admin", s.setupCreate)
	pub(r)
	noIdem(r)
	r = s.add("POST", "/api/v1/auth/login", "", "Sign in", s.login)
	pub(r)
	noIdem(r)
	noIdem(s.add("POST", "/api/v1/auth/logout", viewer, "Sign out", s.logout))
	s.add("GET", "/api/v1/auth/me", viewer, "Current user and CSRF token", s.me)
	pub(s.add("GET", "/api/v1/auth/session", "", "Session status without an error when signed out", s.sessionStatus))
	s.add("POST", "/api/v1/auth/totp", viewer, "TOTP (coming soon)", s.comingSoon("TOTP second factor"))
	s.add("POST", "/api/v1/auth/password", viewer, "Change own password", s.changePassword)

	s.add("GET", "/api/v1/users", admin, "List users", s.listUsers)
	s.add("POST", "/api/v1/users", admin, "Create a user", s.createUser)
	s.add("PATCH", "/api/v1/users/{id}", admin, "Change a user's role or disable them", s.updateUser)
	s.add("DELETE", "/api/v1/users/{id}", admin, "Delete a user", s.deleteUser)
	s.add("GET", "/api/v1/api-tokens", admin, "API tokens (coming soon)", s.emptyList)
	s.add("POST", "/api/v1/api-tokens", admin, "API tokens (coming soon)", s.comingSoon("API tokens"))

	s.add("GET", "/api/v1/settings", viewer, "Settings catalog with values", s.getSettings)
	s.add("PATCH", "/api/v1/settings", admin, "Change global defaults", s.patchSettings)

	s.add("POST", "/api/v1/do/token", admin, "DigitalOcean API token (coming soon)", s.comingSoon("DigitalOcean API discovery"))
	s.add("GET", "/api/v1/do/clusters", operator, "DigitalOcean clusters (coming soon)", s.comingSoon("DigitalOcean API discovery"))

	s.add("GET", "/api/v1/migrations", viewer, "List migrations", s.listMigrations)
	s.add("POST", "/api/v1/migrations", operator, "Create a migration", s.createMigration)
	s.add("GET", "/api/v1/migrations/{id}", viewer, "Migration with databases", s.getMigration)
	s.add("PATCH", "/api/v1/migrations/{id}", operator, "Rename or change per-migration settings", s.patchMigration)
	s.add("DELETE", "/api/v1/migrations/{id}", operator, "Delete a migration (after cleanup)", s.deleteMigration)
	s.add("PUT", "/api/v1/migrations/{id}/permission", operator, "Customer permission record", s.putPermission)
	s.add("PUT", "/api/v1/migrations/{id}/connections/{kind}", operator, "Set source or target connection", s.putConnection)
	s.add("POST", "/api/v1/migrations/{id}/connections/{kind}/test", operator, "Test a connection", s.testConnection)
	s.add("POST", "/api/v1/migrations/{id}/discover", operator, "Discover source databases", s.discover)
	s.add("PUT", "/api/v1/migrations/{id}/databases", operator, "Select databases and target names", s.putDatabases)
	s.add("POST", "/api/v1/migrations/{id}/preflight", operator, "Run preflight", s.runPreflight)
	s.add("GET", "/api/v1/migrations/{id}/preflight/latest", viewer, "Latest preflight results", s.latestPreflight)
	s.add("GET", "/api/v1/migrations/{id}/preflight/runs", viewer, "Preflight history", s.preflightRuns)
	s.add("GET", "/api/v1/migrations/{id}/preflight/runs/{run}", viewer, "One preflight run's results", s.preflightRun)
	s.add("POST", "/api/v1/migrations/{id}/acceptances", operator, "Accept an acceptable blocker", s.accept)
	s.add("GET", "/api/v1/migrations/{id}/plan", viewer, "Dry run: commands and estimates", s.plan)
	s.add("POST", "/api/v1/migrations/{id}/start", operator, "Start", s.start)
	s.add("POST", "/api/v1/migrations/{id}/queue", operator, "Queue (coming soon)", s.comingSoon("Queueing several migrations"))
	s.add("POST", "/api/v1/migrations/{id}/pause", operator, "Pause (stop every database, keep slots)", s.pause)
	s.add("POST", "/api/v1/migrations/{id}/resume", operator, "Resume", s.resume)
	s.add("POST", "/api/v1/migrations/{id}/abort", operator, "Abort", s.abort)
	s.add("POST", "/api/v1/migrations/{id}/databases/{db}/stop", operator, "Stop one database", s.stopDB)
	s.add("POST", "/api/v1/migrations/{id}/databases/{db}/resume", operator, "Resume one database", s.resumeDB)
	s.add("POST", "/api/v1/migrations/{id}/databases/{db}/restart", operator, "Restart one database from zero", s.restartDB)
	s.add("GET", "/api/v1/migrations/{id}/databases/{db}", viewer, "One database with attempts and live detail", s.getDB)
	s.add("GET", "/api/v1/migrations/{id}/cutover/readiness", viewer, "Cutover readiness gate", s.readiness)
	s.add("POST", "/api/v1/migrations/{id}/cutover", operator, "Start the cutover", s.cutover)
	s.add("POST", "/api/v1/migrations/{id}/cutover/abort", operator, "Abort the cutover before end positions are set", s.abortCutover)
	s.add("POST", "/api/v1/migrations/{id}/cutover/confirm-writers-stopped", operator, "Confirm writers stopped (part of starting the cutover)", s.cutover)
	s.add("POST", "/api/v1/migrations/{id}/cutover/freeze", admin, "Write freeze (coming soon)", s.comingSoon("Write freeze"))
	s.add("POST", "/api/v1/migrations/{id}/cutover/unfreeze", admin, "Undo write freeze (coming soon)", s.comingSoon("Write freeze"))
	s.add("POST", "/api/v1/migrations/{id}/switched", operator, "Mark applications switched", s.switched)
	s.add("POST", "/api/v1/migrations/{id}/verify", operator, "Run verification again", s.verify)
	s.add("GET", "/api/v1/migrations/{id}/verification", viewer, "Latest verification", s.verification)
	s.add("POST", "/api/v1/migrations/{id}/cleanup", operator, "Clean up", s.cleanup)
	s.add("GET", "/api/v1/migrations/{id}/metrics", viewer, "Time series", s.metrics)
	s.add("GET", "/api/v1/migrations/{id}/logs", viewer, "Log search and download", s.logs)
	s.add("GET", "/api/v1/migrations/{id}/events", viewer, "Timeline", s.events)
	s.add("GET", "/api/v1/migrations/{id}/operations", viewer, "Operations", s.listOps)
	s.add("GET", "/api/v1/migrations/{id}/report", viewer, "Report (html or md; pdf coming soon)", s.report)
	s.add("POST", "/api/v1/migrations/{id}/support-bundle", operator, "Support bundle (coming soon)", s.comingSoon("Support bundle"))
	s.add("GET", "/api/v1/migrations/{id}/source-health", viewer, "Source impact: snapshots, dead tuples, connections", s.sourceHealth)
	s.add("GET", "/api/v1/operations/{id}", viewer, "Operation status and steps", s.getOp)
	s.add("GET", "/api/v1/logs", viewer, "Log search across everything", s.logs)
	s.add("GET", "/api/v1/events", viewer, "Timeline across migrations", s.events)
	s.add("GET", "/api/v1/alerts", viewer, "Alerts", s.alerts)
	s.add("POST", "/api/v1/alerts/{id}/ack", operator, "Acknowledge an alert", s.ackAlert)
	s.add("GET", "/api/v1/audit", viewer, "Audit log", s.auditList)
	s.add("GET", "/api/v1/audit/verify", viewer, "Verify the audit chain", s.auditVerify)
	s.add("GET", "/api/v1/system", viewer, "System health and versions", s.system)
	s.add("GET", "/api/v1/host/metrics", viewer, "Droplet metrics", s.hostMetrics)
	s.add("POST", "/api/v1/system/restart", admin, "Restart the app (engines keep running)", s.restartApp)
	s.add("GET", "/metrics", admin, "Prometheus (coming soon)", s.comingSoon("Prometheus metrics"))
	s.add("POST", "/api/v1/dev/fixtures", admin, "Create demonstration fixtures (dev only)", s.devFixtures)

	s.add("GET", "/api/v1/stream/migrations/{id}", viewer, "Live migration stream (SSE)", s.streamMigration)
	s.add("GET", "/api/v1/stream/logs", viewer, "Live log stream (SSE)", s.streamLogs)
	s.add("GET", "/api/v1/stream/system", viewer, "Live system stream (SSE)", s.streamSystem)
}

func (s *Server) comingSoon(what string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		return apiErr(501, "coming_soon", what+" is coming soon; it is not part of this build.", "")
	}
}

func (s *Server) emptyList(w http.ResponseWriter, r *http.Request) error {
	return writeJSON(w, 200, []any{})
}

// ---------- health

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) error {
	ok, late := s.Health.Alive(90 * time.Second)
	if !ok {
		return writeJSON(w, 503, map[string]any{"ok": false, "late": late})
	}
	return writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) error {
	ok, detail := s.Health.Ready()
	code := 200
	if !ok {
		code = 503
	}
	detail["ok"] = ok
	return writeJSON(w, code, detail)
}

// ---------- setup and auth

func (s *Server) setupToken() string {
	b, err := os.ReadFile(s.Cfg.SetupToken)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) error {
	n, err := s.Auth.CountUsers(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"needs_setup": n == 0, "tls": s.TLSInfo(), "setup_token_file": s.Cfg.SetupToken, "version": s.Version})
}

func (s *Server) setupCreate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		SetupToken string `json:"setup_token"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		TimeZone   string `json:"time_zone"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	n, err := s.Auth.CountUsers(r.Context())
	if err != nil {
		return err
	}
	if n > 0 {
		return apiErr(409, "already_set_up", "Upwell is already set up. Sign in instead.", "")
	}
	want := s.setupToken()
	if want == "" || in.SetupToken != want {
		return apiErr(403, "bad_setup_token", "The setup token is not correct.", "install.sh printed it; it is also in "+s.Cfg.SetupToken+" on the droplet.")
	}
	u, err := s.Auth.CreateUser(r.Context(), in.Username, in.Password, auth.RoleAdmin)
	if err != nil {
		return badRequest(err.Error())
	}
	if in.TimeZone != "" {
		s.Store.DB.ExecContext(r.Context(), `INSERT OR REPLACE INTO settings(key,value,updated_by,updated_at) VALUES ('_time_zone',?,?,?)`, strconv.Quote(in.TimeZone), u.Username, store.Now())
	}
	os.Remove(s.Cfg.SetupToken)
	s.Audit.Append(r.Context(), audit.Actor{UserID: u.ID, Username: u.Username, IP: clientIP(r)}, "setup.first_admin", u.ID, nil, map[string]any{"username": u.Username})
	return writeJSON(w, 201, u)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	tok, sess, err := s.Auth.Login(r.Context(), strings.TrimSpace(strings.ToLower(in.Username)), in.Password, clientIP(r), r.UserAgent())
	a := audit.Actor{Username: in.Username, IP: clientIP(r)}
	if err != nil {
		s.Audit.Append(r.Context(), a, "auth.login_failed", in.Username, nil, map[string]any{"reason": err.Error()})
		if errors.Is(err, auth.ErrBadCredentials) || errors.Is(err, auth.ErrLocked) || errors.Is(err, auth.ErrDisabled) {
			return apiErr(401, "bad_credentials", err.Error(), "")
		}
		return err
	}
	a.UserID = sess.User.ID
	s.Audit.Append(r.Context(), a, "auth.login", sess.User.ID, nil, nil)
	http.SetCookie(w, &http.Cookie{Name: "upwell_session", Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, Expires: time.UnixMilli(sess.ExpiresAt)})
	return writeJSON(w, 200, map[string]any{"user": sess.User, "csrf_token": sess.CSRF, "expires_at": sess.ExpiresAt})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	sess := session(r)
	s.Auth.Logout(r.Context(), sess.ID)
	s.Audit.Append(r.Context(), actor(r), "auth.logout", sess.User.ID, nil, nil)
	http.SetCookie(w, &http.Cookie{Name: "upwell_session", Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	return writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) error {
	sess := session(r)
	var tz string
	s.Store.DB.QueryRowContext(r.Context(), `SELECT value FROM settings WHERE key='_time_zone'`).Scan(&tz)
	tz, _ = strconv.Unquote(tz)
	g, _ := s.Settings.Global(r.Context())
	return writeJSON(w, 200, map[string]any{"user": sess.User, "csrf_token": sess.CSRF, "expires_at": sess.ExpiresAt, "idle_minutes": g.Int("session_idle_minutes"), "time_zone": tz, "version": s.Version})
}

// sessionStatus lets the UI find out whether it is signed in without a 401.
func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) error {
	tok := ""
	if c, _ := r.Cookie("upwell_session"); c != nil {
		tok = c.Value
	}
	if tok != "" {
		if sess, err := s.Auth.Resolve(r.Context(), tok); err == nil {
			r = r.WithContext(context.WithValue(r.Context(), sessKey, sess))
			return s.me(w, r)
		}
	}
	n, err := s.Auth.CountUsers(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"user": nil, "needs_setup": n == 0})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	sess := session(r)
	var hash string
	s.Store.DB.QueryRowContext(r.Context(), `SELECT password_hash FROM users WHERE id=?`, sess.User.ID).Scan(&hash)
	if !auth.CheckPassword(hash, in.Current) {
		return apiErr(400, "bad_password", "The current password is not correct.", "")
	}
	if err := s.Auth.SetPassword(r.Context(), sess.User.ID, in.New); err != nil {
		return badRequest(err.Error())
	}
	s.Audit.Append(r.Context(), actor(r), "auth.password_changed", sess.User.ID, nil, nil)
	return writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- users

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) error {
	us, err := s.Auth.ListUsers(r.Context())
	if err != nil {
		return err
	}
	if us == nil {
		us = []auth.User{}
	}
	return writeJSON(w, 200, us)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	u, err := s.Auth.CreateUser(r.Context(), strings.ToLower(strings.TrimSpace(in.Username)), in.Password, in.Role)
	if err != nil {
		return badRequest(err.Error())
	}
	s.Audit.Append(r.Context(), actor(r), "user.create", u.ID, nil, map[string]any{"username": u.Username, "role": u.Role})
	return writeJSON(w, 201, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Role     string  `json:"role"`
		Disabled bool    `json:"disabled"`
		Password *string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	id := r.PathValue("id")
	before, err := s.Auth.GetUser(r.Context(), id)
	if err != nil {
		return err
	}
	if id == session(r).User.ID && (in.Role != auth.RoleAdmin || in.Disabled) {
		return badRequest("You cannot remove your own Admin role or disable yourself.")
	}
	if err := s.Auth.UpdateUser(r.Context(), id, in.Role, in.Disabled); err != nil {
		return badRequest(err.Error())
	}
	if in.Password != nil {
		if err := s.Auth.SetPassword(r.Context(), id, *in.Password); err != nil {
			return badRequest(err.Error())
		}
	}
	after, _ := s.Auth.GetUser(r.Context(), id)
	s.Audit.Append(r.Context(), actor(r), "user.update", id, before, map[string]any{"role": after.Role, "disabled": after.Disabled, "password_reset": in.Password != nil})
	return writeJSON(w, 200, after)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == session(r).User.ID {
		return badRequest("You cannot delete yourself.")
	}
	before, err := s.Auth.GetUser(r.Context(), id)
	if err != nil {
		return err
	}
	if err := s.Auth.DeleteUser(r.Context(), id); err != nil {
		return err
	}
	s.Audit.Append(r.Context(), actor(r), "user.delete", id, before, nil)
	return writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- settings

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) error {
	items, err := s.Settings.Items(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, items)
}

func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request) error {
	var in map[string]any
	if err := decode(r, &in); err != nil {
		return err
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := settings.Validate(k, in[k]); err != nil {
			return badRequest(err.Error())
		}
	}
	for _, k := range keys {
		prev, next, err := s.Settings.Set(r.Context(), k, in[k], session(r).User.Username)
		if err != nil {
			return badRequest(err.Error())
		}
		s.Audit.Append(r.Context(), actor(r), "settings.change", k, prev, next)
		if k == "log_level" {
			s.Log.SetLevel(fmt.Sprint(next))
		}
	}
	return s.getSettings(w, r)
}

// ---------- migrations

func (s *Server) listMigrations(w http.ResponseWriter, r *http.Request) error {
	ms, err := s.Orch.ListMigrations(r.Context())
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, m := range ms {
		dbs, _ := s.Orch.ListDatabases(r.Context(), m.ID)
		out = append(out, migrationView(m, dbs))
	}
	return writeJSON(w, 200, out)
}

func migrationView(m orch.Migration, dbs []orch.Database) map[string]any {
	if dbs == nil {
		dbs = []orch.Database{}
	}
	inc := 0
	var total, done float64
	counts := map[string]int{}
	for _, d := range dbs {
		if d.Include {
			inc++
			counts[d.State]++
			total += float64(d.SizeBytes)
		}
	}
	_ = done
	return map[string]any{"migration": m, "databases": dbs, "included": inc, "state_counts": counts, "total_bytes": total}
}

func (s *Server) createMigration(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.Orch.CreateMigration(r.Context(), actor(r), in.Name)
	if err != nil {
		return err
	}
	return writeJSON(w, 201, m)
}

func (s *Server) getMigration(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	m, err := s.Orch.GetMigration(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	dbs, _ := s.Orch.ListDatabases(ctx, m.ID)
	perm, _ := s.Orch.GetPermission(ctx, m.ID)
	view := migrationView(m, dbs)
	view["permission"] = perm
	if m.SourceConnID != "" {
		if c, err := s.Orch.GetConnection(ctx, m.SourceConnID); err == nil {
			view["source"] = c
		}
	}
	if m.TargetConnID != "" {
		if c, err := s.Orch.GetConnection(ctx, m.TargetConnID); err == nil {
			view["target"] = c
		}
	}
	view["settings_effective"] = s.Orch.Settings(ctx, m)
	run, _, _ := s.Orch.LatestPreflight(ctx, m.ID)
	view["preflight"] = run
	// live values for the overview
	live := map[string]map[string]float64{}
	for _, d := range dbs {
		vals := map[string]float64{}
		for _, n := range []string{"replay_rate", "slot_retained_bytes", "slot_headroom_bytes", "slot_active", "slot_wal_status", "heartbeat_latency_s", "target_size_bytes", "source_size_bytes", "log_errors"} {
			if v, ok := s.Orch.LastValue(m.ID, d.SourceName, n); ok {
				vals[n] = v
			}
		}
		live[d.SourceName] = vals
	}
	view["live"] = live
	cl := map[string]float64{}
	for _, n := range []string{"src_wal_rate", "src_connections", "dst_connections", "src_oldest_xact_s", "src_xmin_age"} {
		if v, ok := s.Orch.LastValue(m.ID, "", n); ok {
			cl[n] = v
		}
	}
	view["cluster"] = cl
	view["now"] = store.Now()
	return writeJSON(w, 200, view)
}

func (s *Server) patchMigration(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name     *string        `json:"name"`
		Settings map[string]any `json:"settings"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.Orch.UpdateMigration(r.Context(), actor(r), r.PathValue("id"), in.Name, in.Settings)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

func (s *Server) deleteMigration(w http.ResponseWriter, r *http.Request) error {
	if err := s.Orch.DeleteMigration(r.Context(), actor(r), r.PathValue("id")); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) putPermission(w http.ResponseWriter, r *http.Request) error {
	var in orch.Permission
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.Orch.SetPermission(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, p)
}

func (s *Server) putConnection(w http.ResponseWriter, r *http.Request) error {
	kind := r.PathValue("kind")
	if kind != "source" && kind != "target" {
		return badRequest("kind must be source or target")
	}
	var in orch.ConnInput
	if err := decode(r, &in); err != nil {
		return err
	}
	c, err := s.Orch.SetConnection(r.Context(), actor(r), r.PathValue("id"), kind, in)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, c)
}

func (s *Server) testConnection(w http.ResponseWriter, r *http.Request) error {
	kind := r.PathValue("kind")
	res, err := s.Orch.TestMigrationConnection(r.Context(), r.PathValue("id"), kind)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, res)
}

func (s *Server) discover(w http.ResponseWriter, r *http.Request) error {
	dbs, err := s.Orch.Discover(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	if dbs == nil {
		dbs = []orch.Database{}
	}
	return writeJSON(w, 200, dbs)
}

func (s *Server) putDatabases(w http.ResponseWriter, r *http.Request) error {
	var in []orch.DBSelection
	if err := decode(r, &in); err != nil {
		return err
	}
	dbs, err := s.Orch.SetDatabases(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	if dbs == nil {
		dbs = []orch.Database{}
	}
	return writeJSON(w, 200, dbs)
}

func opResponse(w http.ResponseWriter, op *orch.Operation, err error) error {
	if errors.Is(err, orch.ErrDuplicate) {
		return writeJSON(w, 202, map[string]any{"operation_id": op.ID, "duplicate": true})
	}
	if err != nil {
		return err
	}
	return writeJSON(w, 202, map[string]any{"operation_id": op.ID})
}

func (s *Server) runPreflight(w http.ResponseWriter, r *http.Request) error {
	op, err := s.Orch.RunPreflight(r.Context(), actor(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	return opResponse(w, op, err)
}

func (s *Server) latestPreflight(w http.ResponseWriter, r *http.Request) error {
	run, res, err := s.Orch.LatestPreflight(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if res == nil {
		res = []check.Result{}
	}
	return writeJSON(w, 200, map[string]any{"run": run, "results": res, "catalog": pgpack.Catalog})
}

func (s *Server) preflightRuns(w http.ResponseWriter, r *http.Request) error {
	runs, err := s.Orch.PreflightRuns(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, runs)
}

func (s *Server) preflightRun(w http.ResponseWriter, r *http.Request) error {
	res, err := s.Orch.CheckResults(r.Context(), r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, res)
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ResultID string `json:"result_id"`
		Reason   string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	res, err := s.Orch.Accept(r.Context(), actor(r), r.PathValue("id"), in.ResultID, in.Reason)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, res)
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) error {
	p, est, err := s.Orch.Plan(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if p == nil {
		p = []orch.PlanEntry{}
	}
	return writeJSON(w, 200, map[string]any{"databases": p, "estimates": est})
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) error {
	var in orch.StartOptions
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.Orch.Start(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Orch.Pause(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Orch.Resume(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

type confirmBody struct {
	Confirm string `json:"confirm"`
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) error {
	var in confirmBody
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.Orch.Abort(r.Context(), actor(r), r.PathValue("id"), in.Confirm)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

func (s *Server) stopDB(w http.ResponseWriter, r *http.Request) error {
	d, err := s.Orch.StopDatabase(r.Context(), actor(r), r.PathValue("id"), r.PathValue("db"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, d)
}

func (s *Server) resumeDB(w http.ResponseWriter, r *http.Request) error {
	d, err := s.Orch.ResumeDatabase(r.Context(), actor(r), r.PathValue("id"), r.PathValue("db"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, d)
}

func (s *Server) restartDB(w http.ResponseWriter, r *http.Request) error {
	var in confirmBody
	if err := decode(r, &in); err != nil {
		return err
	}
	d, err := s.Orch.RestartDatabase(r.Context(), actor(r), r.PathValue("id"), r.PathValue("db"), in.Confirm)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, d)
}

func (s *Server) getDB(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	m, err := s.Orch.GetMigration(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	d, err := s.Orch.GetDatabase(ctx, m.ID, r.PathValue("db"))
	if err != nil {
		return err
	}
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT number, kind, unit_name, command, started_at, ended_at, exit_code, COALESCE(end_reason,'') FROM attempts WHERE database_id=? ORDER BY number DESC`, d.ID)
	attempts := []map[string]any{}
	if err == nil {
		for rows.Next() {
			var n int
			var kind, unit, cmd, reason string
			var started int64
			var ended, code *int64
			rows.Scan(&n, &kind, &unit, &cmd, &started, &ended, &code, &reason)
			attempts = append(attempts, map[string]any{"number": n, "kind": kind, "unit": unit, "command": cmd, "started_at": started, "ended_at": ended, "exit_code": code, "end_reason": reason})
		}
		rows.Close()
	}
	live := map[string]float64{}
	for _, n := range []string{"replay_rate", "slot_retained_bytes", "slot_headroom_bytes", "slot_active", "slot_wal_status", "heartbeat_latency_s", "target_size_bytes", "source_size_bytes", "log_errors", "backlog_bytes"} {
		if v, ok := s.Orch.LastValue(m.ID, d.SourceName, n); ok {
			live[n] = v
		}
	}
	unit, _ := s.Orch.Runner().Status(ctx, d.Instance)
	detail := map[string]any{"migration": m, "database": d, "attempts": attempts, "live": live, "unit": unit, "unit_name": s.Orch.Runner().UnitName(d.Instance), "now": store.Now()}
	// Active COPY and index builds on the target (cheap, timeout-bounded).
	if src, dst, err := s.Orch.Conns(ctx, m); err == nil && d.State == orch.DBaseCopy {
		_ = src
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		detail["copy_progress"] = pgpack.CopyProgress(cctx, dst.WithDB(d.TargetName))
	}
	return writeJSON(w, 200, detail)
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) error {
	conds, ok, err := s.Orch.Readiness(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"conditions": conds, "ready": ok})
}

func (s *Server) cutover(w http.ResponseWriter, r *http.Request) error {
	var in orch.CutoverParams
	if err := decode(r, &in); err != nil {
		return err
	}
	op, err := s.Orch.StartCutover(r.Context(), actor(r), session(r).User.Role, r.PathValue("id"), r.Header.Get("Idempotency-Key"), in)
	return opResponse(w, op, err)
}

func (s *Server) abortCutover(w http.ResponseWriter, r *http.Request) error {
	if err := s.Orch.AbortCutover(r.Context(), actor(r), r.PathValue("id")); err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) switched(w http.ResponseWriter, r *http.Request) error {
	var in confirmBody
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.Orch.MarkSwitched(r.Context(), actor(r), r.PathValue("id"), in.Confirm)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, m)
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) error {
	op, err := s.Orch.Reverify(r.Context(), actor(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	return opResponse(w, op, err)
}

func (s *Server) verification(w http.ResponseWriter, r *http.Request) error {
	run, rows, err := s.Orch.Verification(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, map[string]any{"run_id": run, "results": rows})
}

func (s *Server) cleanup(w http.ResponseWriter, r *http.Request) error {
	var in confirmBody
	if err := decode(r, &in); err != nil {
		return err
	}
	op, err := s.Orch.Cleanup(r.Context(), actor(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"), in.Confirm)
	return opResponse(w, op, err)
}

func qint(r *http.Request, k string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(k), 10, 64)
	return v
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) error {
	var names []string
	if n := r.URL.Query().Get("names"); n != "" {
		names = strings.Split(n, ",")
	}
	m, err := s.Orch.GetMigration(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	series, err := s.Orch.Metrics(r.Context(), m.ID, names, qint(r, "from"), qint(r, "to"))
	if err != nil {
		return err
	}
	if series == nil {
		series = []orch.Series{}
	}
	return writeJSON(w, 200, series)
}

func (s *Server) hostMetrics(w http.ResponseWriter, r *http.Request) error {
	var names []string
	if n := r.URL.Query().Get("names"); n != "" {
		names = strings.Split(n, ",")
	}
	series, err := s.Orch.Metrics(r.Context(), "_host", names, qint(r, "from"), qint(r, "to"))
	if err != nil {
		return err
	}
	if series == nil {
		series = []orch.Series{}
	}
	return writeJSON(w, 200, series)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	mig := r.PathValue("id")
	if mig == "" {
		mig = q.Get("migration")
	}
	if mig != "" {
		if m, err := s.Orch.GetMigration(r.Context(), mig); err == nil {
			mig = m.ID
		}
	}
	sqlq := `SELECT seq, ts, level, component, COALESCE(migration_id,''), COALESCE(database,''), COALESCE(attempt,0), COALESCE(step,''), COALESCE(table_name,''), COALESCE(op,''), msg, COALESCE(raw,'') FROM logs WHERE 1=1`
	var args []any
	add := func(cond string, v any) { sqlq += " AND " + cond; args = append(args, v) }
	if mig != "" {
		add("migration_id=?", mig)
	}
	if v := q.Get("database"); v != "" {
		add("database=?", v)
	}
	if v := q.Get("component"); v != "" {
		add("component=?", v)
	}
	if v := q.Get("op"); v != "" {
		add("op=?", v)
	}
	if v := q.Get("attempt"); v != "" {
		add("attempt=?", v)
	}
	if v := q.Get("level"); v != "" {
		levels := []string{}
		for _, l := range []string{"trace", "debug", "info", "warn", "error", "fatal"} {
			if logx.LevelAtLeast(l, v) {
				levels = append(levels, "'"+l+"'")
			}
		}
		sqlq += " AND level IN (" + strings.Join(levels, ",") + ")"
	}
	if v := q.Get("q"); v != "" {
		add("(msg LIKE ? ESCAPE '\\')", "%"+strings.NewReplacer("%", "\\%", "_", "\\_").Replace(v)+"%")
	}
	if v := qint(r, "from"); v > 0 {
		add("ts>=?", v)
	}
	if v := qint(r, "to"); v > 0 {
		add("ts<=?", v)
	}
	if v := qint(r, "before"); v > 0 {
		add("seq<?", v)
	}
	if v := qint(r, "after"); v > 0 {
		add("seq>?", v)
	}
	limit := qint(r, "limit")
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	order := "DESC"
	if qint(r, "after") > 0 {
		order = "ASC"
	}
	sqlq += " ORDER BY seq " + order + " LIMIT ?"
	args = append(args, limit)
	rows, err := s.Store.DB.QueryContext(r.Context(), sqlq, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []logx.Record{}
	for rows.Next() {
		var rec logx.Record
		rows.Scan(&rec.Seq, &rec.TS, &rec.Level, &rec.Component, &rec.Migration, &rec.Database, &rec.Attempt, &rec.Step, &rec.Table, &rec.Op, &rec.Msg, &rec.Raw)
		out = append(out, rec)
	}
	if order == "DESC" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if q.Get("format") == "download" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="upwell-logs.jsonl"`)
		enc := json.NewEncoder(w)
		for _, rec := range out {
			enc.Encode(rec)
		}
		return nil
	}
	return writeJSON(w, 200, out)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) error {
	mig := r.PathValue("id")
	q := `SELECT id, seq, COALESCE(migration_id,''), COALESCE(database,''), type, severity, message, data, ts FROM events`
	var args []any
	if mig != "" {
		m, err := s.Orch.GetMigration(r.Context(), mig)
		if err != nil {
			return err
		}
		q += ` WHERE migration_id=?`
		args = append(args, m.ID)
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	limit := qint(r, "limit")
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	args = append(args, limit)
	rows, err := s.Store.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, migID, db, typ, sev, msg, data string
		var seq, ts int64
		rows.Scan(&id, &seq, &migID, &db, &typ, &sev, &msg, &data, &ts)
		var d any
		json.Unmarshal([]byte(data), &d)
		out = append(out, map[string]any{"id": id, "seq": seq, "migration_id": migID, "database": db, "type": typ, "severity": sev, "message": msg, "data": d, "ts": ts})
	}
	return writeJSON(w, 200, out)
}

func (s *Server) listOps(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Orch.GetMigration(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	ops, err := s.Orch.ListOperations(r.Context(), m.ID, 100)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, ops)
}

func (s *Server) getOp(w http.ResponseWriter, r *http.Request) error {
	op, err := s.Orch.GetOperation(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, 200, op)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) error {
	format := r.URL.Query().Get("format")
	if format == "pdf" {
		return apiErr(501, "coming_soon", "PDF reports are coming soon; download HTML or Markdown.", "")
	}
	rep, err := report.Build(r.Context(), s.Orch, s.Store, r.PathValue("id"), s.Version)
	if err != nil {
		return err
	}
	name := "upwell-" + rep.Migration.ShortID
	switch format {
	case "md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.md"`)
		}
		w.Write(rep.Markdown())
		return nil
	case "json":
		return writeJSON(w, 200, rep)
	default:
		b, err := rep.HTML()
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.html"`)
		}
		w.Write(b)
		return nil
	}
}

func (s *Server) sourceHealth(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Orch.GetMigration(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	src, _, err := s.Orch.Conns(r.Context(), m)
	if err != nil {
		return writeJSON(w, 200, map[string]any{"available": false, "reason": err.Error()})
	}
	dbs, _ := s.Orch.ListDatabases(r.Context(), m.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	return writeJSON(w, 200, pgpack.SourceHealth(ctx, src, dbs2names(dbs)))
}

func dbs2names(dbs []orch.Database) []string {
	var out []string
	for _, d := range dbs {
		if d.Include {
			out = append(out, d.SourceName)
		}
	}
	return out
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) error {
	as, err := s.Orch.ListAlerts(r.Context(), r.URL.Query().Get("state"), 500)
	if err != nil {
		return err
	}
	return writeJSON(w, 200, as)
}

func (s *Server) ackAlert(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Note string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.Orch.AckAlert(r.Context(), r.PathValue("id"), session(r).User.Username, in.Note); err != nil {
		return err
	}
	s.Audit.Append(r.Context(), actor(r), "alert.ack", r.PathValue("id"), nil, map[string]any{"note": in.Note})
	return writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) auditList(w http.ResponseWriter, r *http.Request) error {
	es, err := s.Audit.List(r.Context(), int(qint(r, "limit")), qint(r, "before"), r.URL.Query().Get("action"))
	if err != nil {
		return err
	}
	if es == nil {
		es = []audit.Entry{}
	}
	if r.URL.Query().Get("format") == "download" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="upwell-audit.jsonl"`)
		enc := json.NewEncoder(w)
		for i := len(es) - 1; i >= 0; i-- {
			enc.Encode(es[i])
		}
		return nil
	}
	return writeJSON(w, 200, es)
}

func (s *Server) auditVerify(w http.ResponseWriter, r *http.Request) error {
	res, err := s.Audit.Verify(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, 200, res)
}

func toolVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "not installed"
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

func dirSize(p string) int64 {
	var n int64
	filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	ev, _ := s.Orch.Engine().Version(ctx)
	var storeSize int64
	for _, suf := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(s.Cfg.StorePath() + suf); err == nil {
			storeSize += fi.Size()
		}
	}
	schema, _ := s.Store.SchemaVersion(ctx)
	ok, late := s.Health.Alive(90 * time.Second)
	ready, readyDetail := s.Health.Ready()
	return writeJSON(w, 200, map[string]any{
		"version": s.Version, "go": runtime.Version(), "started_at": s.Orch.StartedAt.UnixMilli(), "uptime_s": int64(time.Since(s.Orch.StartedAt).Seconds()),
		"engine":   map[string]any{"name": s.Orch.Engine().Name(), "version": ev, "runner": s.Orch.Runner().Name()},
		"tools":    map[string]string{"pg_dump": toolVersion(s.binPath("pg_dump")), "psql": toolVersion(s.binPath("psql"))},
		"store":    map[string]any{"path": s.Cfg.StorePath(), "bytes": storeSize, "schema_version": schema},
		"logs":     map[string]any{"dir": s.Cfg.LogDir, "bytes": dirSize(s.Cfg.LogDir)},
		"runs":     map[string]any{"dir": s.Cfg.RunsDir(), "bytes": dirSize(s.Cfg.RunsDir())},
		"watchdog": map[string]any{"healthy": ok, "late": late, "beats": s.Health.Beats(), "systemd": UnderSystemd()},
		"ready":    ready, "ready_detail": readyDetail, "tls": s.TLSInfo(), "rebooted": s.Orch.Rebooted, "reconciled": s.Orch.Reconciled,
		"updates": map[string]any{"available": false, "note": "Updates are applied by running install.sh again."},
		"dev":     s.Cfg.Dev, "units": s.engineUnits(ctx),
	})
}

// engineUnits lists the engine unit of every database of migrations that
// started and are not cleaned up.
func (s *Server) engineUnits(ctx context.Context) []map[string]any {
	out := []map[string]any{}
	ms, err := s.Orch.ListMigrations(ctx)
	if err != nil {
		return out
	}
	for _, m := range ms {
		if !m.Flags.Started || m.Flags.CleanedUp || m.Fixture {
			continue
		}
		dbs, _ := s.Orch.ListDatabases(ctx, m.ID)
		for _, d := range dbs {
			if !d.Include {
				continue
			}
			st, _ := s.Orch.Runner().Status(ctx, d.Instance)
			out = append(out, map[string]any{"migration": m.ShortID, "migration_name": m.Name, "database": d.SourceName, "state": d.State, "unit": s.Orch.Runner().UnitName(d.Instance), "status": st})
		}
	}
	return out
}

// restartApp exits so systemd restarts the service. Engines run in their own
// units and keep running; reconciliation reattaches them on start.
func (s *Server) restartApp(w http.ResponseWriter, r *http.Request) error {
	if !UnderSystemd() {
		return apiErr(409, "no_systemd", "Upwell is not running under systemd here, so it would not come back after exiting.", "Restart it with your process manager, or on the droplet run: sudo systemctl restart upwell")
	}
	s.Audit.Append(r.Context(), actor(r), "system.restart", "upwell", nil, nil)
	s.Log.Write(logx.Record{Level: "warn", Component: "app", Msg: "restart requested by " + session(r).User.Username})
	go func() {
		time.Sleep(500 * time.Millisecond)
		SDNotify("STOPPING=1")
		os.Exit(0)
	}()
	return writeJSON(w, 202, map[string]any{"ok": true, "note": "Upwell restarts in a few seconds; running migrations are not affected."})
}

func (s *Server) binPath(n string) string {
	if s.Cfg.Engine.PgBinDir != "" {
		return filepath.Join(s.Cfg.Engine.PgBinDir, n)
	}
	return n
}

func (s *Server) devFixtures(w http.ResponseWriter, r *http.Request) error {
	if !s.Cfg.Dev {
		return apiErr(404, "not_found", "Fixtures exist only in development mode.", "")
	}
	ids, err := orch.SeedFixtures(r.Context(), s.Orch, actor(r))
	if err != nil {
		return err
	}
	return writeJSON(w, 201, map[string]any{"migrations": ids})
}

// ---------- SSE

func (s *Server) sse(w http.ResponseWriter, r *http.Request, filter func(bus.Event) bool) error {
	fl, ok := w.(http.Flusher)
	if !ok {
		return apiErr(500, "no_stream", "Streaming is not supported by this connection.", "")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	since, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	sub, backlog := s.Bus.Subscribe(since, filter)
	defer sub.Close()
	send := func(e bus.Event) error {
		b, _ := json.Marshal(e)
		_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, b)
		return err
	}
	fmt.Fprintf(w, "retry: 3000\n: connected\n\n")
	for _, e := range backlog {
		if send(e) != nil {
			return nil
		}
	}
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case e := <-sub.C:
			if send(e) != nil {
				return nil
			}
			// drain quickly to batch writes
			for i := 0; i < 200; i++ {
				select {
				case e2 := <-sub.C:
					if send(e2) != nil {
						return nil
					}
					continue
				default:
				}
				break
			}
			fl.Flush()
		case <-ping.C:
			fmt.Fprintf(w, ": ping %d\n\n", time.Now().Unix())
			fl.Flush()
		}
	}
}

func (s *Server) streamMigration(w http.ResponseWriter, r *http.Request) error {
	m, err := s.Orch.GetMigration(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.sse(w, r, func(e bus.Event) bool {
		return (e.Topic == "migration" && e.Migration == m.ID) || e.Topic == "alerts"
	})
}

func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	mig, db, comp, lvl, op := q.Get("migration"), q.Get("database"), q.Get("component"), q.Get("level"), q.Get("op")
	if mig != "" {
		if m, err := s.Orch.GetMigration(r.Context(), mig); err == nil {
			mig = m.ID
		}
	}
	return s.sse(w, r, func(e bus.Event) bool {
		if e.Topic != "logs" {
			return false
		}
		if mig != "" && e.Migration != mig {
			return false
		}
		if db == "" && comp == "" && lvl == "" && op == "" {
			return true
		}
		var rec logx.Record
		if json.Unmarshal(e.Data, &rec) != nil {
			return false
		}
		return (db == "" || rec.Database == db) && (comp == "" || rec.Component == comp) && (lvl == "" || logx.LevelAtLeast(rec.Level, lvl)) && (op == "" || rec.Op == op)
	})
}

func (s *Server) streamSystem(w http.ResponseWriter, r *http.Request) error {
	return s.sse(w, r, func(e bus.Event) bool { return e.Topic == "system" || e.Topic == "alerts" })
}

// ---------- OpenAPI

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) error {
	paths := map[string]map[string]any{}
	for _, rt := range s.routes {
		p := rt.Path
		if paths[p] == nil {
			paths[p] = map[string]any{}
		}
		op := map[string]any{"summary": rt.Summary, "responses": map[string]any{"200": map[string]any{"description": "OK"}, "default": map[string]any{"description": "Error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}}}}
		if !rt.public {
			op["security"] = []any{map[string]any{"session": []string{}}}
			op["x-required-role"] = rt.Role
		}
		var params []any
		for _, part := range strings.Split(p, "/") {
			if strings.HasPrefix(part, "{") {
				params = append(params, map[string]any{"name": strings.Trim(part, "{}"), "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
			}
		}
		if rt.Method != "GET" && !rt.noIdem {
			params = append(params, map[string]any{"name": "Idempotency-Key", "in": "header", "required": true, "schema": map[string]any{"type": "string"}},
				map[string]any{"name": "X-CSRF-Token", "in": "header", "required": !rt.public, "schema": map[string]any{"type": "string"}})
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		paths[p][strings.ToLower(rt.Method)] = op
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "Upwell API", "version": s.Version, "description": "Every action in the Upwell UI is available here. Long operations return 202 with an operation ID."},
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{"session": map[string]any{"type": "apiKey", "in": "cookie", "name": "upwell_session"}},
			"schemas":         map[string]any{"Error": map[string]any{"type": "object", "properties": map[string]any{"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "remediation": map[string]any{"type": "string"}, "details": map[string]any{}}}},
		},
	}
	return writeJSON(w, 200, doc)
}
