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

// evidence is what a call offers as proof of receipt; every call, evidence or not, goes
// through Mailbox.settle so a non-evidence call has the same path a real one would.
type evidence struct {
	cursor string
	reply  string
	token  string
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
	// AfterMark runs after a pull has marked entries collecting and released the lock,
	// before the response is returned: the window where the response is in flight.
	AfterMark func()
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

// settle applies the evidence rules; the caller holds the lock. Only a reply naming an
// entry, a cursor an earlier pull issued, or an echoed token settles anything.
func (m *Mailbox) settle(thread string, ev evidence) {
	if ev.cursor != "" && m.cursors[thread][ev.cursor] {
		if c := m.find(thread, ev.cursor); c != nil {
			for _, e := range m.entries[thread] {
				if e.seq <= c.seq && e.state == Collecting {
					e.state = Accepted
				}
			}
		}
	}
	if ev.reply != "" {
		if e := m.find(thread, ev.reply); e != nil && e.state == Collecting {
			e.state = Accepted
		}
	}
	if ev.token != "" {
		for _, e := range m.entries[thread] {
			if e.state == Collecting && e.token == ev.token {
				e.state = Accepted
			}
		}
	}
}

// Pull is receive_message. after must be a cursor an earlier pull for this thread issued;
// it settles collecting entries at or before it. A pull without a cursor settles nothing.
func (m *Mailbox) Pull(thread, after string, limit int) PullResult {
	m.mu.Lock()
	m.settle(thread, evidence{cursor: after})
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
	hook := m.AfterMark
	m.mu.Unlock()
	if hook != nil {
		hook()
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
	m.settle(thread, evidence{reply: id})
	return nil
}

func (m *Mailbox) Ack(thread, token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settle(thread, evidence{token: token})
}

// List stands in for any call that is not evidence (list_agents, send_message): it takes
// the same settle path with no evidence.
func (m *Mailbox) List(thread string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settle(thread, evidence{})
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
