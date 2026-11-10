package a2a

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// TaskStore persists task changes as journal.TypeTask records. Every record
// is the full task state after one change; its Message, when set, is the one
// A2A message that change appended to the task's history.
type TaskStore interface {
	// Load returns the records to rebuild tasks from, in append order.
	Load() []journal.Record
	// Save durably records r before the change it describes is visible.
	Save(r journal.Record) error
}

// MemoryTaskStore keeps records in memory; a Server rebuilt on the same
// MemoryTaskStore sees the tasks of the one before it.
type MemoryTaskStore struct {
	mu   sync.Mutex
	recs []journal.Record
}

func NewMemoryTaskStore() *MemoryTaskStore { return &MemoryTaskStore{} }

func (m *MemoryTaskStore) Load() []journal.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.recs)
}

func (m *MemoryTaskStore) Save(r journal.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	m.recs = append(m.recs, r)
	return nil
}

// JournalTaskStore keeps task records in the agent's journal (§5).
type JournalTaskStore struct {
	j    *journal.Journal
	recs []journal.Record
}

// NewJournalTaskStore reads j's task records once. A task whose latest record
// is terminal and older than opt.Retention is dropped; a non-terminal task is
// kept whatever its age, as the journal keeps it.
func NewJournalTaskStore(j *journal.Journal, opt journal.Options) (*JournalTaskStore, error) {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Retention == 0 {
		opt.Retention = journal.DefaultRetention
	}
	all, err := j.Records()
	if err != nil {
		return nil, err
	}
	var recs []journal.Record
	latest := map[string]journal.Record{}
	for _, r := range all {
		if r.Type == journal.TypeTask && r.TaskID != "" {
			recs = append(recs, r)
			latest[r.TaskID] = r
		}
	}
	cutoff := opt.Now().Add(-opt.Retention)
	recs = slices.DeleteFunc(recs, func(r journal.Record) bool {
		l := latest[r.TaskID]
		return !openTaskState(l.State) && l.At.Before(cutoff)
	})
	return &JournalTaskStore{j: j, recs: recs}, nil
}

func (s *JournalTaskStore) Load() []journal.Record { return slices.Clone(s.recs) }

func (s *JournalTaskStore) Save(r journal.Record) error { return s.j.Append(r) }

var taskStateNames = map[sdk.TaskState]string{
	sdk.TaskStateSubmitted: journal.StateSubmitted,
	sdk.TaskStateWorking:   journal.StateWorking,
	sdk.TaskStateCompleted: journal.StateCompleted,
	sdk.TaskStateFailed:    journal.StateFailed,
	sdk.TaskStateCanceled:  journal.StateCanceled,
}

func taskStateOf(name string) sdk.TaskState {
	for ts, n := range taskStateNames {
		if n == name {
			return ts
		}
	}
	return sdk.TaskStateUnspecified
}

func openTaskState(name string) bool {
	return name == journal.StateSubmitted || name == journal.StateWorking
}

func (t *task) record(state string, appended *sdk.Message) journal.Record {
	r := journal.Record{
		Type:      journal.TypeTask,
		TaskID:    string(t.id),
		MessageID: t.messageID,
		ContextID: t.contextID,
		Principal: t.principal,
		Kind:      string(t.kind),
		State:     state,
		Delivery:  t.delivery,
		Reason:    t.reason,
		Inbox:     t.inbox,
		Copy:      t.copy,
	}
	if t.copy {
		r.OrigKind = string(t.origKind)
	}
	if !t.deadline.IsZero() {
		d := t.deadline
		r.Deadline = &d
	}
	if appended != nil {
		r.Message, _ = json.Marshal(appended)
		if t.kind == msg.Ask && t.state == sdk.TaskStateCompleted {
			r.Text = messageText(appended)
		}
	}
	return r
}

func messageText(m *sdk.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		b.WriteString(p.Text())
	}
	return b.String()
}

// replay rebuilds the in-memory index from rs; a record with an unrecognized
// state is skipped, and records after a task's terminal one are ignored.
func (st *store) replay(rs []journal.Record) {
	for _, r := range rs {
		id := sdk.TaskID(r.TaskID)
		t := st.byID[id]
		if r.State == journal.StateRemoved {
			if t != nil {
				delete(st.byID, t.id)
				delete(st.byMsg, t.messageID)
				for _, h := range t.handles {
					delete(st.byMsg, h)
				}
			}
			continue
		}
		state := taskStateOf(r.State)
		if state == sdk.TaskStateUnspecified {
			continue
		}
		if t == nil {
			t = &task{id: id, contextID: r.ContextID, principal: r.Principal, messageID: r.MessageID, kind: msg.Kind(r.Kind), inbox: r.Inbox, copy: r.Copy, origKind: msg.Kind(r.OrigKind), done: make(chan struct{})}
			if r.Deadline != nil {
				t.deadline = *r.Deadline
			}
			st.byID[id] = t
			st.byMsg[t.messageID] = t
		}
		if t.state.Terminal() {
			continue
		}
		t.state, t.delivery, t.reason, t.updated = state, r.Delivery, r.Reason, r.At
		if len(r.Message) > 0 {
			var m sdk.Message
			if json.Unmarshal(r.Message, &m) == nil {
				switch i := slices.IndexFunc(t.history, func(h *sdk.Message) bool { return h.ID == m.ID && h.Role == sdk.MessageRoleUser }); {
				case m.Role == sdk.MessageRoleUser && i > 0 && deliveryState(&m) == string(adapter.Rejected):
					t.history = slices.Delete(t.history, i, i+1)
					st.unindex(t, m.ID)
				case m.Role == sdk.MessageRoleUser && i > 0:
					t.history[i] = &m
				case m.Role == sdk.MessageRoleUser && deliveryState(&m) == string(adapter.Rejected):
				default:
					t.history = append(t.history, &m)
					if state.Terminal() {
						t.status = &m
					}
					if m.Role == sdk.MessageRoleUser {
						st.index(t, m.ID)
					}
				}
			}
		}
		if state.Terminal() {
			close(t.done)
		}
	}
	for _, t := range st.byID {
		if len(t.history) == 0 {
			continue
		}
		if ct, ok := t.history[0].Metadata[MetaKey].(map[string]any); ok {
			if cid, ok := ct["client_message_id"].(string); ok && cid != "" {
				st.clientIDs[[2]string{t.principal, cid}] = t.messageID
			}
		}
	}
}
