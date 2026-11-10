package codexqueue

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

type EntryState int

const (
	Mailboxed EntryState = iota
	Collecting
	Accepted
	Unknown
)

var (
	ErrNotCollected = errors.New("not_collected")
	ErrNotFound     = errors.New("no such entry for this thread")
)

type entry struct {
	id          string
	seq         int
	state       EntryState
	token       string
	collectedAt time.Time
}

type PullResult struct {
	Entries []string
	Next    string
	Token   string
	More    bool
}

// Mailbox is the per-thread journal of entries (ADR 0012 / plan 3.2). Every method takes the
// calling thread: a call from another thread never touches this thread's entries.
type Mailbox struct {
	mu      sync.Mutex
	now     func() time.Time
	bound   time.Duration
	seq     int
	tokens  int
	entries map[string][]*entry
	cursors map[string]map[string]bool
}

func NewMailbox(now func() time.Time, bound time.Duration) *Mailbox {
	return &Mailbox{now: now, bound: bound, entries: map[string][]*entry{}, cursors: map[string]map[string]bool{}}
}

func (m *Mailbox) find(thread, id string) *entry {
	for _, e := range m.entries[thread] {
		if e.id == id {
			return e
		}
	}
	return nil
}

func (m *Mailbox) Commit(thread, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(thread, id) != nil {
		return
	}
	m.seq++
	m.entries[thread] = append(m.entries[thread], &entry{id: id, seq: m.seq})
}

func allowed(from, to EntryState) bool {
	switch from {
	case Mailboxed:
		return to == Collecting
	case Collecting:
		return to == Accepted || to == Unknown
	}
	return false
}

// Resolve applies an outcome only if it moves the entry forward; a late or replayed
// outcome that would go backwards or skip a state is ignored.
func (m *Mailbox) Resolve(thread, id string, to EntryState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.find(thread, id); e != nil && allowed(e.state, to) {
		e.state = to
	}
}

func (m *Mailbox) State(thread, id string) EntryState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.find(thread, id); e != nil {
		return e.state
	}
	return -1
}

// Pull is receive_message. after must be a cursor an earlier pull for this thread issued;
// it settles collecting entries at or before it. A pull without a cursor settles nothing.
func (m *Mailbox) Pull(thread, after string, limit int) PullResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	if after != "" && m.cursors[thread][after] {
		if c := m.find(thread, after); c != nil {
			for _, e := range m.entries[thread] {
				if e.seq <= c.seq && e.state == Collecting {
					e.state = Accepted
				}
			}
		}
	}
	var res PullResult
	res.Next = after
	m.tokens++
	token := fmt.Sprintf("%s#%d", thread, m.tokens)
	for _, e := range m.entries[thread] {
		if e.state != Mailboxed {
			continue
		}
		if len(res.Entries) == limit {
			res.More = true
			break
		}
		e.state, e.token, e.collectedAt = Collecting, token, m.now()
		res.Entries = append(res.Entries, e.id)
		res.Next = e.id
	}
	if len(res.Entries) > 0 {
		res.Token = token
		if m.cursors[thread] == nil {
			m.cursors[thread] = map[string]bool{}
		}
		for _, id := range res.Entries {
			m.cursors[thread][id] = true
		}
	}
	return res
}

func (m *Mailbox) Reply(thread, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.find(thread, id)
	if e == nil {
		return ErrNotFound
	}
	if e.state == Mailboxed {
		return ErrNotCollected
	}
	if e.state == Collecting {
		e.state = Accepted
	}
	return nil
}

func (m *Mailbox) Ack(thread, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries[thread] {
		if e.state == Collecting && token != "" && e.token == token {
			e.state = Accepted
		}
	}
}

// Other is any call that is not evidence (list_agents, send_message, a second pull
// without a cursor): it exists so tests can issue one concurrently.
func (m *Mailbox) Other(thread string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.entries[thread]
}

func (m *Mailbox) Expire() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, es := range m.entries {
		for _, e := range es {
			if e.state == Collecting && now.Sub(e.collectedAt) >= m.bound {
				e.state = Unknown
			}
		}
	}
}

func (m *Mailbox) ReplayAfterCrash() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, es := range m.entries {
		for _, e := range es {
			if e.state == Collecting {
				e.state = Unknown
			}
		}
	}
}
