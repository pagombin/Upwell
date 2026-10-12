package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/pagombin/upwell/internal/store"
)

// API tokens (overnight item 4): read-only bearer tokens for automation.
// Only a SHA-256 hash is stored; the token is shown once when created. A
// token expires, and a revoked token stops working on its next request
// (every request reads the store).

// Token scopes.
const (
	ScopeRead    = "read"    // every read-only API endpoint, as a Viewer
	ScopeMetrics = "metrics" // only the Prometheus endpoint
)

// TokenPrefix starts every API token, so a leaked one is recognisable.
const TokenPrefix = "upw_"

// Errors.
var (
	ErrTokenInvalid = errors.New("the API token is not valid, has expired or was revoked")
	ErrTokenScope   = errors.New("unknown token scope")
)

// APIToken is a token's record (never the token itself).
type APIToken struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	Scope      string `json:"scope"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
	RevokedAt  *int64 `json:"revoked_at,omitempty"`
	RevokedBy  string `json:"revoked_by,omitempty"`
}

// CreateAPIToken makes a token and returns it once with its record.
func (s *Service) CreateAPIToken(ctx context.Context, name, scope, by string, ttl time.Duration) (string, APIToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return "", APIToken{}, errors.New("give the token a name of at most 80 characters")
	}
	if scope != ScopeRead && scope != ScopeMetrics {
		return "", APIToken{}, ErrTokenScope
	}
	if ttl < time.Hour || ttl > 366*24*time.Hour {
		return "", APIToken{}, errors.New("a token expires after 1 to 366 days")
	}
	raw := TokenPrefix + randHex(24)
	now := store.Now()
	t := APIToken{ID: store.NewID(), Name: name, Prefix: raw[:len(TokenPrefix)+6], Scope: scope, CreatedBy: by, CreatedAt: now, ExpiresAt: now + ttl.Milliseconds()}
	_, err := s.Store.DB.ExecContext(ctx, `INSERT INTO api_tokens(id,name,token_hash,prefix,scope,created_by,created_at,expires_at) VALUES (?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, hashToken(raw), t.Prefix, t.Scope, t.CreatedBy, t.CreatedAt, t.ExpiresAt)
	return raw, t, err
}

// ListAPITokens returns every token's record, newest first.
func (s *Service) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.Store.DB.QueryContext(ctx, `SELECT id, name, prefix, scope, created_by, created_at, expires_at, last_used_at, revoked_at, COALESCE(revoked_by,'') FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		var used, rev sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Scope, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &used, &rev, &t.RevokedBy); err != nil {
			return nil, err
		}
		if used.Valid {
			t.LastUsedAt = &used.Int64
		}
		if rev.Valid {
			t.RevokedAt = &rev.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken revokes a token; it fails on its next use.
func (s *Service) RevokeAPIToken(ctx context.Context, id, by string) (APIToken, error) {
	res, err := s.Store.DB.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=?, revoked_by=? WHERE id=? AND revoked_at IS NULL`, store.Now(), by, id)
	if err != nil {
		return APIToken{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return APIToken{}, store.ErrNotFound
	}
	all, err := s.ListAPITokens(ctx)
	for _, t := range all {
		if t.ID == id {
			return t, err
		}
	}
	return APIToken{}, err
}

// ResolveAPIToken returns the record of a valid (unexpired, unrevoked)
// token, and records its use at most once a minute.
func (s *Service) ResolveAPIToken(ctx context.Context, raw string) (APIToken, error) {
	if !strings.HasPrefix(raw, TokenPrefix) {
		return APIToken{}, ErrTokenInvalid
	}
	var t APIToken
	var used sql.NullInt64
	err := s.Store.DB.QueryRowContext(ctx, `SELECT id, name, prefix, scope, created_by, created_at, expires_at, last_used_at FROM api_tokens
		WHERE token_hash=? AND revoked_at IS NULL`, hashToken(raw)).Scan(&t.ID, &t.Name, &t.Prefix, &t.Scope, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, ErrTokenInvalid
	}
	if err != nil {
		return APIToken{}, err
	}
	now := store.Now()
	if t.ExpiresAt <= now {
		return APIToken{}, ErrTokenInvalid
	}
	if !used.Valid || now-used.Int64 > 60_000 {
		s.Store.DB.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=?`, now, t.ID)
	}
	return t, nil
}
