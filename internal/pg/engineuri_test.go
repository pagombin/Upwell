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
