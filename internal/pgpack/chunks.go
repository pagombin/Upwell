package pgpack

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pagombin/upwell/internal/pg"
)

// Full comparison of large tables by key range (D91). A table too large for
// the checksum time budget was only checked for being empty; now, when
// verify_large_tables is "full", it is split into ranges of its primary key's
// first column, and each range's row count and order-independent hash
// (rowsHash) are compared on both sides, several ranges at once. A table
// without a usable key is hashed whole.

// ChunkTracker remembers ranges already compared equal in this verification
// (keyed by the caller to the cutover), so an interrupted one resumes.
type ChunkTracker interface {
	Done(table, lo, hi string) bool
	Record(table, lo, hi string, n int64)
}

// ChunkProgress reports compared ranges: done of total, the table being compared.
type ChunkProgress func(done, total int, table string)

type keyInfo struct {
	col  string // quoted column name
	typ  string // its type, for casts
	ok   bool
	desc string
}

// tableKey returns the first column of the table's primary key, or of a
// unique index whose columns are all NOT NULL.
func tableKey(ctx context.Context, c *pgx.Conn, table string) (keyInfo, error) {
	var col, typ *string
	err := pg.QueryRow(ctx, c, `SELECT quote_ident(a.attname), format_type(a.atttypid, a.atttypmod)
		FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
		WHERE i.indrelid = $1::regclass AND i.indisunique AND i.indisvalid AND i.indpred IS NULL AND i.indexprs IS NULL
		  AND NOT EXISTS (SELECT 1 FROM unnest(i.indkey) k JOIN pg_attribute x ON x.attrelid = i.indrelid AND x.attnum = k WHERE NOT x.attnotnull)
		ORDER BY i.indisprimary DESC, i.indnatts LIMIT 1`, table).Scan(&col, &typ)
	if err != nil && err != pgx.ErrNoRows {
		return keyInfo{}, err
	}
	if col == nil {
		return keyInfo{}, nil
	}
	return keyInfo{col: *col, typ: *typ, ok: true, desc: *col + " (" + *typ + ")"}, nil
}

