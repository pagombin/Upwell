package orch

import (
	"bufio"
	"io"
	"os"
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
