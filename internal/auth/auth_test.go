package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/store"
)

const testPW = "correct-horse-battery"

func newTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewService(st)
	s.FastHashForTests()
	return s
}

func failedLogins(t *testing.T, s *Service, id string) (int, bool) {
	t.Helper()
	var n int
	var locked *int64
	if err := s.Store.DB.QueryRow(`SELECT failed_logins, locked_until FROM users WHERE id=?`, id).Scan(&n, &locked); err != nil {
		t.Fatal(err)
	}
	return n, locked != nil && *locked > store.Now()
}

// Parallel wrong guesses all count: none is lost to a read-modify-write race.
func TestFailedLoginCounterIsAtomic(t *testing.T) {
	s := newTestService(t)
	u, err := s.CreateUser(context.Background(), "alice", testPW, RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Login(context.Background(), "alice", "wrong-password-xx", "", ""); !errors.Is(err, ErrBadCredentials) {
				t.Errorf("login: %v", err)
			}
		}()
	}
	wg.Wait()
	if n, locked := failedLogins(t, s, u.ID); n != 7 || locked {
		t.Fatalf("after 7 parallel failures: failed=%d locked=%v", n, locked)
	}
	for i := 0; i < 2; i++ {
		s.Login(context.Background(), "alice", "wrong-password-xx", "", "")
	}
	if _, _, err := s.Login(context.Background(), "alice", "wrong-password-xx", "", ""); !errors.Is(err, ErrLocked) {
		t.Fatalf("10th failure: %v", err)
	}
	if _, locked := failedLogins(t, s, u.ID); !locked {
		t.Fatal("not locked after 10 failures")
	}
	// While locked even the right password is refused.
	if _, _, err := s.Login(context.Background(), "alice", testPW, "", ""); !errors.Is(err, ErrLocked) {
		t.Fatalf("right password while locked: %v", err)
	}
}

// A disabled account is only revealed to someone who knows its password.
func TestDisabledNotRevealedWithoutPassword(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "bob", testPW, RoleViewer)
	yes := true
	if err := s.UpdateUser(ctx, u.ID, UserChange{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login(ctx, "bob", "wrong-password-xx", "", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password on disabled user: %v", err)
	}
	if _, _, err := s.Login(ctx, "nobody", "wrong-password-xx", "", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	if _, _, err := s.Login(ctx, "bob", testPW, "", ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("right password on disabled user: %v", err)
	}
}

func TestDummyHashUsesServiceParameters(t *testing.T) {
	s := newTestService(t)
	if !strings.Contains(s.dummyHash(), "$m=1024,t=1,p=1$") {
		t.Fatalf("dummy hash %s", s.dummyHash())
	}
	p := NewService(s.Store)
	if !strings.Contains(p.dummyHash(), "$m=65536,t=2,p=2$") {
		t.Fatalf("production dummy hash %s", p.dummyHash())
	}
}

func countSessions(t *testing.T, s *Service, userID string) int {
	t.Helper()
	var n int
	s.Store.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=?`, userID).Scan(&n)
	return n
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "carol", testPW, RoleViewer)
	_, keep, err := s.Login(ctx, "carol", testPW, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Login(ctx, "carol", testPW, "", "")
	s.Login(ctx, "carol", testPW, "", "")
	if err := s.SetPassword(ctx, u.ID, "another-long-password", keep.ID); err != nil {
		t.Fatal(err)
	}
	if n := countSessions(t, s, u.ID); n != 1 {
		t.Fatalf("sessions after self-service change: %d", n)
	}
	// An Admin reset (no session kept) ends them all.
	np := "a-third-long-password"
	if err := s.UpdateUser(ctx, u.ID, UserChange{Password: &np}); err != nil {
		t.Fatal(err)
	}
	if n := countSessions(t, s, u.ID); n != 0 {
		t.Fatalf("sessions after reset: %d", n)
	}
}

func TestUpdateUserPartialAndAtomic(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "dave", testPW, RoleViewer)
	yes := true
	op := RoleOperator
	if err := s.UpdateUser(ctx, u.ID, UserChange{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	// A change without Disabled leaves the user disabled.
	if err := s.UpdateUser(ctx, u.ID, UserChange{Role: &op}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUser(ctx, u.ID)
	if !got.Disabled || got.Role != RoleOperator {
		t.Fatalf("after role-only change: %+v", got)
	}
	// A bad password changes nothing, not even the role sent with it.
	short, admin := "short", RoleAdmin
	if err := s.UpdateUser(ctx, u.ID, UserChange{Role: &admin, Password: &short}); err == nil {
		t.Fatal("short password accepted")
	}
	got, _ = s.GetUser(ctx, u.ID)
	if got.Role != RoleOperator {
		t.Fatalf("role changed despite the bad password: %+v", got)
	}
}

func TestCreateFirstAdminOnlyOnce(t *testing.T) {
	s := newTestService(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateFirstAdmin(context.Background(), "admin"+string(rune('a'+i)), testPW)
			if err == nil {
				mu.Lock()
				created++
				mu.Unlock()
			} else if !errors.Is(err, ErrAlreadySetUp) {
				t.Errorf("setup %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if n, _ := s.CountUsers(context.Background()); created != 1 || n != 1 {
		t.Fatalf("created %d admins, %d users", created, n)
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewRateLimiter(10, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("11th attempt allowed")
	}
	if !l.Allow("5.6.7.8") {
		t.Fatal("another address refused")
	}
	now = now.Add(6 * time.Second) // one token back
	if !l.Allow("1.2.3.4") || l.Allow("1.2.3.4") {
		t.Fatal("refill is not one token per 6s")
	}
}
