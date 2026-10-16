package orch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pagombin/upwell/internal/pg"
)

// D107: once a minute, each streaming database logs how much of the
// source's WAL its engine received and applied in that minute, the backlog,
// and whether it is gaining or losing ground, so the Logs tab tells the CDC
// story without opening the charts.

type cdcMark struct {
	at            time.Time
	write, replay uint64
	backlog       int64
}

var cdcMarks sync.Map // database id -> cdcMark

func (o *Orchestrator) cdcProgress(ctx context.Context, m Migration, d Database, writeLSN, replayLSN string, backlog int64) {
	w, err1 := pg.ParseLSN(writeLSN)
	r, err2 := pg.ParseLSN(replayLSN)
	if err1 != nil || err2 != nil || w == 0 || r == 0 {
		return
	}
	now := time.Now()
	prev, ok := cdcMarks.Load(d.ID)
	if !ok {
		cdcMarks.Store(d.ID, cdcMark{now, w, r, backlog})
		return
	}
	p := prev.(cdcMark)
	secs := now.Sub(p.at).Seconds()
	if secs < 60 {
		return
	}
	cdcMarks.Store(d.ID, cdcMark{now, w, r, backlog})
	if w < p.write || r < p.replay { // a restart from zero, or a new slot
		return
	}
	recv, appl := float64(w-p.write)/secs, float64(r-p.replay)/secs
	trend := "steady"
	if backlog >= 0 && p.backlog >= 0 {
		switch delta := float64(backlog-p.backlog) / secs; {
		case delta > 64<<10:
			trend = fmt.Sprintf("growing %s/s", humanBytes(delta))
		case delta < -(64 << 10):
			trend = fmt.Sprintf("shrinking %s/s, about %s to drain", humanBytes(-delta), humanDuration(float64(backlog)/(-delta)))
		}
	}
	o.logf("info", "cdc", m.ID, d.SourceName, "", "CDC last %s: received %s/s, applied %s/s of source WAL; backlog %s (%s); %s",
		time.Duration(secs*float64(time.Second)).Round(time.Second), humanBytes(recv), humanBytes(appl), humanBytes(float64(max(backlog, 0))), trend, Label(d.State))
}

func humanBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", b/(1<<10))
	}
	return fmt.Sprintf("%.0f B", b)
}
