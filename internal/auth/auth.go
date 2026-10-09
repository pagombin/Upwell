// Package auth implements local users, Argon2id passwords, sessions with
// idle and absolute timeouts, lockout, CSRF tokens and role checks.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/pagombin/upwell/internal/store"
)

// Roles, lowest to highest.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

var roleRank = map[string]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// Allows reports whether role has at least min.
func Allows(role, min string) bool { return roleRank[role] >= roleRank[min] && roleRank[min] > 0 }

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool { return roleRank[r] > 0 }

// MinPasswordLen is the minimum password length.
const MinPasswordLen = 14

// Errors.
var (
	ErrBadCredentials = errors.New("the username or password is not correct")
	ErrLocked         = errors.New("this account is locked for 15 minutes after 10 failed sign-ins")
	ErrDisabled       = errors.New("this account is disabled")
	ErrNoSession      = errors.New("not signed in")
	ErrAlreadySetUp   = errors.New("upwell is already set up")
)

// MaxConcurrentVerifies bounds how many Argon2id verifications (64 MiB each
// with production parameters) run at once, so a burst of sign-in attempts
// cannot exhaust memory.
const MaxConcurrentVerifies = 4

// Lockout policy.
const (
	lockoutThreshold = 10
	lockoutDuration  = 15 * time.Minute
)

