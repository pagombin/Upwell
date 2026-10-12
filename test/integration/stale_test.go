//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/orch"
)

// TestStaleReadinessTurnsRed (audit finding 3, bug bash 5): when Upwell can
// no longer reach the target, the heartbeat-lag and schema conditions must
// not stay green on their last good measurement; they turn red and say how
// old the measurement is.
func TestStaleReadinessTurnsRed(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_stale")
	id := streamingMigration(t, e, "Stale readiness", db)
	var rd struct {
		Ready      bool             `json:"ready"`
		Conditions []orch.Condition `json:"conditions"`
	}
	e.waitFor(5*time.Minute, "readiness green", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		return rd.Ready
	})
	// New connections to the target database are refused; the engine's open
	// apply connection keeps working.
	superSQL(t, dstPort, "postgres", "REVOKE CONNECT ON DATABASE "+db+" FROM doadmin, PUBLIC")
	t.Cleanup(func() { superSQL(t, dstPort, "postgres", "GRANT CONNECT ON DATABASE "+db+" TO doadmin") })
	red := map[string]string{}
	e.waitFor(5*time.Minute, "lag and schema conditions to turn red", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		red = map[string]string{}
		for _, c := range rd.Conditions {
			if (c.Key == "lag" || c.Key == "schema") && !c.OK {
				red[c.Key] = c.Detail
			}
		}
		return len(red) == 2 && !rd.Ready
	})
	for k, d := range red {
		t.Logf("%s: %s", k, d)
		if !strings.Contains(d, "ago") {
			t.Errorf("%s does not say how old its measurement is: %q", k, d)
		}
	}
	superSQL(t, dstPort, "postgres", "GRANT CONNECT ON DATABASE "+db+" TO doadmin")
	e.waitFor(5*time.Minute, "readiness green again", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		return rd.Ready
	})
	e.abortAndClean(id, "Stale readiness")
}
