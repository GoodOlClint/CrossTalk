package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const DefaultRetention = 24 * time.Hour

type Type string

const (
	TypeDedupe     Type = "dedupe"
	TypeTask       Type = "task"
	TypeDispatch   Type = "dispatch"
	TypeOutcome    Type = "outcome"
	TypeCancel     Type = "cancel"
	TypeRoute      Type = "route"
	TypeObligation Type = "obligation"
	TypeClaim      Type = "claim"
	TypeOutbound   Type = "outbound"
)

const (
	StateSubmitted   = "submitted"
	StateWorking     = "working"
	StateDispatching = "dispatching"
	StateUnknown     = "unknown"
	StateCanceled    = "canceled"
	StateOpen        = "open"
	StateClosed      = "closed"
	StatePending     = "pending"
	StateDelivered   = "delivered"
)

type Binding struct {
	InstanceID      string `json:"instance_id"`
	HarnessEndpoint string `json:"harness_endpoint"`
	SessionRef      string `json:"session_ref"`
}

// SameConversation reports whether b and o name the same conversation on the
// same server; instance_id changes per registration and is not compared.
func (b Binding) SameConversation(o Binding) bool {
	return b.HarnessEndpoint == o.HarnessEndpoint && b.SessionRef == o.SessionRef
}

// Record is one journal line. Which fields are set depends on Type:
//
//	dedupe:     Sender, MessageID, State
//	task:       TaskID, Principal, Kind, State
//	dispatch:   MessageID (State = dispatching)
//	outcome:    MessageID, State
//	cancel:     MessageID
//	route:      Session, MessageID, Sender (original sender), State open|closed
//	obligation: MessageID (the reply), InReplyTo, To, State pending|delivered
//	claim:      Owner (agent_id), Claimed
//	outbound:   MessageID, To (agent_id, harness:session_ref, or an unresolved name), Kind, State
type Record struct {
	Type      Type      `json:"type"`
	At        time.Time `json:"at"`
	Binding   Binding   `json:"binding"`
	MessageID string    `json:"message_id,omitempty"`
	Sender    string    `json:"sender,omitempty"`
	State     string    `json:"state,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	Principal string    `json:"principal,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Session   string    `json:"session,omitempty"`
	InReplyTo string    `json:"in_reply_to,omitempty"`
	To        string    `json:"to,omitempty"`
	Owner     string    `json:"owner,omitempty"`
	Claimed   bool      `json:"claimed,omitempty"`
	// Message is the normalized message a dedupe record was accepted with,
	// kept so the mailbox can return it.
	Message json.RawMessage `json:"message,omitempty"`
}

func (r Record) key() string {
	switch r.Type {
	case TypeDedupe:
		return "d\x00" + r.Sender + "\x00" + r.MessageID
	case TypeTask:
		return "t\x00" + r.TaskID
	case TypeDispatch, TypeOutcome, TypeCancel:
		return "m\x00" + r.MessageID
	case TypeRoute:
		return "r\x00" + r.Session + "\x00" + r.MessageID + "\x00" + r.Sender
	case TypeObligation:
		return "o\x00" + r.MessageID
	case TypeClaim:
		return "c"
	case TypeOutbound:
		return "s\x00" + r.MessageID
	}
	return "?\x00" + string(r.Type)
}

type dispatchState struct {
	claimed  bool
	canceled bool
	outcome  string
}

// unresolved reports whether the latest record for a key keeps that key past retention.
func unresolved(latest Record) bool {
	switch latest.Type {
	case TypeDedupe, TypeTask:
		switch latest.State {
		case StateSubmitted, StateWorking, StateDispatching, StateUnknown:
			return true
		}
	case TypeDispatch:
		return true
	case TypeOutcome:
		return latest.State == StateUnknown
	case TypeRoute:
		return latest.State == StateOpen
	case TypeObligation:
		return latest.State != StateDelivered
	case TypeClaim:
		return latest.Claimed
	}
	return false
}

type Options struct {
	Now       func() time.Time
	Retention time.Duration
}

type Replay struct {
	// Dispatching holds claims with no recorded outcome. They are never
	// re-injected; the caller reconciles them against the harness.
	Dispatching []Record
	// Unresolved holds the latest record of every other unresolved key.
	Unresolved       []Record
	DiscardedPartial bool
	Truncated        int
}

type Journal struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	binding  Binding
	now      func() time.Time
	latest   map[string]Record
	dispatch map[string]*dispatchState
}

var ErrClosed = errors.New("journal: closed")

func Path(stateDir, agentID string) string {
	return filepath.Join(stateDir, "journal", agentID+".jsonl")
}

// Open replays the agent's journal, drops a partial trailing line, truncates
// resolved records older than the retention window, and returns the journal
// ready for appends under binding. The caller must hold the agent lock.
func Open(stateDir, agentID string, binding Binding, opt Options) (*Journal, Replay, error) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Retention == 0 {
		opt.Retention = DefaultRetention
	}
	path := Path(stateDir, agentID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, Replay{}, err
	}
	records, partial, err := readAll(path)
	if err != nil {
		return nil, Replay{}, err
	}

	latest := map[string]Record{}
	for _, r := range records {
		latest[r.key()] = r
	}
	cutoff := opt.Now().Add(-opt.Retention)
	var kept []Record
	for _, r := range records {
		if !r.At.Before(cutoff) || unresolved(latest[r.key()]) {
			kept = append(kept, r)
		}
	}
	rp := Replay{DiscardedPartial: partial, Truncated: len(records) - len(kept)}
	if partial || rp.Truncated > 0 {
		if err := rewrite(path, kept); err != nil {
			return nil, Replay{}, err
		}
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, Replay{}, err
	}
	j := &Journal{f: f, path: path, binding: binding, now: opt.Now, latest: map[string]Record{}, dispatch: map[string]*dispatchState{}}
	for _, r := range kept {
		j.index(r)
	}
	for _, r := range j.latest {
		if !unresolved(r) {
			continue
		}
		if r.Type == TypeDispatch {
			rp.Dispatching = append(rp.Dispatching, r)
		} else {
			rp.Unresolved = append(rp.Unresolved, r)
		}
	}
	return j, rp, nil
}

