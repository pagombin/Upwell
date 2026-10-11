package pg

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// faultHook, when set, is consulted before every query made through the
// helpers below. Tests use it to make a chosen query fail and prove that no
// safety check passes when it could not run. It is never set in production.
var faultHook atomic.Pointer[func(sql string) error]

// SetFaultHook installs f (nil removes it) and returns a function that
// restores the previous hook.
func SetFaultHook(f func(sql string) error) (restore func()) {
	var prev *func(string) error
	if f == nil {
		prev = faultHook.Swap(nil)
	} else {
		prev = faultHook.Swap(&f)
	}
	return func() { faultHook.Store(prev) }
}

func fault(sql string) error {
	if f := faultHook.Load(); f != nil {
		return (*f)(sql)
	}
	return nil
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// QueryRow is c.QueryRow through the fault hook.
func QueryRow(ctx context.Context, c *pgx.Conn, sql string, args ...any) pgx.Row {
	if err := fault(sql); err != nil {
		return errRow{err}
	}
	return c.QueryRow(ctx, sql, args...)
}

// Query is c.Query through the fault hook.
func Query(ctx context.Context, c *pgx.Conn, sql string, args ...any) (pgx.Rows, error) {
	if err := fault(sql); err != nil {
		return nil, err
	}
	return c.Query(ctx, sql, args...)
}

// Exec is c.Exec through the fault hook.
func Exec(ctx context.Context, c *pgx.Conn, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := fault(sql); err != nil {
		return pgconn.CommandTag{}, err
	}
	return c.Exec(ctx, sql, args...)
}
