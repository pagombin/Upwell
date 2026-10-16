package pg

import (
	"strings"
	"testing"
	"time"
)

// D98: the engine's TCP_USER_TIMEOUT comes from the setting; 0 leaves the
// kernel's own limit (no parameter at all).
func TestEngineURIUserTimeout(t *testing.T) {
	c := Conn{Host: "h", Port: 5432, User: "u", DBName: "d", SSLMode: "require"}
	if u := c.EngineURI(600 * time.Second); !strings.Contains(u, "tcp_user_timeout=600000") {
		t.Fatalf("600 s: %s", u)
	}
	if u := c.EngineURI(0); strings.Contains(u, "tcp_user_timeout") {
		t.Fatalf("0: %s", u)
	}
}

// D101: the engine's connections carry libpq keepalives, since pgcopydb
// applies its own defaults only to the URI it logs.
func TestEngineURIKeepalives(t *testing.T) {
	c := Conn{Host: "h", Port: 5432, User: "u", DBName: "d", SSLMode: "require"}
	for _, ut := range []time.Duration{0, 600 * time.Second} {
		u := c.EngineURI(ut)
		for _, want := range []string{"keepalives=1", "keepalives_idle=10", "keepalives_interval=10", "keepalives_count=60"} {
			if !strings.Contains(u, want) {
				t.Errorf("%v: %s lacks %s", ut, u, want)
			}
		}
	}
}
