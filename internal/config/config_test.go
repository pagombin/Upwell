package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetupTokenFollowsDataDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("data_dir: /srv/upwell\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.SetupToken != "/srv/upwell/setup-token" {
		t.Fatalf("setup token %q", c.SetupToken)
	}
	if err := os.WriteFile(p, []byte("data_dir: /srv/upwell\nsetup_token_file: /etc/upwell/setup-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ = Load(p); c.SetupToken != "/etc/upwell/setup-token" {
		t.Fatalf("explicit setup token %q", c.SetupToken)
	}
	if d := Default(); d.SetupToken != filepath.Join(d.DataDir, "setup-token") {
		t.Fatalf("default setup token %q", d.SetupToken)
	}
}