// User is a local account.
type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
	LastLoginAt *int64 `json:"last_login_at,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// Session is a signed-in browser session.
type Session struct {
	ID        string
	User      User
	CSRF      string
	ExpiresAt int64
}

// Service holds auth state.
type Service struct {
	Store     *store.Store
	IdleMin   func() int
	MaxHours  func() int
	argonMem  uint32
	argonTime uint32
	argonPar  uint8

	verifySem chan struct{}
	dummyOnce sync.Once
	dummy     string
}

// NewService returns a service with production Argon2id parameters.
func NewService(st *store.Store) *Service {
	return &Service{Store: st, IdleMin: func() int { return 30 }, MaxHours: func() int { return 12 }, argonMem: 64 * 1024, argonTime: 2, argonPar: 2,
		verifySem: make(chan struct{}, MaxConcurrentVerifies)}
}

// FastHashForTests lowers Argon2 cost; tests only.
func (s *Service) FastHashForTests() { s.argonMem, s.argonTime, s.argonPar = 1024, 1, 1 }

// HashPassword returns an encoded Argon2id hash.
func (s *Service) HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(pw), salt, s.argonTime, s.argonMem, s.argonPar, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", s.argonMem, s.argonTime, s.argonPar,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h)), nil
}

// CheckPassword verifies pw against encoded. Prefer the Service methods,
// which bound how many verifications run at once.
func CheckPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// verify checks pw against encoded while holding one of the bounded
// verification slots. It returns ctx's error if no slot frees up in time.
func (s *Service) verify(ctx context.Context, encoded, pw string) (bool, error) {
	if s.verifySem == nil {
		return CheckPassword(encoded, pw), nil
	}
	select {
	case s.verifySem <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-s.verifySem }()
	return CheckPassword(encoded, pw), nil
}

// dummyHash is a hash built once with this service's real parameters. It is
// checked for unknown usernames so they cost as much time as known ones.
func (s *Service) dummyHash() string {
	s.dummyOnce.Do(func() {
		h, err := s.HashPassword(randHex(16))
		if err != nil {
			h = fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g", s.argonMem, s.argonTime, s.argonPar)
		}
		s.dummy = h
	})
	return s.dummy
}

// VerifyUserPassword checks pw against the stored hash of userID.
func (s *Service) VerifyUserPassword(ctx context.Context, userID, pw string) (bool, error) {
	var hash string
	err := s.Store.DB.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		_, verr := s.verify(ctx, s.dummyHash(), pw)
		return false, verr
	}
	if err != nil {
		return false, err
	}
	return s.verify(ctx, hash, pw)
}

// UserIDByName returns the id of the user with this username, or "" if none.
func (s *Service) UserIDByName(ctx context.Context, username string) (string, error) {
	var id string
	err := s.Store.DB.QueryRowContext(ctx, `SELECT id FROM users WHERE username=?`, username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ValidatePassword enforces the password policy.
func ValidatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLen {
		return fmt.Errorf("passwords need at least %d characters", MinPasswordLen)
	}
	return nil
}

// ValidateUsername checks the username pattern.
func ValidateUsername(u string) error {
	if len(u) < 2 || len(u) > 64 {
		return errors.New("usernames are 2 to 64 characters")
	}
	for _, r := range u {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == '@') {
			return errors.New("usernames use lowercase letters, digits, dot, dash, underscore and @")
		}
	}
	return nil
}

// CountUsers returns the number of users.
func (s *Service) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// prepareUser validates a new user and hashes the password.
func (s *Service) prepareUser(username, password, role string) (User, string, error) {
	if err := ValidateUsername(username); err != nil {
		return User{}, "", err
	}
	if err := ValidatePassword(password); err != nil {
		return User{}, "", err
	}
	if !ValidRole(role) {
		return User{}, "", fmt.Errorf("role must be viewer, operator or admin")
	}
	h, err := s.HashPassword(password)
	if err != nil {
		return User{}, "", err
	}
	return User{ID: store.NewID(), Username: username, Role: role, CreatedAt: store.Now()}, h, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertUser(ctx context.Context, db execer, u User, hash string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		u.ID, u.Username, hash, u.Role, u.CreatedAt, u.CreatedAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("a user named %s already exists", u.Username)
	}
	return err
}

// CreateUser adds a user.
func (s *Service) CreateUser(ctx context.Context, username, password, role string) (User, error) {
	u, h, err := s.prepareUser(username, password, role)
	if err != nil {
		return User{}, err
	}
	if err := insertUser(ctx, s.Store.DB, u, h); err != nil {
		return User{}, err
	}
	return u, nil
}

// CreateFirstAdmin creates the first Admin only while no user exists. The
// count and the insert run in one immediate (write-locked) transaction, so two
// concurrent first-run setups cannot both succeed. It returns ErrAlreadySetUp
// when a user exists.
func (s *Service) CreateFirstAdmin(ctx context.Context, username, password string) (User, error) {
	u, h, err := s.prepareUser(username, password, RoleAdmin)
	if err != nil {
		return User{}, err
	}
	err = s.Store.Immediate(ctx, func(c *sql.Conn) error {
		var n int
		if err := c.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadySetUp
		}
		return insertUser(ctx, c, u, h)
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// SetPassword changes a user's password and signs out the user's other
// sessions. keepSessionID, when not empty, is the one session kept (the
// caller's own, on a self-service change).
func (s *Service) SetPassword(ctx context.Context, userID, password, keepSessionID string) error {
	p := password
	return s.UpdateUser(ctx, userID, UserChange{Password: &p, KeepSessionID: keepSessionID})
}

// UserChange is a partial update; nil fields stay as they are.
type UserChange struct {
	Role     *string
	Disabled *bool
	Password *string
	// KeepSessionID is the session kept when the password changes (the
	// caller's own when they change their own password).
	KeepSessionID string
}

// UpdateUser applies a partial change in one transaction. Everything is
// validated (and the password hashed) before anything is written. Disabling a
// user signs out all their sessions; setting a password signs out all but
// KeepSessionID.
func (s *Service) UpdateUser(ctx context.Context, id string, ch UserChange) error {
	if ch.Role != nil && !ValidRole(*ch.Role) {
		return fmt.Errorf("role must be viewer, operator or admin")
	}
	var hash string
	if ch.Password != nil {
		if err := ValidatePassword(*ch.Password); err != nil {
			return err
		}
		h, err := s.HashPassword(*ch.Password)
		if err != nil {
			return err
		}
		hash = h
	}
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := store.Now()
	if ch.Role != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET role=?, updated_at=? WHERE id=?`, *ch.Role, now, id); err != nil {
			return err
		}
	}
	if ch.Disabled != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled=?, updated_at=? WHERE id=?`, *ch.Disabled, now, id); err != nil {
			return err
		}
		if *ch.Disabled {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id); err != nil {
				return err
			}
		}
	}
	if ch.Password != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?, failed_logins=0, locked_until=NULL, updated_at=? WHERE id=?`, hash, now, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND id<>?`, id, ch.KeepSessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteUser removes a user.
func (s *Service) DeleteUser(ctx context.Context, id string) error {
	_, err := s.Store.DB.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	return err
}

// ListUsers returns all users.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT id, username, role, disabled, last_login_at, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var last sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &last, &u.CreatedAt); err != nil {
			return nil, err
		}
		if last.Valid {
			u.LastLoginAt = &last.Int64
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUser returns one user.
func (s *Service) GetUser(ctx context.Context, id string) (User, error) {
	var u User
	var last sql.NullInt64
	err := s.Store.DB.QueryRowContext(ctx, `SELECT id, username, role, disabled, last_login_at, created_at FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &last, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, store.ErrNotFound
	}
	if last.Valid {
		u.LastLoginAt = &last.Int64
	}
	return u, err
}

