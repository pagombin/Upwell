package orch

import (
	"strings"
	"testing"
)

// The droplet's base copy failure (2026-10-11): the target connection was
// lost in the middle of a COPY, then pgcopydb's error handling logged more
// errors, the last of which ("Failed to close snapshot ... on (null)") was
// what Upwell showed.
func TestRootCauseOfDropletFailure(t *testing.T) {
	var c classes
	for _, m := range []string{
		"[TARGET 0] SSL error: unexpected eof while reading",
		"[TARGET 0] Context: Failed to copy data to target",
		"Failed to copy data for table with oid 17013 and part number 0, see above for details",
		"BUG: call to pgsql_commit() without holding an open multi statement connection",
		`Failed to close snapshot "0000034B-00000005-1" on "(null)"`,
	} {
		c.lastError = m
		c.note(m)
	}
	got := c.rootCause()
	for _, want := range []string{"connection to the target was closed", "table with oid 17013", "SSL error: unexpected eof while reading"} {
		if !strings.Contains(got, want) {
			t.Errorf("root cause %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "close snapshot") {
		t.Errorf("root cause still names the follow-on error: %q", got)
	}
}

func TestRootCauseFirstError(t *testing.T) {
	var c classes
	for _, m := range []string{"Failed to create index foo: out of memory", "Terminating all processes in our process group"} {
		c.lastError = m
		c.note(m)
	}
	if got := c.rootCause(); !strings.HasPrefix(got, "Failed to create index foo: out of memory") {
		t.Fatalf("root cause %q", got)
	}
}

// The droplet's 240 GB base copy (2026-10-16): both CREATE INDEX connections
// timed out. libpq's error lines carry no [TARGET] prefix; only the lines
// after them do. Upwell missed the lost connection, so no incident report
// was written and the reason named only "Failed to get async query results".
func TestRootCauseOfIndexConnectionTimeout(t *testing.T) {
	var c classes
	for _, m := range []string{
		"Failed to get async query results:",
		"could not receive data from server: Connection timed out",
		"SSL SYSCALL error: Connection timed out",
		"[TARGET 0] connection pointer is NULL",
		"[TARGET 0] SQL query: CREATE INDEX fkey_stock_27 ON public.stock7 USING btree (s_i_id);",
		"[TARGET 0] SQL params:",
		"Failed to create index with oid 17132, see above for details",
	} {
		c.lastError = m
		c.note(m)
	}
	if c.connLost != "target" {
		t.Fatalf("lost connection not detected: %+v", c)
	}
	got := c.rootCause()
	for _, want := range []string{"connection to the target was closed", "creating index fkey_stock_27 on public.stock7", "Connection timed out"} {
		if !strings.Contains(got, want) {
			t.Errorf("root cause %q does not contain %q", got, want)
		}
	}
}
