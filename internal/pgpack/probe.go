package pgpack

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/check"
	"github.com/pagombin/upwell/internal/pg"
)

// probe runs one check's queries and remembers the first error, so a check
// whose query failed can only end as a blocker that says it could not check
// (D43). Every preflight query goes through a probe.
type probe struct {
	r             *Runner
	ctx           context.Context
	id, scope, db string
	t             time.Time
	err           error
}

func (r *Runner) probe(ctx context.Context, id, scope, db string) *probe {
	return &probe{r: r, ctx: ctx, id: id, scope: scope, db: db, t: time.Now()}
}

// fail records err (the first one wins).
func (p *probe) fail(err error) {
	if err != nil && p.err == nil {
		p.err = err
	}
}

type probeRow struct {
	p   *probe
	row pgx.Row
}

func (x probeRow) Scan(dest ...any) error {
	err := x.row.Scan(dest...)
	x.p.fail(err)
	return err
}

// row is QueryRow whose error is recorded.
func (p *probe) row(c *pgx.Conn, sql string, args ...any) pgx.Row {
	return probeRow{p, pg.QueryRow(p.ctx, c, sql, args...)}
}

// rowMaybe is row for a query that may legitimately return no row: no row
// is reported as found=false, any other error is recorded.
func (p *probe) rowMaybe(c *pgx.Conn, sql string, args []any, dest ...any) (found bool) {
	err := pg.QueryRow(p.ctx, c, sql, args...).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	p.fail(err)
	return err == nil
}

// strings is pg.QueryStrings whose error is recorded.
func (p *probe) strings(c *pgx.Conn, sql string, args ...any) []string {
	v, err := pg.QueryStrings(p.ctx, c, sql, args...)
	p.fail(err)
	return v
}

// emit reports the check's result, or "could not check" after any error.
func (p *probe) emit(level string, hard bool, msg, fix string, ev map[string]any) {
	if p.err != nil {
		p.r.couldNotCheck(p.id, p.scope, p.db, p.err, ev, p.t)
		return
	}
	p.r.emit(p.id, p.scope, p.db, level, hard, msg, fix, ev, p.t)
}

func (p *probe) ok(msg string, ev map[string]any) { p.emit(check.OK, false, msg, "", ev) }

// couldNotCheck is the result of a check that could not run: a blocker,
// hard when the check itself is hard. A failed query is never an OK.
func (r *Runner) couldNotCheck(id, scope, db string, err error, ev map[string]any, t time.Time) {
	if ev == nil {
		ev = map[string]any{}
	}
	ev["error"] = err.Error()
	ev["checked"] = false
	r.emit(id, scope, db, check.Blocker, def(id).FailsAs == "hard", "Could not check: "+err.Error()+".",
		"Run preflight again. If it keeps failing, check the connection and the user's privileges on this cluster.", ev, t)
}
