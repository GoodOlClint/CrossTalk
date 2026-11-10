package codexqueue

import (
	"fmt"
	"sync"
	"time"
)

// Test doubles. The Codex simulator encodes only what step 0 measured
// (docs/plans/codex-no-daemon-queue-step0.md); it is not new evidence.

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type row struct {
	thread, wakeID string
	order          int
	foreign        bool
}

// fakeQueue is the queued_items table plus a watcher that dispatches rows.
type fakeQueue struct {
	mu       sync.Mutex
	rows     []row
	max      int
	busyN    int
	inserts  int
	onInsert func(thread string)
}

func newQueue() *fakeQueue { return &fakeQueue{max: 100} }

func (q *fakeQueue) InsertWake(thread, wakeID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.busyN > 0 {
		q.busyN--
		return ErrBusy
	}
	n, hasOurs := 0, false
	for _, r := range q.rows {
		if r.thread == thread {
			n++
			if !r.foreign {
				hasOurs = true
			}
		}
	}
	if n >= q.max {
		return ErrCap
	}
	if hasOurs {
		return ErrExistingRow
	}
	if q.onInsert != nil {
		q.onInsert(thread)
	}
	q.rows = append(q.rows, row{thread: thread, wakeID: wakeID, order: n})
	q.inserts++
	return nil
}

func (q *fakeQueue) HasRow(wakeID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.rows {
		if r.wakeID == wakeID {
			return true
		}
	}
	return false
}

func (q *fakeQueue) count(thread string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, r := range q.rows {
		if r.thread == thread {
			n++
		}
	}
	return n
}

func (q *fakeQueue) fill(thread string, n int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := 0; i < n; i++ {
		q.rows = append(q.rows, row{thread: thread, wakeID: fmt.Sprintf("foreign-%d", i), foreign: true, order: i})
	}
}

// take removes the head row for thread (Codex accepted the turn) and returns its wake id.
func (q *fakeQueue) take(thread string) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, r := range q.rows {
		if r.thread == thread {
			q.rows = append(q.rows[:i], q.rows[i+1:]...)
			return r.wakeID, true
		}
	}
	return "", false
}

type turn struct {
	inputs  []string
	trigger string
}

// simThread models one Codex thread at 0.167.0: a queue watcher, and the TUI's own
// follow-up queue, which is not in the table (step 0 probe 9).
type simThread struct {
	q        *fakeQueue
	thread   string
	tuiQueue []string
	turns    []turn
	alive    bool
}

func newSim(q *fakeQueue, thread string) *simThread {
	return &simThread{q: q, thread: thread, alive: true}
}

func (s *simThread) typeTUIQueued(text string) { s.tuiQueue = append(s.tuiQueue, text) }

// dispatch is the watcher firing at an idle moment: the head row starts a queue turn and
// any TUI-queued prompt is folded into that turn as a steer.
func (s *simThread) dispatch() bool {
	if !s.alive {
		return false
	}
	id, ok := s.q.take(s.thread)
	if !ok {
		return false
	}
	t := turn{trigger: "queue", inputs: []string{id}}
	t.inputs = append(t.inputs, s.tuiQueue...)
	s.tuiQueue = nil
	s.turns = append(s.turns, t)
	return true
}

// killAtConsumption is kill -9 the moment the row is gone: the row was taken, no turn
// is recorded, and nothing is redelivered on resume (step 0 probe 6).
func (s *simThread) killAtConsumption() bool {
	if _, ok := s.q.take(s.thread); !ok {
		return false
	}
	s.alive = false
	return true
}

func (s *simThread) resume() { s.alive = true }