// Login checks credentials and creates a session. It returns the raw token.
//
// The password is always verified (for unknown users against a dummy hash
// built with the real parameters) before the account's disabled state is
// revealed, so neither the answer nor its timing tells whether a username
// exists. A locked account answers ErrLocked whatever the password, so
// guessing cannot continue while it is locked.
func (s *Service) Login(ctx context.Context, username, password, ip, ua string) (string, Session, error) {
	var id, hash string
	var disabled bool
	var locked sql.NullInt64
	err := s.Store.DB.QueryRowContext(ctx, `SELECT id, password_hash, disabled, locked_until FROM users WHERE username=?`, username).
		Scan(&id, &hash, &disabled, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.verify(ctx, s.dummyHash(), password); err != nil {
			return "", Session{}, err
		}
		return "", Session{}, ErrBadCredentials
	}
	if err != nil {
		return "", Session{}, err
	}
	ok, err := s.verify(ctx, hash, password)
	if err != nil {
		return "", Session{}, err
	}
	now := store.Now()
	if locked.Valid && locked.Int64 > now {
		return "", Session{}, ErrLocked
	}
	if !ok {
		lockedNow, err := s.recordFailure(ctx, id, now)
		if err != nil {
			return "", Session{}, err
		}
		if lockedNow {
			return "", Session{}, ErrLocked
		}
		return "", Session{}, ErrBadCredentials
	}
	if disabled {
		return "", Session{}, ErrDisabled
	}
	if _, err := s.Store.DB.ExecContext(ctx, `UPDATE users SET failed_logins=0, locked_until=NULL, last_login_at=? WHERE id=?`, now, id); err != nil {
		return "", Session{}, err
	}
	tok := randHex(32)
	sess := Session{ID: store.NewID(), CSRF: randHex(24), ExpiresAt: now + int64(s.MaxHours())*3600*1000}
	_, err = s.Store.DB.ExecContext(ctx, `INSERT INTO sessions(id,user_id,token_hash,csrf_token,ip,user_agent,created_at,last_seen_at,expires_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		sess.ID, id, hashToken(tok), sess.CSRF, ip, ua, now, now, sess.ExpiresAt)
	if err != nil {
		return "", Session{}, err
	}
	sess.User, err = s.GetUser(ctx, id)
	return tok, sess, err
}

// recordFailure counts one failed sign-in in a single atomic statement, so
// parallel guesses all count, and locks the account at the threshold. It
// reports whether the account is now locked.
func (s *Service) recordFailure(ctx context.Context, id string, now int64) (bool, error) {
	lockUntil := now + lockoutDuration.Milliseconds()
	var lockedUntil sql.NullInt64
	err := s.Store.DB.QueryRowContext(ctx, `UPDATE users SET
		locked_until = CASE WHEN failed_logins + 1 >= ? THEN ? ELSE locked_until END,
		failed_logins = CASE WHEN failed_logins + 1 >= ? THEN 0 ELSE failed_logins + 1 END
		WHERE id=? RETURNING locked_until`, lockoutThreshold, lockUntil, lockoutThreshold, id).Scan(&lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return lockedUntil.Valid && lockedUntil.Int64 > now, nil
}

// Resolve returns the session for a raw token, enforcing timeouts.
func (s *Service) Resolve(ctx context.Context, tok string) (Session, error) {
	if tok == "" {
		return Session{}, ErrNoSession
	}
	var sess Session
	var userID string
	var lastSeen int64
	err := s.Store.DB.QueryRowContext(ctx, `SELECT id, user_id, csrf_token, last_seen_at, expires_at FROM sessions WHERE token_hash=?`, hashToken(tok)).
		Scan(&sess.ID, &userID, &sess.CSRF, &lastSeen, &sess.ExpiresAt)
	if err != nil {
		return Session{}, ErrNoSession
	}
	now := store.Now()
	if now > sess.ExpiresAt || now-lastSeen > int64(s.IdleMin())*60*1000 {
		s.Store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, sess.ID)
		return Session{}, ErrNoSession
	}
	if now-lastSeen > 15000 {
		s.Store.DB.ExecContext(ctx, `UPDATE sessions SET last_seen_at=? WHERE id=?`, now, sess.ID)
	}
	sess.User, err = s.GetUser(ctx, userID)
	if err != nil || sess.User.Disabled {
		return Session{}, ErrNoSession
	}
	return sess, nil
}

// Logout deletes the session.
func (s *Service) Logout(ctx context.Context, sessionID string) {
	s.Store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, sessionID)
}

// CheckCSRF compares the header token with the session's.
func CheckCSRF(sess Session, header string) bool {
	return header != "" && subtle.ConstantTimeCompare([]byte(sess.CSRF), []byte(header)) == 1
}

func hashToken(t string) string { h := sha256.Sum256([]byte(t)); return hex.EncodeToString(h[:]) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand never fails on Linux
	return hex.EncodeToString(b)
}

// RandomToken returns n random bytes hex-encoded.
func RandomToken(n int) string { return randHex(n) }
