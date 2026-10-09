// Package bus is the in-process publish/subscribe used for every live view.
package bus

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one published message.
type Event struct {
	Seq       int64           `json:"seq"`
	Topic     string          `json:"topic"`
	Type      string          `json:"type"`
	Migration string          `json:"migration,omitempty"`
	Data      json.RawMessage `json:"data"`
	TS        int64           `json:"ts"`
}

// Bus fans events out to subscribers and keeps a ring for Last-Event-ID resume.
type Bus struct {
	mu   sync.RWMutex
	subs map[int64]*Sub
	next int64
	seq  atomic.Int64
	ring []Event
	pos  int
	full bool
}

// Sub is a subscription.
type Sub struct {
	C      chan Event
	filter func(Event) bool
	id     int64
	b      *Bus
}

// New returns a bus with a resume ring of size n.
func New(n int) *Bus { return &Bus{subs: map[int64]*Sub{}, ring: make([]Event, n)} }

// Publish sends data (marshalled to JSON) on topic.
func (b *Bus) Publish(topic, typ, migration string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	e := Event{Seq: b.seq.Add(1), Topic: topic, Type: typ, Migration: migration, Data: raw, TS: time.Now().UnixMilli()}
	b.mu.Lock()
	b.ring[b.pos] = e
	b.pos = (b.pos + 1) % len(b.ring)
	if b.pos == 0 {
		b.full = true
	}
	subs := make([]*Sub, 0, len(b.subs))
	for _, s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		if s.filter != nil && !s.filter(e) {
			continue
		}
		select {
		case s.C <- e:
		default: // slow subscriber: drop; the client resyncs from the API
		}
	}
}

// Subscribe registers a filtered subscriber and returns buffered events with
// Seq greater than since.
func (b *Bus) Subscribe(since int64, filter func(Event) bool) (*Sub, []Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	s := &Sub{C: make(chan Event, 4096), filter: filter, id: b.next, b: b}
	b.subs[s.id] = s
	var backlog []Event
	if since > 0 {
		n := b.pos
		if b.full {
			n = len(b.ring)
		}
		start := 0
		if b.full {
			start = b.pos
		}
		for i := 0; i < n; i++ {
			e := b.ring[(start+i)%len(b.ring)]
			if e.Seq > since && (filter == nil || filter(e)) {
				backlog = append(backlog, e)
			}
		}
	}
	return s, backlog
}

// Close removes the subscription.
func (s *Sub) Close() {
	s.b.mu.Lock()
	delete(s.b.subs, s.id)
	s.b.mu.Unlock()
}
