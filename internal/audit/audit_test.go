package audit

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pagombin/upwell/internal/store"
)

// Two store handles on one file stand in for the CLI and the service: their
// appends must never collide on seq or break the chain.
func TestConcurrentAppendAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	st1, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st1.Close()
	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	logs := []*Log{{Store: st1}, {Store: st2}}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := logs[i%2].Append(context.Background(), System, "test.append", "", nil, map[string]any{"i": i}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	r, err := logs[0].Verify(context.Background())
	if err != nil || !r.OK || r.Entries != 40 {
		t.Fatalf("verify: %+v %v", r, err)
	}
}

// A cancelled request context does not lose the entry.
func TestAppendIgnoresCancel(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Log{Store: st}).Append(ctx, System, "test.cancelled", "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

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
