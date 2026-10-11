package orch

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pagombin/upwell/internal/engine"
	"github.com/pagombin/upwell/internal/logx"
)

// classes accumulates what the log of the current attempt says.
type classes struct {
	errors         int
	warnings       int
	lastError      string
	slotLost       bool
	baseCopyFailed bool
	permanent      bool
	endposReached  bool
	// engineLock: pgcopydb's own SQLite catalog was locked by another of its
	// processes (F12), an engine-internal transient failure.
	engineLock bool
	// fatal or applyGone during streaming: pgcopydb gave up (or its apply
	// process exited) while other processes, such as receive, which ignores
	// SIGTERM (F3), keep the unit looking alive. Nothing is applied any more.
	fatal     bool
	applyGone bool
	// firstError is the attempt's first error line: when pgcopydb fails, its
	// own error handling often logs more errors after the real one (on the
	// droplet: "Failed to close snapshot" after a lost target connection).
	firstError string
	// connLost names a connection that closed under the engine ("source" or
	// "target"), with the error; failedTable is the table being copied.
	connLost, connLostMsg string
	failedTable           string
}

var (
	connLostRe    = regexp.MustCompile(`^\[(SOURCE|TARGET)[^\]]*\] (.*(SSL error|SSL SYSCALL|server closed the connection|could not receive data from server|could not send data to server|unexpected eof|terminating connection|connection to server .* failed|no connection to the server).*)$`)
	failedTableRe = regexp.MustCompile(`Failed to copy data for table with oid (\d+)`)
)

// note records one error line.
func (c *classes) note(msg string) {
	if c.firstError == "" {
		c.firstError = msg
	}
	if m := connLostRe.FindStringSubmatch(msg); m != nil && c.connLost == "" {
		c.connLost, c.connLostMsg = strings.ToLower(m[1]), m[2]
	}
	if m := failedTableRe.FindStringSubmatch(msg); m != nil && c.failedTable == "" {
		c.failedTable = "table with oid " + m[1]
	}
}

// rootCause is the reason to show for a failed attempt: a lost connection
// first, else the first error (with the last one when they differ).
func (c classes) rootCause() string {
	switch {
	case c.connLost != "":
		during := ""
		if c.failedTable != "" {
			during = " while copying " + c.failedTable
		}
		return fmt.Sprintf("the connection to the %s was closed under the engine%s (%s). Something between this droplet and the %s (a proxy or load balancer, a failover or a restart) ended it; pgcopydb does not retry a table, so the copy must restart", c.connLost, during, c.connLostMsg, c.connLost)
	case c.firstError != "" && c.firstError != c.lastError:
		return c.firstError + " (later: " + c.lastError + ")"
	}
	return c.lastError
}

type tailer struct {
	migration, db string
	file          string
	attempt       int
	offset        int64
	inode         uint64
	cls           classes
	// since: lines the engine wrote before the current attempt started (the
	// tail of a stopped run, read late) are indexed but never classified
	// against the new attempt.
	since time.Time
}

type tailers struct {
	o  *Orchestrator
	mu sync.Mutex
	m  map[string]*tailer
}

func newTailers(o *Orchestrator) *tailers { return &tailers{o: o, m: map[string]*tailer{}} }

func key(m, d string) string { return m + "\x00" + d }

// attach starts (or keeps) following file for a database. A new attempt
// number resets the classification.
func (t *tailers) attach(migration, db, file string, attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := key(migration, db)
	if tl, ok := t.m[k]; ok && tl.file == file {
		if tl.attempt != attempt {
			tl.attempt = attempt
			tl.cls = classes{}
			tl.since = time.Now().Add(-2 * time.Second)
		}
		return
	}
	tl := &tailer{migration: migration, db: db, file: file, attempt: attempt, since: time.Now().Add(-2 * time.Second)}
	var inode int64
	t.o.st.DB.QueryRow(`SELECT inode, offset FROM log_offsets WHERE file=?`, file).Scan(&inode, &tl.offset)
	tl.inode = uint64(inode)
	t.m[k] = tl
}

func (t *tailers) reset(migration, db string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tl, ok := t.m[key(migration, db)]; ok {
		tl.cls = classes{}
		tl.since = time.Now().Add(-2 * time.Second)
	}
}

// poll reads new lines, indexes them and returns the attempt's classes.
func (t *tailers) poll(migration, db string) classes {
	t.mu.Lock()
	tl, ok := t.m[key(migration, db)]
	t.mu.Unlock()
	if !ok {
		return classes{}
	}
	t.read(tl)
	t.mu.Lock()
	defer t.mu.Unlock()
	return tl.cls
}

func (t *tailers) read(tl *tailer) {
	f, err := os.Open(tl.file)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}
	if (tl.inode != 0 && ino != tl.inode) || fi.Size() < tl.offset {
		tl.offset = 0 // rotated or truncated
	}
	tl.inode = ino
	if fi.Size() == tl.offset {
		return
	}
	if _, err := f.Seek(tl.offset, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 64*1024)
	eng := t.o.eng
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			break // partial last line: read it next time
		}
		tl.offset += int64(len(line))
		raw := line[:len(line)-1]
		if raw == "" {
			continue
		}
		rec := eng.ParseLog(raw)
		cls := eng.Classify(rec)
		t.mu.Lock()
		// Only minutes-old lines are a stopped run's tail; anything hours off is a
		// clock or time-zone difference and must still be classified.
		stale := !rec.TS.IsZero() && rec.TS.Before(tl.since) && tl.since.Sub(rec.TS) < 30*time.Minute
		switch {
		case stale:
		case rec.Level == "error" || rec.Level == "fatal":
			tl.cls.errors++
			tl.cls.lastError = rec.Msg
			tl.cls.note(rec.Msg)
			if strings.Contains(rec.Msg, "database is locked") {
				tl.cls.engineLock = true
			}
			if rec.Level == "fatal" {
				tl.cls.fatal = true
			}
		case rec.Level == "warn":
			tl.cls.warnings++
		}
		if stale {
			cls = engine.FailNone
		}
		if !stale && strings.Contains(rec.Msg, "Apply process has terminated") {
			tl.cls.applyGone = true
		}
		switch cls {
		case engine.FailSlotLost:
			tl.cls.slotLost = true
			tl.cls.lastError = rec.Msg
		case engine.FailBaseCopy:
			tl.cls.baseCopyFailed = true
		case engine.FailPermanent:
			tl.cls.permanent = true
		case engine.FailEndposReached:
			tl.cls.endposReached = true
		}
		attempt := tl.attempt
		t.mu.Unlock()
		ts := int64(0)
		if !rec.TS.IsZero() {
			ts = rec.TS.UnixMilli()
		}
		t.o.log.Write(logx.Record{TS: ts, Level: rec.Level, Component: "engine", Migration: tl.migration, Database: tl.db, Attempt: attempt, Step: rec.Step, Table: rec.Table, Msg: rec.Msg, Raw: raw})
	}
	t.o.st.DB.Exec(`INSERT INTO log_offsets(file,inode,offset) VALUES (?,?,?) ON CONFLICT(file) DO UPDATE SET inode=excluded.inode, offset=excluded.offset`, tl.file, int64(tl.inode), tl.offset)
}
