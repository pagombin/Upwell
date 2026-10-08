package audit

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pagombin/upwell/internal/store"
)

func TestChain(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := &Log{Store: st}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := l.Append(ctx, Actor{Username: "admin", IP: "10.0.0.1"}, "settings.change", "heartbeat", map[string]any{"v": i}, map[string]any{"v": i + 1}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := l.Verify(ctx)
	if err != nil || !r.OK || r.Entries != 5 {
		t.Fatalf("verify: %+v %v", r, err)
	}
	// The table is append-only.
	if _, err := st.DB.Exec(`UPDATE audit_log SET action='x' WHERE seq=2`); err == nil {
		t.Fatal("UPDATE of an audit entry succeeded")
	}
	if _, err := st.DB.Exec(`DELETE FROM audit_log WHERE seq=2`); err == nil {
		t.Fatal("DELETE of an audit entry succeeded")
	}
	// Tampering underneath the triggers is detected.
	st.DB.Exec(`DROP TRIGGER IF EXISTS audit_no_update`)
	st.DB.Exec(`DROP TRIGGER IF EXISTS audit_log_no_update`)
	if _, err := st.DB.Exec(`UPDATE audit_log SET target='other' WHERE seq=3`); err != nil {
		t.Skipf("cannot simulate tampering: %v", err)
	}
	r, _ = l.Verify(ctx)
	if r.OK || r.BadSeq != 3 {
		t.Fatalf("tampering not detected: %+v", r)
	}
}
