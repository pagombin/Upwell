// Package audit writes the append-only, hash-chained audit log.
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/store"
)

// Entry is one audit record.
type Entry struct {
	Seq      int64           `json:"seq"`
	ID       string          `json:"id"`
	TS       int64           `json:"ts"`
	UserID   string          `json:"user_id,omitempty"`
	Username string          `json:"username,omitempty"`
	IP       string          `json:"ip,omitempty"`
	Action   string          `json:"action"`
	Target   string          `json:"target,omitempty"`
	Before   json.RawMessage `json:"before,omitempty"`
	After    json.RawMessage `json:"after,omitempty"`
	PrevHash string          `json:"prev_hash"`
	Hash     string          `json:"hash"`
}

// Log appends entries.
type Log struct {
	Store *store.Store
	// OnError, when set, hears about an entry that could not be written after
	// retries. Most callers ignore Append's error, so the failure is never
	// silent: without OnError it goes to the standard logger (stderr).
	OnError func(action string, err error)
	mu      sync.Mutex
}

// Actor identifies who acted.
type Actor struct {
	UserID, Username, IP string
}

// System is the actor for actions the app takes on its own.
var System = Actor{Username: "upwell"}

func digest(e Entry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%d|%s|%s|%s|%s|%s|%s|%s|%s", e.Seq, e.ID, e.TS, e.UserID, e.Username, e.IP, e.Action, e.Target, e.Before, e.After, e.PrevHash)
	return hex.EncodeToString(h.Sum(nil))
}

func toRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if r, ok := v.(json.RawMessage); ok {
		return r
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

const appendAttempts = 3

// Append writes one entry. before and after are any JSON-able values (or nil).
//
// The next sequence number is read and the entry inserted inside one
// immediate transaction, so a writer in another process (the CLI while the
// service runs) cannot take the same seq. A failed attempt is retried a few
// times. The entry is written even if ctx is cancelled (a client that
// disconnects must not erase the record of what it did).
func (l *Log) Append(ctx context.Context, a Actor, action, target string, before, after any) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	e := Entry{ID: store.NewID(), TS: store.Now(), UserID: a.UserID, Username: a.Username, IP: a.IP, Action: action, Target: target, Before: toRaw(before), After: toRaw(after)}
	var err error
	for i := 0; i < appendAttempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 100 * time.Millisecond)
		}
		if e, err = l.appendOnce(ctx, e); err == nil {
			return e, nil
		}
	}
	err = fmt.Errorf("audit entry %s not written: %w", action, err)
	if l.OnError != nil {
		l.OnError(action, err)
	} else {
		log.Print(err)
	}
	return e, err
}

func (l *Log) appendOnce(ctx context.Context, e Entry) (Entry, error) {
	err := l.Store.Immediate(ctx, func(c *sql.Conn) error {
		var prevSeq sql.NullInt64
		var prevHash sql.NullString
		if err := c.QueryRowContext(ctx, `SELECT seq, hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&prevSeq, &prevHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		e.Seq = prevSeq.Int64 + 1
		e.PrevHash = prevHash.String
		if e.PrevHash == "" {
			e.PrevHash = "genesis"
		}
		e.Hash = digest(e)
		_, err := c.ExecContext(ctx, `INSERT INTO audit_log(seq,id,ts,user_id,username,ip,action,target,before,after,prev_hash,hash) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.Seq, e.ID, e.TS, store.NullString(e.UserID), store.NullString(e.Username), store.NullString(e.IP), e.Action, e.Target, nullRaw(e.Before), nullRaw(e.After), e.PrevHash, e.Hash)
		return err
	})
	return e, err
}

func nullRaw(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

// List returns entries newest first.
func (l *Log) List(ctx context.Context, limit int, beforeSeq int64, action string) ([]Entry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT seq,id,ts,COALESCE(user_id,''),COALESCE(username,''),COALESCE(ip,''),action,target,COALESCE(before,''),COALESCE(after,''),prev_hash,hash FROM audit_log WHERE 1=1`
	var args []any
	if beforeSeq > 0 {
		q += ` AND seq < ?`
		args = append(args, beforeSeq)
	}
	if action != "" {
		q += ` AND action LIKE ?`
		args = append(args, action+"%")
	}
	q += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	return l.query(ctx, q, args...)
}

func (l *Log) query(ctx context.Context, q string, args ...any) ([]Entry, error) {
	rows, err := l.Store.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var before, after string
		if err := rows.Scan(&e.Seq, &e.ID, &e.TS, &e.UserID, &e.Username, &e.IP, &e.Action, &e.Target, &before, &after, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		if before != "" {
			e.Before = json.RawMessage(before)
		}
		if after != "" {
			e.After = json.RawMessage(after)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifyResult reports the chain check.
type VerifyResult struct {
	OK      bool   `json:"ok"`
	Entries int64  `json:"entries"`
	BadSeq  int64  `json:"bad_seq,omitempty"`
	Message string `json:"message"`
}

// Verify walks the whole chain.
func (l *Log) Verify(ctx context.Context) (VerifyResult, error) {
	all, err := l.query(ctx, `SELECT seq,id,ts,COALESCE(user_id,''),COALESCE(username,''),COALESCE(ip,''),action,target,COALESCE(before,''),COALESCE(after,''),prev_hash,hash FROM audit_log ORDER BY seq`)
	if err != nil {
		return VerifyResult{}, err
	}
	prev := "genesis"
	for i, e := range all {
		if e.Seq != int64(i+1) {
			return VerifyResult{OK: false, Entries: int64(len(all)), BadSeq: e.Seq, Message: fmt.Sprintf("entry %d is missing", i+1)}, nil
		}
		if e.PrevHash != prev || digest(e) != e.Hash {
			return VerifyResult{OK: false, Entries: int64(len(all)), BadSeq: e.Seq, Message: fmt.Sprintf("entry %d does not match the chain: the log was changed", e.Seq)}, nil
		}
		prev = e.Hash
	}
	return VerifyResult{OK: true, Entries: int64(len(all)), Message: fmt.Sprintf("all %d entries verified", len(all))}, nil
}
