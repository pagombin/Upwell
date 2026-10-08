// Package logx is the logging module: every record from every component is
// redacted, written to a JSON-lines file, indexed in the store and published
// to the live log stream.
package logx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/bus"
	"github.com/pagombin/upwell/internal/store"
)

// Record is one structured log line.
type Record struct {
	Seq       int64  `json:"seq,omitempty"`
	TS        int64  `json:"ts"`
	Level     string `json:"level"`
	Component string `json:"component"`
	Migration string `json:"migration,omitempty"`
	Database  string `json:"database,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	Step      string `json:"step,omitempty"`
	Table     string `json:"table,omitempty"`
	Op        string `json:"op,omitempty"`
	Msg       string `json:"msg"`
	Raw       string `json:"raw,omitempty"`
}

var levelRank = map[string]int{"trace": 0, "debug": 1, "info": 2, "warn": 3, "error": 4, "fatal": 5}

// LevelAtLeast reports whether level is at or above min.
func LevelAtLeast(level, min string) bool { return levelRank[level] >= levelRank[min] }

// Logger fans records out to file, store and bus.
type Logger struct {
	mu       sync.Mutex
	file     io.WriteCloser
	path     string
	store    *store.Store
	bus      *bus.Bus
	minLevel string
	pending  []Record
	flushC   chan struct{}
	done     chan struct{}
	stderr   bool
}

// New creates a logger writing app.log under dir.
func New(dir string, st *store.Store, b *bus.Bus, minLevel string, stderr bool) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "app.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	l := &Logger{file: f, path: p, store: st, bus: b, minLevel: minLevel, flushC: make(chan struct{}, 1), done: make(chan struct{}), stderr: stderr}
	go l.flusher()
	return l, nil
}

// SetLevel changes the minimum level for app records (engine lines are always kept).
func (l *Logger) SetLevel(level string) {
	if _, ok := levelRank[level]; ok {
		l.mu.Lock()
		l.minLevel = level
		l.mu.Unlock()
	}
}

// Write logs one record. Engine records are kept regardless of level.
func (l *Logger) Write(r Record) {
	if r.TS == 0 {
		r.TS = time.Now().UnixMilli()
	}
	if r.Level == "" {
		r.Level = "info"
	}
	l.mu.Lock()
	min := l.minLevel
	l.mu.Unlock()
	if r.Component != "engine" && !LevelAtLeast(r.Level, min) {
		return
	}
	r.Msg = Redact(r.Msg)
	r.Raw = Redact(r.Raw)
	b, _ := json.Marshal(r)
	l.mu.Lock()
	l.file.Write(append(b, '\n'))
	if l.stderr {
		fmt.Fprintf(os.Stderr, "%s %-5s %-12s %s\n", time.UnixMilli(r.TS).UTC().Format("15:04:05.000"), r.Level, r.Component, r.Msg)
	}
	l.pending = append(l.pending, r)
	n := len(l.pending)
	l.mu.Unlock()
	if n >= 200 {
		select {
		case l.flushC <- struct{}{}:
		default:
		}
	}
}

// Logf is a convenience for app records.
func (l *Logger) Logf(level, component, migration, format string, args ...any) {
	l.Write(Record{Level: level, Component: component, Migration: migration, Msg: fmt.Sprintf(format, args...)})
}

func (l *Logger) flusher() {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-l.flushC:
		case <-l.done:
			l.flush()
			return
		}
		l.flush()
	}
}

func (l *Logger) flush() {
	l.mu.Lock()
	batch := l.pending
	l.pending = nil
	l.mu.Unlock()
	if len(batch) == 0 || l.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := l.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs(ts,level,component,migration_id,database,attempt,step,table_name,op,msg,raw) VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return
	}
	for i := range batch {
		r := &batch[i]
		res, err := stmt.ExecContext(ctx, r.TS, r.Level, r.Component, store.NullString(r.Migration), store.NullString(r.Database),
			r.Attempt, store.NullString(r.Step), store.NullString(r.Table), store.NullString(r.Op), r.Msg, store.NullString(r.Raw))
		if err == nil {
			r.Seq, _ = res.LastInsertId()
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		return
	}
	if l.bus != nil {
		for _, r := range batch {
			l.bus.Publish("logs", "log", r.Migration, r)
		}
	}
}

// Close flushes and closes the file.
func (l *Logger) Close() {
	close(l.done)
	time.Sleep(50 * time.Millisecond)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.file.Close()
}

// Slog returns a slog.Logger whose records flow through this logger.
func (l *Logger) Slog(component string) *slog.Logger {
	return slog.New(&handler{l: l, component: component})
}

type handler struct {
	l         *Logger
	component string
	attrs     []slog.Attr
}

func (h *handler) Enabled(context.Context, slog.Level) bool { return true }
func (h *handler) Handle(_ context.Context, r slog.Record) error {
	lvl := "info"
	switch {
	case r.Level >= slog.LevelError:
		lvl = "error"
	case r.Level >= slog.LevelWarn:
		lvl = "warn"
	case r.Level < slog.LevelInfo:
		lvl = "debug"
	}
	rec := Record{TS: r.Time.UnixMilli(), Level: lvl, Component: h.component, Msg: r.Message}
	add := func(a slog.Attr) bool {
		switch a.Key {
		case "migration":
			rec.Migration = a.Value.String()
		case "database":
			rec.Database = a.Value.String()
		case "op":
			rec.Op = a.Value.String()
		default:
			rec.Msg += " " + a.Key + "=" + a.Value.String()
		}
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	h.l.Write(rec)
	return nil
}
func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &handler{l: h.l, component: h.component, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}
func (h *handler) WithGroup(string) slog.Handler { return h }
