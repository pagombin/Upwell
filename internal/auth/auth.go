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
	Store      *store.Store
	IdleMin    func() int
	MaxHours   func() int
	argonMem   uint32
	argonTime  uint32
	argonPar   uint8
}

// NewService returns a service with production Argon2id parameters.
func NewService(st *store.Store) *Service {
	return &Service{Store: st, IdleMin: func() int { return 30 }, MaxHours: func() int { return 12 }, argonMem: 64 * 1024, argonTime: 2, argonPar: 2}
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

// CheckPassword verifies pw against encoded.
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

// CreateUser adds a user.
func (s *Service) CreateUser(ctx context.Context, username, password, role string) (User, error) {
	if err := ValidateUsername(username); err != nil {
		return User{}, err
	}
	if err := ValidatePassword(password); err != nil {
		return User{}, err
	}
	if !ValidRole(role) {
		return User{}, fmt.Errorf("role must be viewer, operator or admin")
	}
	h, err := s.HashPassword(password)
	if err != nil {
		return User{}, err
	}
	u := User{ID: store.NewID(), Username: username, Role: role, CreatedAt: store.Now()}
	_, err = s.Store.DB.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		u.ID, u.Username, h, u.Role, u.CreatedAt, u.CreatedAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return User{}, fmt.Errorf("a user named %s already exists", username)
	}
	return u, err
}

// SetPassword changes a user's password.
func (s *Service) SetPassword(ctx context.Context, userID, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	h, err := s.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.Store.DB.ExecContext(ctx, `UPDATE users SET password_hash=?, failed_logins=0, locked_until=NULL, updated_at=? WHERE id=?`, h, store.Now(), userID)
	return err
}

// UpdateUser changes role and disabled state.
func (s *Service) UpdateUser(ctx context.Context, id, role string, disabled bool) error {
	if !ValidRole(role) {
		return fmt.Errorf("role must be viewer, operator or admin")
	}
	_, err := s.Store.DB.ExecContext(ctx, `UPDATE users SET role=?, disabled=?, updated_at=? WHERE id=?`, role, disabled, store.Now(), id)
	if disabled {
		s.Store.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id)
	}
	return err
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
func (s *Service) Login(ctx context.Context, username, password, ip, ua string) (string, Session, error) {
	var id, hash string
	var disabled bool
	var failed int
	var locked sql.NullInt64
	err := s.Store.DB.QueryRowContext(ctx, `SELECT id, password_hash, disabled, failed_logins, locked_until FROM users WHERE username=?`, username).
		Scan(&id, &hash, &disabled, &failed, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		// Spend similar time as a real check so usernames cannot be probed by timing.
		CheckPassword("$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g", password)
		return "", Session{}, ErrBadCredentials
	}
	if err != nil {
		return "", Session{}, err
	}
	now := store.Now()
	if locked.Valid && locked.Int64 > now {
		return "", Session{}, ErrLocked
	}
	if disabled {
		return "", Session{}, ErrDisabled
	}
	if !CheckPassword(hash, password) {
		failed++
		var lockUntil any
		if failed >= 10 {
			lockUntil = now + int64(15*time.Minute/time.Millisecond)
			failed = 0
		}
		s.Store.DB.ExecContext(ctx, `UPDATE users SET failed_logins=?, locked_until=COALESCE(?, locked_until) WHERE id=?`, failed, lockUntil, id)
		if lockUntil != nil {
			return "", Session{}, ErrLocked
		}
		return "", Session{}, ErrBadCredentials
	}
	s.Store.DB.ExecContext(ctx, `UPDATE users SET failed_logins=0, locked_until=NULL, last_login_at=? WHERE id=?`, now, id)
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
	rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomToken returns n random bytes hex-encoded.
func RandomToken(n int) string { return randHex(n) }
