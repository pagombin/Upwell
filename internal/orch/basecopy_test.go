package orch

import (
	"strings"
	"testing"
	"time"
)

// D102: the operator could not tell how long the 300 GB base copy took; the
// event said only that it finished.
func TestBaseCopyDoneMessage(t *testing.T) {
	m := baseCopyDoneMessage("migration_db", 2*time.Hour+57*time.Minute+12*time.Second, 330_712_301_568)
	for _, want := range []string{"Base copy of migration_db finished in 2h57m12s", "308.0 GiB", "29.7 MiB/s", "streaming changes"} {
		if !strings.Contains(m, want) {
			t.Errorf("%q lacks %q", m, want)
		}
	}
	if m := baseCopyDoneMessage("x", 0, 0); m != "Base copy of x finished; streaming changes" {
		t.Errorf("unknown duration: %q", m)
	}
}
