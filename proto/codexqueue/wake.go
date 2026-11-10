package codexqueue

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

type GenState string

const (
	GenPending   GenState = "pending"
	GenQueued    GenState = "queued"
	GenAdmitted  GenState = "admitted"
	GenCollected GenState = "collected"
	GenUnknown   GenState = "unknown"
	GenFailed    GenState = "failed"
)

var (
	ErrBusy        = errors.New("queue busy")
	ErrCap         = errors.New("queue at cap")
	ErrExistingRow = errors.New("a CrossTalk row for this thread is already in the table")
	ErrCrash       = errors.New("injected crash")
)

// Queue is the one guarded insert: a single immediate transaction that revalidates the
// gate, refuses at the cap or when a CrossTalk row for the thread exists, and inserts.
type Queue interface {
	InsertWake(thread, wakeID string) error
	HasRow(wakeID string) bool
}

type Gen struct {
	N          int
	WakeID     string
	State      GenState
	AdmittedAt time.Time
	Reason     string
}

func (g *Gen) open() bool {
	return g.State == GenPending || g.State == GenQueued || g.State == GenAdmitted
}

// Wakes holds the persisted generation records; a test "restart" is Replay on the same value.
type Wakes struct {
	mu      sync.Mutex
	q       Queue
	now     func() time.Time
	bound   time.Duration
	Retries int
	Crash   func(point string) bool
	gens    map[string][]*Gen
}

func NewWakes(q Queue, now func() time.Time, bound time.Duration) *Wakes {
	return &Wakes{q: q, now: now, bound: bound, Retries: 3, gens: map[string][]*Gen{}}
}

func (w *Wakes) Gens(thread string) []Gen {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Gen
	for _, g := range w.gens[thread] {
		out = append(out, *g)
	}
	return out
}

func (w *Wakes) crash(p string) bool { return w.Crash != nil && w.Crash(p) }

// Signal is MailSignal: write a row only when no generation is open.
func (w *Wakes) Signal(thread string) (*Gen, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, g := range w.gens[thread] {
		if g.open() {
			return nil, nil
		}
	}
	n := len(w.gens[thread]) + 1
	g := &Gen{N: n, WakeID: fmt.Sprintf("ct-wake:%s:%d", thread, n), State: GenPending}
	w.gens[thread] = append(w.gens[thread], g)
	if w.crash("after-pending") {
		return g, ErrCrash
	}
	var err error
	for i := 0; i <= w.Retries; i++ {
		if err = w.q.InsertWake(thread, g.WakeID); !errors.Is(err, ErrBusy) {
			break
		}
	}
	if err != nil {
		g.State, g.Reason = GenFailed, err.Error()
		return g, nil
	}
	if w.crash("after-commit") {
		return g, ErrCrash
	}
	g.State = GenQueued
	return g, nil
}

// Observe notices that Codex took a queued row.
func (w *Wakes) Observe(thread string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, g := range w.gens[thread] {
		if g.State == GenQueued && !w.q.HasRow(g.WakeID) {
			g.State, g.AdmittedAt = GenAdmitted, w.now()
		}
	}
}

// Pulled records a top-level receive_message from the thread.
func (w *Wakes) Pulled(thread string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, g := range w.gens[thread] {
		if g.State == GenQueued || g.State == GenAdmitted {
			g.State = GenCollected
		}
	}
}

func (w *Wakes) Tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, gs := range w.gens {
		for _, g := range gs {
			if g.State == GenAdmitted && w.now().Sub(g.AdmittedAt) >= w.bound {
				g.State = GenUnknown
			}
		}
	}
}

// Replay applies the ambiguity policy after a restart. It never writes a row.
func (w *Wakes) Replay() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, gs := range w.gens {
		for _, g := range gs {
			switch g.State {
			case GenPending:
				if w.q.HasRow(g.WakeID) {
					g.State = GenQueued
				} else {
					g.State = GenUnknown
				}
			case GenQueued:
				if !w.q.HasRow(g.WakeID) {
					g.State, g.AdmittedAt = GenAdmitted, w.now()
				}
			}
		}
	}
}
