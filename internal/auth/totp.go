package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238 TOTP is defined over HMAC-SHA1; authenticator apps expect it
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/store"
)

// TOTP (RFC 6238): 30-second steps, 6 digits, HMAC-SHA1, as every common
// authenticator app expects. A code is accepted one step either side of
// now (clock skew), and never twice: each user's last accepted step is
// kept and only a later step passes.
const (
	totpStep    = 30
	totpDigits  = 6
	totpSkew    = 1
	recoveryNum = 10
)

// Errors.
var (
	ErrMFARequired   = errors.New("enter the code from your authenticator app")
	ErrBadOTP        = errors.New("the authentication code is not correct")
	ErrTOTPEnabled   = errors.New("the second factor is already on; turn it off first")
	ErrTOTPNotSet    = errors.New("the second factor is not set up")
	ErrNoPending     = errors.New("start setting up the second factor first")
	ErrNeedsVault    = errors.New("the secrets store is not available")
	errBadTOTPSecret = errors.New("invalid TOTP secret")
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret in base32.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// TOTPCode returns the code for a secret at a time step.
func TOTPCode(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil || len(key) == 0 {
		return "", errBadTOTPSecret
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	h := m.Sum(nil)
	off := h[len(h)-1] & 0x0f
	v := binary.BigEndian.Uint32(h[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000), nil
}

// TOTPStep is the time step for t.
func TOTPStep(t time.Time) int64 { return t.Unix() / totpStep }

// matchTOTP returns the step a code matches within the skew window and
// after lastStep, or 0.
func matchTOTP(secret, code string, now time.Time, lastStep int64) int64 {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0
	}
	cur := TOTPStep(now)
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		st := cur + d
		if st <= lastStep {
			continue
		}
		want, err := TOTPCode(secret, st)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return st
		}
	}
	return 0
}