// chunkBounds samples the source's keys and returns n-1 increasing bounds
// (as text) that split the table into about n ranges. Read-only (TABLESAMPLE,
// no ANALYZE): the source may be frozen. The sample is REPEATABLE, so an
// unchanged source gives the same ranges and an interrupted verification
// resumes on the ranges it already compared.
func chunkBounds(ctx context.Context, c *pgx.Conn, table string, k keyInfo, est, bytes int64, n int) ([]string, error) {
	if n < 2 {
		return nil, nil
	}
	// SYSTEM sampling reads whole pages: sample enough rows, and enough
	// pages (about 20 per range), for ranges of similar size.
	pct := 100.0
	if est > 0 {
		byRows := float64(n*200) / float64(est)
		byPages := 0.0
		if bytes > 0 {
			byPages = float64(n*20) / (float64(bytes) / 8192)
		}
		pct = max(byRows, byPages) * 100
		pct = min(max(pct, 0.0001), 100)
	}
	// The first key of each tile of the sample.
	q := fmt.Sprintf(`SELECT min(%[1]s)::text FROM (
		SELECT %[1]s, ntile(%[2]d) OVER (ORDER BY %[1]s) AS t FROM (SELECT %[1]s FROM %[3]s TABLESAMPLE SYSTEM (%[4]f) REPEATABLE (42)) s
	) x GROUP BY t ORDER BY min(%[1]s)`, k.col, n, table, pct)
	vals, err := pg.QueryStrings(ctx, c, q)
	if err != nil {
		return nil, err
	}
	// The first tile's minimum is not a bound; drop duplicates.
	var out []string
	for i, v := range vals {
		if i == 0 || (len(out) > 0 && out[len(out)-1] == v) {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

type chunk struct {
	table, lo, hi string // lo, hi: "" for open ends
	where         string
	args          []any
}

func chunksFor(table string, k keyInfo, bounds []string) []chunk {
	if !k.ok || len(bounds) == 0 {
		return []chunk{{table: table}}
	}
	var out []chunk
	for i := 0; i <= len(bounds); i++ {
		c := chunk{table: table}
		switch {
		case i == 0:
			c.hi = bounds[0]
			c.where = fmt.Sprintf("WHERE %s < $1::%s", k.col, k.typ)
			c.args = []any{c.hi}
		case i == len(bounds):
			c.lo = bounds[i-1]
			c.where = fmt.Sprintf("WHERE %s >= $1::%s", k.col, k.typ)
			c.args = []any{c.lo}
		default:
			c.lo, c.hi = bounds[i-1], bounds[i]
			c.where = fmt.Sprintf("WHERE %s >= $1::%s AND %s < $2::%s", k.col, k.typ, k.col, k.typ)
			c.args = []any{c.lo, c.hi}
		}
		out = append(out, c)
	}
	return out
}

func chunkHash(ctx context.Context, c *pgx.Conn, ch chunk) (int64, string, error) {
	var n int64
	var sum *string
	q := fmt.Sprintf(`SELECT count(*), COALESCE(sum(('x'||substr(md5(ROW(t.*)::text),1,15))::bit(60)::bigint),0)::text FROM %s t %s`, ch.table, ch.where)
	if err := pg.QueryRow(ctx, c, q, ch.args...).Scan(&n, &sum); err != nil {
		return 0, "", err
	}
	return n, *sum, nil
}

// LargeTable is one table for the full comparison.
type LargeTable struct {
	Name  string
	Bytes int64
	Est   int64
}

// FullCompareResult is the outcome for one table.
type FullCompareResult struct {
	Table      string
	Chunks     int
	Rows       int64
	Mismatches []string // "table [lo, hi): source n, target m" or "... contents differ"
	Err        error
	Key        string
}

// FullCompare compares every row of the given tables, split into ranges of
// about chunkBytes, with workers parallel pairs of sessions.
func FullCompare(ctx context.Context, src, dst pg.Conn, tables []LargeTable, chunkBytes int64, workers int, tr ChunkTracker, progress ChunkProgress) []FullCompareResult {
	if workers < 1 {
		workers = 1
	}
	if chunkBytes <= 0 {
		chunkBytes = 1 << 30
	}
	results := make([]FullCompareResult, len(tables))
	// Plan every table's ranges first (on the source), so progress has a total.
	plan := map[int][]chunk{}
	total := 0
	if c, err := pg.Connect(ctx, src, time.Minute); err != nil {
		for i, t := range tables {
			results[i] = FullCompareResult{Table: t.Name, Err: err}
		}
		return results
	} else {
		if err := prepareVerifySession(ctx, c); err != nil {
			c.Close(ctx)
			for i, t := range tables {
				results[i] = FullCompareResult{Table: t.Name, Err: err}
			}
			return results
		}
		for i, t := range tables {
			results[i].Table = t.Name
			k, err := tableKey(ctx, c, t.Name)
			if err != nil {
				results[i].Err = err
				continue
			}
			results[i].Key = k.desc
			n := int(t.Bytes/chunkBytes) + 1
			if n > 10000 {
				n = 10000
			}
			var bounds []string
			if k.ok && n > 1 {
				if bounds, err = chunkBounds(ctx, c, t.Name, k, t.Est, t.Bytes, n); err != nil {
					results[i].Err = err
					continue
				}
			}
			plan[i] = chunksFor(t.Name, k, bounds)
			results[i].Chunks = len(plan[i])
			total += len(plan[i])
		}
		c.Close(ctx)
	}
	type job struct {
		i  int
		ch chunk
	}
	jobs := make(chan job)
	var mu sync.Mutex
	done := 0
	report := func(table string) {
		done++
		if progress != nil {
			progress(done, total, table)
		}
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var sc, dc *pgx.Conn
			defer func() {
				if sc != nil {
					sc.Close(context.Background())
				}
				if dc != nil {
					dc.Close(context.Background())
				}
			}()
			open := func(c pg.Conn) (*pgx.Conn, error) {
				x, err := pg.Connect(ctx, c, 0)
				if err != nil {
					return nil, err
				}
				if err := prepareVerifySession(ctx, x); err != nil {
					x.Close(ctx)
					return nil, err
				}
				return x, nil
			}
			for j := range jobs {
				fail := func(err error) {
					mu.Lock()
					if results[j.i].Err == nil {
						results[j.i].Err = err
					}
					report(j.ch.table)
					mu.Unlock()
				}
				if ctx.Err() != nil {
					fail(ctx.Err())
					continue
				}
				if tr != nil && tr.Done(j.ch.table, j.ch.lo, j.ch.hi) {
					mu.Lock()
					report(j.ch.table)
					mu.Unlock()
					continue
				}
				var err error
				if sc == nil {
					if sc, err = open(src); err != nil {
						fail(err)
						continue
					}
				}
				if dc == nil {
					if dc, err = open(dst); err != nil {
						fail(err)
						continue
					}
				}
				var a, b struct {
					n   int64
					sum string
					err error
				}
				var pw sync.WaitGroup
				pw.Add(2)
				go func() { defer pw.Done(); a.n, a.sum, a.err = chunkHash(ctx, sc, j.ch) }()
				go func() { defer pw.Done(); b.n, b.sum, b.err = chunkHash(ctx, dc, j.ch) }()
				pw.Wait()
				if a.err != nil || b.err != nil {
					// A broken session is reopened for the next range.
					if a.err != nil {
						sc.Close(ctx)
						sc = nil
					}
					if b.err != nil {
						dc.Close(ctx)
						dc = nil
					}
					fail(fmt.Errorf("%s %s: source: %v; target: %v", j.ch.table, rangeText(j.ch), a.err, b.err))
					continue
				}
				mu.Lock()
				results[j.i].Rows += a.n
				switch {
				case a.n != b.n:
					results[j.i].Mismatches = append(results[j.i].Mismatches, fmt.Sprintf("%s %s: source %d rows, target %d", j.ch.table, rangeText(j.ch), a.n, b.n))
				case a.sum != b.sum:
					results[j.i].Mismatches = append(results[j.i].Mismatches, fmt.Sprintf("%s %s: same %d rows, different contents", j.ch.table, rangeText(j.ch), a.n))
				default:
					if tr != nil {
						tr.Record(j.ch.table, j.ch.lo, j.ch.hi, a.n)
					}
				}
				report(j.ch.table)
				mu.Unlock()
			}
		}()
	}
	for i := range tables {
		for _, ch := range plan[i] {
			jobs <- job{i, ch}
		}
	}
	close(jobs)
	wg.Wait()
	for i := range results {
		sort.Strings(results[i].Mismatches)
	}
	return results
}

func rangeText(c chunk) string {
	switch {
	case c.lo == "" && c.hi == "":
		return "(whole table)"
	case c.lo == "":
		return "[start, " + c.hi + ")"
	case c.hi == "":
		return "[" + c.lo + ", end]"
	}
	return "[" + c.lo + ", " + c.hi + ")"
}