func readAll(path string) ([]Record, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	partial := false
	if n := len(b); n > 0 && b[n-1] != '\n' {
		partial = true
		b = b[:bytes.LastIndexByte(b, '\n')+1]
	}
	var out []Record
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, false, fmt.Errorf("journal: %s line %d: %w", path, line, err)
		}
		out = append(out, r)
	}
	return out, partial, sc.Err()
}

func rewrite(path string, records []Record) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriter(tmp)
	for _, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			tmp.Close()
			return err
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := errors.Join(w.Flush(), tmp.Chmod(0o600), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (j *Journal) index(r Record) {
	j.latest[r.key()] = r
	if r.MessageID == "" {
		return
	}
	switch r.Type {
	case TypeDispatch, TypeOutcome, TypeCancel:
	default:
		return
	}
	ds := j.dispatch[r.MessageID]
	if ds == nil {
		ds = &dispatchState{}
		j.dispatch[r.MessageID] = ds
	}
	switch r.Type {
	case TypeDispatch:
		ds.claimed = true
	case TypeOutcome:
		ds.outcome = r.State
	case TypeCancel:
		ds.canceled = true
	}
}

// appendLocked writes r, fsyncs, then indexes it; nothing is indexed if the write fails.
func (j *Journal) appendLocked(r Record) (Record, error) {
	if j.f == nil {
		return Record{}, ErrClosed
	}
	if r.At.IsZero() {
		r.At = j.now().UTC()
	}
	if r.Binding == (Binding{}) {
		r.Binding = j.binding
	}
	line, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	line = append(line, '\n')
	if _, err := j.f.Write(line); err != nil {
		return Record{}, err
	}
	if err := j.f.Sync(); err != nil {
		return Record{}, err
	}
	j.index(r)
	return r, nil
}

// Append durably records r. At and Binding default to now and the journal's binding.
func (j *Journal) Append(r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, err := j.appendLocked(r)
	return err
}

// Accept records (sender, messageID) with state unless it is already recorded,
// in which case it returns the prior state and dup = true and writes nothing.
func (j *Journal) Accept(sender, messageID, state string) (prior string, dup bool, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if r, ok := j.latest[Record{Type: TypeDedupe, Sender: sender, MessageID: messageID}.key()]; ok {
		return r.State, true, nil
	}
	_, err = j.appendLocked(Record{Type: TypeDedupe, Sender: sender, MessageID: messageID, State: state})
	return state, false, err
}

// SetDedupeState records a later state for an accepted (sender, messageID).
func (j *Journal) SetDedupeState(sender, messageID, state string) error {
	return j.Append(Record{Type: TypeDedupe, Sender: sender, MessageID: messageID, State: state})
}

func (j *Journal) Dedupe(sender, messageID string) (string, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.latest[Record{Type: TypeDedupe, Sender: sender, MessageID: messageID}.key()]
	return r.State, ok
}

// Claim durably records a dispatch claim for messageID. It returns false, and
// writes nothing, if the message is already claimed or canceled. The claim is
// on disk before Claim returns, so a harness write may follow it.
func (j *Journal) Claim(messageID string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ds := j.dispatch[messageID]; ds != nil && (ds.claimed || ds.canceled) {
		return false, nil
	}
	if _, err := j.appendLocked(Record{Type: TypeDispatch, MessageID: messageID, State: StateDispatching}); err != nil {
		return false, err
	}
	return true, nil
}

// Cancel durably records a cancel for messageID. It returns false, and writes
// nothing, if the message is already claimed or canceled.
func (j *Journal) Cancel(messageID string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ds := j.dispatch[messageID]; ds != nil && (ds.claimed || ds.canceled) {
		return false, nil
	}
	if _, err := j.appendLocked(Record{Type: TypeCancel, MessageID: messageID, State: StateCanceled}); err != nil {
		return false, err
	}
	return true, nil
}

// Resolve records the dispatch outcome for a claimed messageID.
func (j *Journal) Resolve(messageID, state string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	ds := j.dispatch[messageID]
	if ds == nil || !ds.claimed {
		return fmt.Errorf("journal: resolve %s: not claimed", messageID)
	}
	_, err := j.appendLocked(Record{Type: TypeOutcome, MessageID: messageID, State: state})
	return err
}

// DispatchState returns the latest dispatch-related record for messageID.
func (j *Journal) DispatchState(messageID string) (Record, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.latest[Record{Type: TypeDispatch, MessageID: messageID}.key()]
	return r, ok
}

// Latest returns the latest record sharing r's key.
func (j *Journal) Latest(r Record) (Record, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	l, ok := j.latest[r.key()]
	return l, ok
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}
