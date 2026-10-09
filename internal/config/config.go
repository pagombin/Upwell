// Package config loads /etc/upwell/config.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the install-level configuration. Migration defaults live in the
// settings table, not here.
type Config struct {
	ListenAddress string `yaml:"listen_address"`
	HTTPSPort     int    `yaml:"https_port"`
	HTTPRedirect  bool   `yaml:"http_redirect"`
	HTTPPort      int    `yaml:"http_port"`
	DataDir       string `yaml:"data_dir"`
	LogDir        string `yaml:"log_dir"`
	TLSCert       string `yaml:"tls_cert"`
	TLSKey        string `yaml:"tls_key"`
	MasterKey     string `yaml:"master_key"`
	SetupToken    string `yaml:"setup_token_file"`
	Engine        Engine `yaml:"engine"`
	// Dev enables development conveniences: the fixture seeding endpoint and
	// relaxed watchdog. Never set on a droplet.
	Dev bool `yaml:"dev"`
}

// Engine selects how engine units are run.
type Engine struct {
	Runner   string `yaml:"runner"` // auto, systemd, local
	Pgcopydb string `yaml:"pgcopydb"`
	PgBinDir string `yaml:"pg_bin_dir"`
}

// Default returns the production defaults.
func Default() Config {
	return Config{
		ListenAddress: "0.0.0.0",
		HTTPSPort:     443,
		HTTPRedirect:  true,
		HTTPPort:      80,
		DataDir:       "/var/lib/upwell",
		LogDir:        "/var/log/upwell",
		TLSCert:       "/etc/upwell/tls/cert.pem",
		TLSKey:        "/etc/upwell/tls/key.pem",
		MasterKey:     "/etc/upwell/master.key",
		SetupToken:    "/var/lib/upwell/setup-token", // under data_dir: the service deletes it after setup
		Engine:        Engine{Runner: "auto", Pgcopydb: "pgcopydb"},
	}
}

// Load reads path over the defaults. A missing file returns the defaults.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	// The setup token follows data_dir unless the file names it explicitly.
	c.SetupToken = ""
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	if c.SetupToken == "" && c.DataDir != "" {
		c.SetupToken = filepath.Join(c.DataDir, "setup-token")
	}
	return c, c.Validate()
}

// Validate checks required values.
func (c Config) Validate() error {
	if c.HTTPSPort <= 0 || c.HTTPSPort > 65535 {
		return fmt.Errorf("https_port %d is out of range", c.HTTPSPort)
	}
	if c.DataDir == "" || c.LogDir == "" {
		return fmt.Errorf("data_dir and log_dir are required")
	}
	switch c.Engine.Runner {
	case "auto", "systemd", "local":
	default:
		return fmt.Errorf("engine.runner must be auto, systemd or local, not %q", c.Engine.Runner)
	}
	return nil
}

// StorePath is the SQLite file.
func (c Config) StorePath() string { return filepath.Join(c.DataDir, "store.db") }

// RunsDir holds one directory per engine run.
func (c Config) RunsDir() string { return filepath.Join(c.DataDir, "runs") }