// OTPAuthURI is the URI authenticator apps import (by QR code or by hand).
func OTPAuthURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Enrollment is what the user copies into an authenticator app.
type Enrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"otpauth_uri"`
}

// TOTPEnroll starts setting up the second factor: a new secret, stored
// encrypted and pending until a code from it is confirmed.
func (s *Service) TOTPEnroll(ctx context.Context, userID string) (Enrollment, error) {
	if s.Vault == nil {
		return Enrollment{}, ErrNeedsVault
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return Enrollment{}, err
	}
	if u.TOTPEnabled {
		return Enrollment{}, ErrTOTPEnabled
	}
	var old sql.NullString
	s.Store.DB.QueryRowContext(ctx, `SELECT totp_secret_id FROM users WHERE id=?`, userID).Scan(&old)
	secret := NewTOTPSecret()
	id, err := s.Vault.Put(ctx, "totp:"+userID, secret)
	if err != nil {
		return Enrollment{}, err
	}
	if _, err := s.Store.DB.ExecContext(ctx, `UPDATE users SET totp_secret_id=?, totp_enabled_at=NULL, totp_last_step=0 WHERE id=?`, id, userID); err != nil {
		return Enrollment{}, err
	}
	if old.Valid && old.String != "" {
		_ = s.Vault.Delete(ctx, old.String)
	}
	return Enrollment{Secret: secret, URI: OTPAuthURI("Upwell", u.Username, secret)}, nil
}

func (s *Service) totpSecret(ctx context.Context, userID string) (secret string, enabled bool, lastStep int64, err error) {
	if s.Vault == nil {
		return "", false, 0, ErrNeedsVault
	}
	var id sql.NullString
	var at sql.NullInt64
	if err := s.Store.DB.QueryRowContext(ctx, `SELECT totp_secret_id, totp_enabled_at, totp_last_step FROM users WHERE id=?`, userID).Scan(&id, &at, &lastStep); err != nil {
		return "", false, 0, err
	}
	if !id.Valid || id.String == "" {
		return "", false, 0, ErrTOTPNotSet
	}
	secret, err = s.Vault.Get(ctx, id.String)
	return secret, at.Valid, lastStep, err
}

// acceptTOTP checks a code and records its step atomically, so the same code
// cannot be used twice even by two requests at once.
func (s *Service) acceptTOTP(ctx context.Context, userID, secret, code string, lastStep int64) (bool, error) {
	st := matchTOTP(secret, code, time.Now(), lastStep)
	if st == 0 {
		return false, nil
	}
	res, err := s.Store.DB.ExecContext(ctx, `UPDATE users SET totp_last_step=? WHERE id=? AND totp_last_step < ?`, st, userID, st)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// TOTPConfirm turns the second factor on with a code from the pending
// secret, and returns new one-time recovery codes (shown once).
func (s *Service) TOTPConfirm(ctx context.Context, userID, code string) ([]string, error) {
	secret, enabled, last, err := s.totpSecret(ctx, userID)
	if errors.Is(err, ErrTOTPNotSet) {
		return nil, ErrNoPending
	}
	if err != nil {
		return nil, err
	}
	if enabled {
		return nil, ErrTOTPEnabled
	}
	ok, err := s.acceptTOTP(ctx, userID, secret, code, last)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrBadOTP
	}
	if _, err := s.Store.DB.ExecContext(ctx, `UPDATE users SET totp_enabled_at=? WHERE id=?`, store.Now(), userID); err != nil {
		return nil, err
	}
	return s.newRecoveryCodes(ctx, userID)
}

func (s *Service) newRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id=?`, userID); err != nil {
		return nil, err
	}
	var codes []string
	now := store.Now()
	for i := 0; i < recoveryNum; i++ {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		raw := strings.ToLower(b32.EncodeToString(b))[:12]
		c := raw[:4] + "-" + raw[4:8] + "-" + raw[8:12]
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes(id,user_id,code_hash,created_at) VALUES (?,?,?,?)`, store.NewID(), userID, hashToken(normalizeRecovery(c)), now); err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, tx.Commit()
}

func normalizeRecovery(c string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(c)))
}

// useRecoveryCode spends one unused recovery code.
func (s *Service) useRecoveryCode(ctx context.Context, userID, code string) (bool, error) {
	n := normalizeRecovery(code)
	if len(n) != 12 {
		return false, nil
	}
	res, err := s.Store.DB.ExecContext(ctx, `UPDATE recovery_codes SET used_at=? WHERE user_id=? AND code_hash=? AND used_at IS NULL`, store.Now(), userID, hashToken(n))
	if err != nil {
		return false, err
	}
	k, _ := res.RowsAffected()
	return k == 1, nil
}

// CheckSecondFactor accepts a current TOTP code or an unused recovery code.
// It reports whether a recovery code was spent.
func (s *Service) CheckSecondFactor(ctx context.Context, userID, otp string) (ok, recovery bool, err error) {
	secret, enabled, last, err := s.totpSecret(ctx, userID)
	if err != nil {
		return false, false, err
	}
	if !enabled {
		return false, false, ErrTOTPNotSet
	}
	if ok, err := s.acceptTOTP(ctx, userID, secret, otp, last); err != nil || ok {
		return ok, false, err
	}
	ok, err = s.useRecoveryCode(ctx, userID, otp)
	return ok, ok, err
}

// TOTPDisable turns the second factor off; the caller checks a code first.
func (s *Service) TOTPDisable(ctx context.Context, userID string) error {
	var id sql.NullString
	s.Store.DB.QueryRowContext(ctx, `SELECT totp_secret_id FROM users WHERE id=?`, userID).Scan(&id)
	if _, err := s.Store.DB.ExecContext(ctx, `UPDATE users SET totp_secret_id=NULL, totp_enabled_at=NULL, totp_last_step=0 WHERE id=?`, userID); err != nil {
		return err
	}
	if _, err := s.Store.DB.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id=?`, userID); err != nil {
		return err
	}
	if id.Valid && id.String != "" && s.Vault != nil {
		_ = s.Vault.Delete(ctx, id.String)
	}
	return nil
}

// RegenerateRecoveryCodes replaces a user's recovery codes.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !u.TOTPEnabled {
		return nil, ErrTOTPNotSet
	}
	return s.newRecoveryCodes(ctx, userID)
}

// RecoveryCodesLeft counts a user's unused recovery codes.
func (s *Service) RecoveryCodesLeft(ctx context.Context, userID string) int {
	var n int
	s.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM recovery_codes WHERE user_id=? AND used_at IS NULL`, userID).Scan(&n)
	return n
}
