// Command upwell is the Upwell migration console: web server, API,
// orchestrator and collectors in one binary.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/pagombin/upwell/internal/api"
	"github.com/pagombin/upwell/internal/audit"
	"github.com/pagombin/upwell/internal/auth"
	"github.com/pagombin/upwell/internal/bus"
	"github.com/pagombin/upwell/internal/config"
	"github.com/pagombin/upwell/internal/engine/pgcopydb"
	"github.com/pagombin/upwell/internal/logx"
	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/runner"
	"github.com/pagombin/upwell/internal/secrets"
	"github.com/pagombin/upwell/internal/settings"
	"github.com/pagombin/upwell/internal/store"
	"github.com/pagombin/upwell/internal/tlsutil"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "selftest":
		err = selftest(os.Args[2:])
	case "init":
		err = initCmd(os.Args[2:])
	case "user":
		err = userCmd(os.Args[2:])
	case "migrate":
		err = migrateCmd(os.Args[2:])
	case "key":
		err = keyCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("upwell", Version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "upwell:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: upwell <command> [flags]

Commands:
  serve      Run the web console, API and orchestrator
  init       Create the master key, TLS certificate and setup token (install.sh runs this)
  selftest   Check config, store, master key and port before a restart
  user       Manage local users (user create --username NAME --role admin)
  migrate    Inspect migrations from the command line (migrate list | migrate show ID)
  key        Rotate the master key (key rotate)
  version    Print the version

Every command takes --config (default /etc/upwell/config.yaml).
`)
}

func loadConfig(fs *flag.FlagSet, args []string) (config.Config, error) {
	path := fs.String("config", envOr("UPWELL_CONFIG", "/etc/upwell/config.yaml"), "config file")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	return config.Load(*path)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type app struct {
	cfg   config.Config
	st    *store.Store
	vault *secrets.Vault
	log   *logx.Logger
	bus   *bus.Bus
	audit *audit.Log
	auth  *auth.Service
	set   *settings.Service
	orch  *orch.Orchestrator
}

func open(cfg config.Config, createKey bool, stderrLogs bool) (*app, error) {
	st, err := store.Open(cfg.StorePath())
	if err != nil {
		return nil, fmt.Errorf("opening the store: %w", err)
	}
	var nSecrets int
	st.DB.QueryRow(`SELECT count(*) FROM secrets`).Scan(&nSecrets)
	key, err := secrets.LoadOrCreateKey(cfg.MasterKey, createKey && nSecrets == 0)
	if err != nil {
		return nil, err
	}
	box, err := secrets.New(key)
	if err != nil {
		return nil, err
	}
	b := bus.New(8192)
	lg, err := logx.New(cfg.LogDir, st, b, "info", stderrLogs)
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, st: st, vault: &secrets.Vault{Box: box, Store: st}, log: lg, bus: b, audit: &audit.Log{Store: st}, auth: auth.NewService(st), set: &settings.Service{Store: st}}
	r, err := runner.New(cfg.Engine.Runner, filepath.Join(cfg.DataDir, "units"))
	if err != nil {
		return nil, err
	}
	eng := pgcopydb.New(cfg.Engine.Pgcopydb, r)
	a.orch = orch.New(orch.Deps{Config: cfg, Store: st, Vault: a.vault, Engine: eng, Runner: r, Logger: lg, Bus: b, Audit: a.audit, Settings: a.set})
	g, _ := a.set.Global(context.Background())
	lg.SetLevel(g.Str("log_level"))
	a.auth.IdleMin = func() int { v, _ := a.set.Global(context.Background()); return v.Int("session_idle_minutes") }
	a.auth.MaxHours = func() int { v, _ := a.set.Global(context.Background()); return v.Int("session_max_hours") }
	return a, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	for _, d := range []string{cfg.DataDir, cfg.LogDir, cfg.RunsDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	a, err := open(cfg, true, os.Getenv("UPWELL_LOG_STDERR") == "1")
	if err != nil {
		return err
	}
	defer a.st.Close()
	if err := a.vault.Canary(context.Background()); err != nil {
		return fmt.Errorf("the master key at %s does not decrypt stored secrets: %w", cfg.MasterKey, err)
	}
	if created, err := tlsutil.EnsureSelfSigned(cfg.TLSCert, cfg.TLSKey); err != nil {
		return fmt.Errorf("TLS certificate: %w", err)
	} else if created {
		a.log.Logf("info", "api", "", "generated a self-signed certificate at %s", cfg.TLSCert)
	}
	if n, _ := a.auth.CountUsers(context.Background()); n == 0 {
		if _, err := os.Stat(cfg.SetupToken); os.IsNotExist(err) {
			os.MkdirAll(filepath.Dir(cfg.SetupToken), 0o750)
			_ = os.WriteFile(cfg.SetupToken, []byte(auth.RandomToken(16)+"\n"), 0o600)
		}
		a.log.Logf("warn", "api", "", "no users yet: open the console and complete first-run setup with the token in %s", cfg.SetupToken)
	}
	tlsCfg, err := tlsutil.Config(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return err
	}
	health := api.NewHealth()
	srv := api.New(&api.Server{Cfg: cfg, Store: a.st, Auth: a.auth, Audit: a.audit, Orch: a.orch, Settings: a.set, Bus: a.bus, Log: a.log, Version: Version, Health: health,
		TLSInfo: func() api.TLSInfo {
			i, err := tlsutil.Inspect(cfg.TLSCert)
			if err != nil {
				return api.TLSInfo{}
			}
			return api.TLSInfo{Fingerprint: i.Fingerprint, NotAfter: i.NotAfter.UnixMilli(), Subject: i.Subject, SelfSigned: i.SelfSigned}
		}})
	a.orch.Heartbeat = health.Beat
	health.Beat("orchestrator")
	health.Ready = func() (bool, map[string]any) {
		err := a.st.DB.Ping()
		last := a.orch.LastLoop()
		running := !last.IsZero() && time.Since(last) < 2*time.Minute
		return err == nil && a.orch.Reconciled && running, map[string]any{"store_open": err == nil, "reconciled": a.orch.Reconciled, "orchestrator_running": running}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go a.orch.Run(ctx)
	addr := net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.HTTPSPort))
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), TLSConfig: tlsCfg, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	go func() {
		if err := hs.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Logf("error", "api", "", "HTTPS server stopped: %v", err)
			cancel()
		}
	}()
	if cfg.HTTPRedirect && cfg.HTTPPort > 0 {
		go func() {
			rs := &http.Server{Addr: net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.HTTPPort)), ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host, _, err := net.SplitHostPort(r.Host)
				if err != nil {
					host = r.Host
				}
				target := "https://" + host
				if cfg.HTTPSPort != 443 {
					target += ":" + strconv.Itoa(cfg.HTTPSPort)
				}
				http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
			})}
			if err := rs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.log.Logf("warn", "api", "", "HTTP redirect on port %d is off: %v", cfg.HTTPPort, err)
			}
		}()
	}
	a.log.Logf("info", "api", "", "Upwell %s listening on https://%s (runner %s)", Version, addr, a.orch.Runner().Name())
	// systemd watchdog: ping only while the orchestrator loop is alive.
	api.SDNotify("READY=1")
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ok, _ := health.Alive(90 * time.Second); ok {
					api.SDNotify("WATCHDOG=1")
				}
			}
		}
	}()
	<-ctx.Done()
	api.SDNotify("STOPPING=1")
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	hs.Shutdown(sctx)
	a.log.Logf("info", "api", "", "Upwell stopped; engine units keep running")
	a.log.Close()
	return nil
}

// initCmd prepares files the service cannot create itself under
// ProtectSystem=strict: the master key, the TLS certificate and, while no
// user exists, the first-run setup token. It never replaces existing files.
func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	for _, d := range []string{cfg.DataDir, cfg.LogDir, cfg.RunsDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	st, err := store.Open(cfg.StorePath())
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()
	var nSecrets int
	st.DB.QueryRow(`SELECT count(*) FROM secrets`).Scan(&nSecrets)
	if _, err := os.Stat(cfg.MasterKey); os.IsNotExist(err) && nSecrets > 0 {
		return fmt.Errorf("the store holds %d secrets but the master key %s is missing; restore the key before continuing", nSecrets, cfg.MasterKey)
	}
	if _, err := secrets.LoadOrCreateKey(cfg.MasterKey, true); err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	fmt.Println("master key: ok")
	created, err := tlsutil.EnsureSelfSigned(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return fmt.Errorf("TLS certificate: %w", err)
	}
	info, err := tlsutil.Inspect(cfg.TLSCert)
	if err != nil {
		return err
	}
	fmt.Printf("tls: %s (fingerprint %s)\n", map[bool]string{true: "created", false: "kept"}[created], info.Fingerprint)
	as := auth.NewService(st)
	if n, _ := as.CountUsers(context.Background()); n == 0 {
		if _, err := os.Stat(cfg.SetupToken); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(cfg.SetupToken), 0o750); err != nil {
				return err
			}
			if err := os.WriteFile(cfg.SetupToken, []byte(auth.RandomToken(16)+"\n"), 0o600); err != nil {
				return err
			}
		}
		fmt.Printf("setup token: %s\n", cfg.SetupToken)
	} else {
		fmt.Println("setup token: not needed (users exist)")
	}
	return nil
}

func selftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	fmt.Println("config: ok")
	st, err := store.Open(cfg.StorePath())
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()
	v, _ := st.SchemaVersion(context.Background())
	fmt.Printf("store: ok (schema %d)\n", v)
	key, err := secrets.LoadOrCreateKey(cfg.MasterKey, false)
	if err != nil {
		var n int
		st.DB.QueryRow(`SELECT count(*) FROM secrets`).Scan(&n)
		if n > 0 {
			return fmt.Errorf("master key: %w (the store has %d secrets)", err, n)
		}
		fmt.Println("master key: not created yet (no secrets stored)")
	} else {
		box, _ := secrets.New(key)
		if err := (&secrets.Vault{Box: box, Store: st}).Canary(context.Background()); err != nil {
			return fmt.Errorf("master key: %w", err)
		}
		fmt.Println("master key: ok (decrypts the canary)")
	}
	addr := net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.HTTPSPort))
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		fmt.Printf("port %s: bindable\n", addr)
	} else if strings.Contains(err.Error(), "address already in use") {
		fmt.Printf("port %s: in use (expected while upwell.service runs)\n", addr)
	} else {
		return fmt.Errorf("port %s: %w", addr, err)
	}
	fmt.Println("selftest passed")
	return nil
}

func userCmd(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: upwell user create --username NAME --role viewer|operator|admin (password from UPWELL_PASSWORD or stdin)")
	}
	fs := flag.NewFlagSet("user create", flag.ExitOnError)
	name := fs.String("username", "", "username")
	role := fs.String("role", "admin", "role")
	cfg, err := loadConfig(fs, args[1:])
	if err != nil {
		return err
	}
	pw := os.Getenv("UPWELL_PASSWORD")
	if pw == "" {
		fmt.Fprint(os.Stderr, "Password (at least 14 characters): ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(line, "\r\n")
	}
	st, err := store.Open(cfg.StorePath())
	if err != nil {
		return err
	}
	defer st.Close()
	svc := auth.NewService(st)
	u, err := svc.CreateUser(context.Background(), *name, pw, *role)
	if err != nil {
		return err
	}
	(&audit.Log{Store: st}).Append(context.Background(), audit.Actor{Username: "cli"}, "user.create", u.ID, nil, map[string]any{"username": u.Username, "role": u.Role, "via": "cli"})
	fmt.Printf("created %s (%s)\n", u.Username, u.Role)
	return nil
}

func migrateCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: upwell migrate list | upwell migrate show ID")
	}
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	cfg, err := loadConfig(fs, args[1:])
	if err != nil {
		return err
	}
	a, err := open(cfg, false, false)
	if err != nil {
		return err
	}
	defer a.st.Close()
	ctx := context.Background()
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	defer tw.Flush()
	switch args[0] {
	case "list":
		ms, err := a.orch.ListMigrations(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(tw, "ID\tNAME\tSTATE\tVERDICT")
		for _, m := range ms {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ShortID, m.Name, orch.Label(m.State), m.Flags.Verdict)
		}
	case "show":
		if fs.NArg() < 1 {
			return errors.New("usage: upwell migrate show ID")
		}
		m, err := a.orch.GetMigration(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n\n", m.ShortID, m.Name, orch.Label(m.State))
		dbs, _ := a.orch.ListDatabases(ctx, m.ID)
		fmt.Fprintln(tw, "DATABASE\tSTATE\tUNIT\tVERDICT\tLAST ERROR")
		for _, d := range dbs {
			if d.Include {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.SourceName, orch.Label(d.State), a.orch.Runner().UnitName(d.Instance), d.Verdict, d.LastError)
			}
		}
	default:
		return errors.New("usage: upwell migrate list | upwell migrate show ID")
	}
	return nil
}

func keyCmd(args []string) error {
	if len(args) == 0 || args[0] != "rotate" {
		return errors.New("usage: upwell key rotate (stop upwell.service first)")
	}
	fs := flag.NewFlagSet("key rotate", flag.ExitOnError)
	cfg, err := loadConfig(fs, args[1:])
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.StorePath())
	if err != nil {
		return err
	}
	defer st.Close()
	old, err := secrets.LoadOrCreateKey(cfg.MasterKey, false)
	if err != nil {
		return err
	}
	oldBox, _ := secrets.New(old)
	tmp := cfg.MasterKey + ".new"
	os.Remove(tmp)
	nk, err := secrets.LoadOrCreateKey(tmp, true)
	if err != nil {
		return err
	}
	newBox, _ := secrets.New(nk)
	if err := (&secrets.Vault{Box: oldBox, Store: st}).Rotate(context.Background(), newBox); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Rename(cfg.MasterKey, cfg.MasterKey+".old")
	if err := os.Rename(tmp, cfg.MasterKey); err != nil {
		return err
	}
	(&audit.Log{Store: st}).Append(context.Background(), audit.Actor{Username: "cli"}, "secrets.rotate_key", "", nil, nil)
	fmt.Println("master key rotated; the previous key is at", cfg.MasterKey+".old (delete it once the service runs)")
	return nil
}
