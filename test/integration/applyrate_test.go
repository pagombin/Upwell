//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// TestApplyRateMeasured (E1): the built-in measurement runs the engine on
// scratch databases, reports a rate, suggests assumed_apply_tps, and leaves
// nothing behind; the suggestion can be applied as the setting.
func TestApplyRateMeasured(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_rate")
	seedDB(t, db, 10)
	id := e.defineMigration("Apply rate", []string{db}, nil)
	var r struct {
		OperationID string `json:"operation_id"`
	}
	e.do("POST", "/api/v1/migrations/"+id+"/apply-rate", nil, &r, 202)
	op := e.waitOp(r.OperationID, 15*time.Minute)
	for _, s := range op.Steps {
		t.Logf("step %-8s %-7s %s", s.Key, s.State, s.Detail)
	}
	if op.State != "done" {
		t.Fatalf("measurement failed: %s", op.Error)
	}
	var ms []orch.ApplyMeasurement
	e.do("GET", "/api/v1/apply-rate", nil, &ms, 200)
	if len(ms) == 0 || ms[0].TPS <= 0 || ms[0].Suggested <= 0 || ms[0].Txns == 0 {
		t.Fatalf("no usable measurement: %+v", ms)
	}
	m := ms[0]
	t.Logf("measured %.2f tps over %.0f s (%d txns, lower bound %v); suggested %.1f", m.TPS, m.Seconds, m.Txns, m.LowerBound, m.Suggested)
	name := ""
	if p, ok := op.Params["scratch"].(string); ok {
		name = p
	}
	if !strings.HasPrefix(name, "upwell_rate_") {
		t.Fatalf("scratch name %q", name)
	}
	for _, c := range []struct{ side, sql string }{
		{"source database", "SELECT count(*)::text FROM pg_database WHERE datname='" + name + "'"},
		{"source slot", "SELECT count(*)::text FROM pg_replication_slots WHERE slot_name='" + name + "'"},
	} {
		if n := q(t, srcConn, "defaultdb", c.sql); n != "0" {
			t.Errorf("%s left behind", c.side)
		}
	}
	for _, c := range []struct{ side, sql string }{
		{"target database", "SELECT count(*)::text FROM pg_database WHERE datname='" + name + "'"},
		{"target origin", "SELECT count(*)::text FROM pg_replication_origin WHERE roname='" + name + "'"},
	} {
		if n := q(t, dstConn, "defaultdb", c.sql); n != "0" {
			t.Errorf("%s left behind", c.side)
		}
	}
	// Calibrate: the suggestion is a valid assumed_apply_tps.
	var out any
	e.do("PATCH", "/api/v1/settings", map[string]any{"assumed_apply_tps": m.Suggested}, &out, 200)
}
