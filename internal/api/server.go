// Package api serves the REST API, the live streams and the embedded UI.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/auth"
	"github.com/pagombin/upwell/internal/bus"
	"github.com/pagombin/upwell/internal/config"
	"github.com/pagombin/upwell/internal/logx"
	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
	"github.com/pagombin/upwell/internal/webui"
)

// Server is the HTTP layer.
type Server struct {
	Cfg      config.Config
	Store    *store.Store
	Auth     *auth.Service
	Audit    *audit.Log
	Orch     *orch.Orchestrator
	Settings *settings.Service
	Bus      *bus.Bus
	Log      *logx.Logger
	Version  string
	TLSInfo  func() TLSInfo
	Health   *Health

	mux    *http.ServeMux
	routes []route
	static fs.FS
}

// TLSInfo describes the serving certificate.
type TLSInfo struct {
	Fingerprint string `json:"fingerprint"`
	NotAfter    int64  `json:"not_after"`
	Subject     string `json:"subject"`
	SelfSigned  bool   `json:"self_signed"`
}

type route struct {
	Method, Path, Role, Summary string
	h                           handlerFunc
	public                      bool
	noIdem                      bool
}

type ctxKey int

const sessKey ctxKey = 1

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// APIError is the error body.
type APIError struct {
	Status      int    `json:"-"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
	Details     any    `json:"details,omitempty"`
}

func (e *APIError) Error() string { return e.Message }

func apiErr(status int, code, msg, fix string) error {
	return &APIError{Status: status, Code: code, Message: msg, Remediation: fix}
}

func badRequest(msg string) error { return apiErr(400, "bad_request", msg, "") }

// New builds the server and registers routes.
func New(s *Server) *Server {
	s.mux = http.NewServeMux()
	s.static = webui.FS()
	s.register()
	return s
}

func (s *Server) add(method, path, role, summary string, h handlerFunc) *route {
	r := route{Method: method, Path: path, Role: role, Summary: summary, h: h}
	s.routes = append(s.routes, r)
	return &s.routes[len(s.routes)-1]
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	for i := range s.routes {
		rt := s.routes[i]
		s.mux.HandleFunc(rt.Method+" "+rt.Path, s.wrap(rt))
	}
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, apiErr(404, "not_found", "No such API endpoint: "+r.Method+" "+r.URL.Path, ""))
	})
	s.mux.HandleFunc("/", s.serveUI)
	return securityHeaders(s.mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "SAMEORIGIN")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'")
		h.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func actor(r *http.Request) audit.Actor {
	if s, ok := r.Context().Value(sessKey).(auth.Session); ok {
		return audit.Actor{UserID: s.User.ID, Username: s.User.Username, IP: clientIP(r)}
	}
	return audit.Actor{IP: clientIP(r)}
}

func session(r *http.Request) auth.Session {
	s, _ := r.Context().Value(sessKey).(auth.Session)
	return s
}

type capture struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
	stream bool
}

func (c *capture) WriteHeader(code int) { c.status = code; c.ResponseWriter.WriteHeader(code) }
func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	if !c.stream && c.buf.Len() < 1<<20 {
		c.buf.Write(b)
	}
	return c.ResponseWriter.Write(b)
}
func (c *capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var idemLocks sync.Map // idempotency key -> *sync.Mutex

func (s *Server) wrap(rt route) http.HandlerFunc {
	mutating := rt.Method != http.MethodGet && rt.Method != http.MethodHead
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Logf("error", "api", "", "panic in %s %s: %v", r.Method, r.URL.Path, rec)
				writeErr(w, apiErr(500, "internal", "Something went wrong on the server; the error is in the app log.", ""))
			}
		}()
		if !rt.public {
			c, _ := r.Cookie("upwell_session")
			tok := ""
			if c != nil {
				tok = c.Value
			}
			sess, err := s.Auth.Resolve(r.Context(), tok)
			if err != nil {
				writeErr(w, apiErr(401, "unauthenticated", "Your session ended. Sign in again.", ""))
				return
			}
			if !auth.Allows(sess.User.Role, rt.Role) {
				writeErr(w, apiErr(403, "forbidden", "Your role ("+sess.User.Role+") cannot do this; it needs "+rt.Role+".", "Ask an Admin."))
				return
			}
			if mutating && !auth.CheckCSRF(sess, r.Header.Get("X-CSRF-Token")) {
				writeErr(w, apiErr(403, "csrf", "The request is missing its security token. Reload the page and try again.", ""))
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), sessKey, sess))
		}
		key := r.Header.Get("Idempotency-Key")
		var bodyHash string
		if mutating && !rt.noIdem {
			if key == "" {
				writeErr(w, apiErr(400, "idempotency_key_required", "Every change needs an Idempotency-Key header so repeated submissions act once.", ""))
				return
			}
			if len(key) > 200 {
				writeErr(w, badRequest("Idempotency-Key is too long."))
				return
			}
			lk, _ := idemLocks.LoadOrStore(key, &sync.Mutex{})
			mu := lk.(*sync.Mutex)
			mu.Lock()
			// Hash the request body so a reused key with another request is refused.
			reqBody, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
			sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), reqBody...))
			bodyHash = hex.EncodeToString(sum[:])
			var status int
			var body, prevHash string
			err := s.Store.DB.QueryRowContext(r.Context(), `SELECT status, response, body_hash FROM idempotency WHERE key=?`, key).Scan(&status, &body, &prevHash)
			if err == nil && prevHash != "" && prevHash != bodyHash {
				mu.Unlock()
				writeErr(w, apiErr(422, "idempotency_key_reused", "This Idempotency-Key was already used for a different request.", "Send a new key for a new request."))
				return
			}
			if err == nil {
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replay", "true")
				w.WriteHeader(status)
				io.WriteString(w, body)
				return
			}
			// Hold the key's lock for the whole request so a concurrent duplicate waits and replays.
			defer func() { mu.Unlock(); idemLocks.Delete(key) }()
		}
		cw := &capture{ResponseWriter: w, stream: strings.Contains(rt.Path, "/stream/")}
		err := rt.h(cw, r)
		if err != nil {
			writeErr(cw, err)
		}
		if mutating && !rt.noIdem && cw.status < 500 && cw.status != 0 {
			s.Store.DB.ExecContext(context.Background(), `INSERT OR IGNORE INTO idempotency(key,user_id,method,path,status,response,created_at,body_hash) VALUES (?,?,?,?,?,?,?,?)`,
				key, session(r).User.ID, r.Method, r.URL.Path, cw.status, cw.buf.String(), store.Now(), bodyHash)
		}
		if !strings.Contains(rt.Path, "/stream/") {
			lvl := "debug"
			if cw.status >= 500 {
				lvl = "error"
			}
			s.Log.Write(logx.Record{Level: lvl, Component: "api", Msg: fmt.Sprintf("%s %s %d %s", r.Method, r.URL.Path, cw.status, time.Since(start).Round(time.Millisecond))})
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// A nil slice is an empty list to clients, never null.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
		_, err := io.WriteString(w, "[]\n")
		return err
	}
	return json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *APIError
	var ue *orch.UserError
	switch {
	case errors.As(err, &ae):
	case errors.As(err, &ue):
		ae = &APIError{Status: 409, Code: ue.Code, Message: ue.Msg, Remediation: ue.Remediation}
		if strings.HasPrefix(ue.Code, "invalid") || ue.Code == "confirm" || ue.Code == "reason_required" || ue.Code == "too_long" {
			ae.Status = 400
		}
	case errors.Is(err, store.ErrNotFound):
		ae = &APIError{Status: 404, Code: "not_found", Message: "That item does not exist."}
	default:
		ae = &APIError{Status: 500, Code: "internal", Message: err.Error()}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ae.Status)
	json.NewEncoder(w).Encode(ae)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return badRequest("The request body is not valid JSON for this endpoint: " + err.Error())
	}
	return nil
}

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	f, err := s.static.Open(p)
	if err != nil {
		p = "index.html" // client-side routes
		f, err = s.static.Open(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	defer func() { _ = f.Close() }()
	st, _ := f.Stat()
	if st.IsDir() {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, p, st.ModTime(), f.(io.ReadSeeker))
}

// Health is the internal health model behind /healthz, /readyz and the watchdog.
type Health struct {
	mu    sync.Mutex
	beats map[string]time.Time
	Ready func() (bool, map[string]any)
}

// NewHealth returns an empty model.
func NewHealth() *Health { return &Health{beats: map[string]time.Time{}} }

// Beat records a heartbeat from a component loop.
func (h *Health) Beat(name string) {
	h.mu.Lock()
	h.beats[name] = time.Now()
	h.mu.Unlock()
}

// Beats returns the last heartbeat times.
func (h *Health) Beats() map[string]int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int64{}
	for k, v := range h.beats {
		out[k] = v.UnixMilli()
	}
	return out
}

// Alive reports whether every component beat within deadline.
func (h *Health) Alive(deadline time.Duration) (bool, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var late []string
	for k, v := range h.beats {
		if time.Since(v) > deadline {
			late = append(late, k)
		}
	}
	return len(late) == 0, late
}

// notifySocket is systemd's NOTIFY_SOCKET, taken out of the environment at
// start so child processes (pgcopydb, psql, systemctl) never write to it:
// with NotifyAccess=main systemd logs every such message as a warning.
var notifySocket = func() string {
	s := os.Getenv("NOTIFY_SOCKET")
	for _, k := range []string{"NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID"} {
		_ = os.Unsetenv(k)
	}
	return s
}()

// UnderSystemd reports whether systemd supervises this process.
func UnderSystemd() bool { return notifySocket != "" }

// SDNotify sends a message to systemd when NOTIFY_SOCKET is set.
func SDNotify(msg string) {
	sock := notifySocket
	if sock == "" {
		return
	}
	if strings.HasPrefix(sock, "@") {
		sock = "\x00" + sock[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	_, _ = c.Write([]byte(msg))
}
