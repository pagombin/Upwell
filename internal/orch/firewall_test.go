package orch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// D88: the root helper applies the list Upwell writes; Upwell reads back
// what it allowed. Run here with a stand-in ufw that records its rules.
func TestFirewallHelperRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	fw := filepath.Join(dir, "fw")
	os.MkdirAll(bin, 0o755)
	os.MkdirAll(fw, 0o755)
	stub := `#!/bin/bash
st=$(dirname $0)/../rules; touch $st
case "$1" in
  status) echo "Status: active";;
  show) cat $st;;
  allow) echo "ufw $*" | sed "s/comment upwell cluster/comment 'upwell cluster'/" >> $st;;
  delete) shift; grep -v "^ufw $* comment" $st > $st.n; mv $st.n $st;;
esac
`
	os.WriteFile(filepath.Join(bin, "ufw"), []byte(stub), 0o755)
	// Stand-ins so the helper never touches this machine's kernel settings.
	os.WriteFile(filepath.Join(bin, "sysctl"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "modprobe"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "rules"), []byte("ufw allow proto tcp from 203.0.113.9 port 5432 comment 'upwell cluster'\n"), 0o644)
	os.WriteFile(filepath.Join(fw, "clusters"), []byte("# list\n159.89.220.21 5432\n10.0.0.5 25060\n1.2.3.4;reboot 5432\n300.1.1.1 5432\n"), 0o644)
	cmd := exec.Command("bash", "../../deploy/firewall/upwell-firewall")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "UPWELL_FIREWALL_DIR="+fw)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	}
	allowed, at := readFirewallStatus(filepath.Join(fw, "status"))
	if at == 0 {
		t.Fatal("no status time")
	}
	want := []string{"10.0.0.5 25060", "159.89.220.21 5432"}
	if m := missingAddrs(want, allowed); len(m) > 0 {
		t.Fatalf("not allowed: %v (status %v)", m, allowed)
	}
	if len(allowed) != 2 {
		t.Fatalf("allowed %v, want exactly %v", allowed, want)
	}
	rules, _ := os.ReadFile(filepath.Join(dir, "rules"))
	if strings.Contains(string(rules), "203.0.113.9") {
		t.Fatalf("a rule no longer listed was kept: %s", rules)
	}
	if strings.Contains(string(rules), "reboot") || strings.Contains(string(rules), "300.1.1.1") {
		t.Fatalf("an invalid line reached ufw: %s", rules)
	}
}
