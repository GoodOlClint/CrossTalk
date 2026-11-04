package receiver

import (
	"encoding/json"
	"errors"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

var ErrUnknownCursor = errors.New("receiver: after names no journaled message")

const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
)

type Entry struct {
	Direction  string       `json:"direction"`
	MessageID  string       `json:"message_id"`
	TaskID     *string      `json:"task_id"`
	From       string       `json:"from"`
	To         string       `json:"to,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	InReplyTo  *string      `json:"in_reply_to"`
	State      string       `json:"state"`
	InstanceID string       `json:"instance_id"`
	SessionRef string       `json:"session_ref"`
	Message    *msg.Message `json:"message,omitempty"`
}

// MailboxStates are the states receive_message returns by default (§8);
// accepted and queued are delivered by the harness itself.
var MailboxStates = []adapter.State{adapter.Unreachable, adapter.Unknown, adapter.Bounced}

// Admitted reports whether a dedupe state is one the receiver writes only
// after the message passed hop, policy and size admission and was claimed
// for dispatch. submitted precedes policy; rejected, canceled, bounced and
// unreachable (also written by Recover for unclaimed messages) are not
// evidence of admission.
func Admitted(state string) bool {
	switch adapter.State(state) {
	case adapter.Accepted, adapter.Queued, adapter.Unknown:
		return true
	}
	return false
}

func inMailbox(state string) bool {
	for _, s := range MailboxStates {
		if string(s) == state {
			return true
		}
	}
	return false
}

type dedupeKey struct{ sender, id string }

type entryState struct {
	latest  journal.Record
	binding journal.Binding
	message json.RawMessage
}

func dedupeEntries(rs []journal.Record) ([]dedupeKey, map[dedupeKey]*entryState) {
	var order []dedupeKey
	idx := map[dedupeKey]*entryState{}
	for _, rec := range rs {
		if rec.Type != journal.TypeDedupe {
			continue
		}
		k := dedupeKey{rec.Sender, rec.MessageID}
		e := idx[k]
		if e == nil {
			e = &entryState{binding: rec.Binding}
			idx[k] = e
			order = append(order, k)
		}
		e.latest = rec
		if len(rec.Message) > 0 {
			e.message = rec.Message
		}
	}
	return order, idx
}

type outboundState struct {
	latest  journal.Record
	taskID  string
	message json.RawMessage
}

type mailKey struct {
	dedupeKey
	out bool
}

// Mailbox returns, in journal order, the inbound messages whose latest state
// is unreachable, unknown or bounced, and the outbound messages whose
// latest state is unknown. after is a message_id cursor over the whole
// journal; limit <= 0 means no limit.
func (r *Receiver) Mailbox(after string, limit int) ([]Entry, error) {
	rs, err := r.j.Records()
	if err != nil {
		return nil, err
	}
	_, idx := dedupeEntries(rs)
	outs := map[string]*outboundState{}
	var order []mailKey
	seen := map[dedupeKey]bool{}
	for _, rec := range rs {
		switch rec.Type {
		case journal.TypeDedupe:
			if k := (dedupeKey{rec.Sender, rec.MessageID}); !seen[k] {
				seen[k] = true
				order = append(order, mailKey{dedupeKey: k})
			}
		case journal.TypeOutbound:
			o := outs[rec.MessageID]
			if o == nil {
				o = &outboundState{}
				outs[rec.MessageID] = o
				order = append(order, mailKey{dedupeKey: dedupeKey{id: rec.MessageID}, out: true})
			}
			o.latest = rec
			if rec.TaskID != "" {
				o.taskID = rec.TaskID
			}
			if len(rec.Message) > 0 {
				o.message = rec.Message
			}
		}
	}
	self := r.self().Name
	start := 0
	if after != "" {
		start = -1
		for i, k := range order {
			if k.id == after {
				start = i + 1
			}
		}
		if start < 0 {
			return nil, ErrUnknownCursor
		}
	}
	var out []Entry
	for _, mk := range order[start:] {
		k := mk.dedupeKey
		if mk.out {
			o := outs[k.id]
			if o.latest.State != string(adapter.Unknown) {
				continue
			}
			en := Entry{Direction: DirectionOutbound, MessageID: k.id, From: self, To: o.latest.To, State: o.latest.State, Reason: o.latest.Reason, InstanceID: o.latest.Binding.InstanceID, SessionRef: o.latest.Binding.SessionRef}
			if o.taskID != "" {
				t := o.taskID
				en.TaskID = &t
			}
			if o.latest.InReplyTo != "" {
				irt := o.latest.InReplyTo
				en.InReplyTo = &irt
			}
			if len(o.message) > 0 {
				var m msg.Message
				if json.Unmarshal(o.message, &m) == nil {
					en.Message = &m
				}
			}
			out = append(out, en)
			if limit > 0 && len(out) == limit {
				break
			}
			continue
		}
		e := idx[k]
		if !inMailbox(e.latest.State) {
			continue
		}
		en := Entry{Direction: DirectionInbound, MessageID: k.id, From: k.sender, State: e.latest.State, InstanceID: e.binding.InstanceID, SessionRef: e.binding.SessionRef}
		if len(e.message) > 0 {
			var m msg.Message
			if json.Unmarshal(e.message, &m) == nil {
				en.Message, en.From, en.TaskID, en.InReplyTo = &m, m.From, m.TaskID, m.InReplyTo
			}
		}
		out = append(out, en)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// Recover applies the §5 replay rules after journal.Open: an unresolved
// dispatch is never re-injected; it becomes unknown (the Claude adapter has
// no evidence to reconcile with) or unreachable when it was bound to another
// conversation. Open routes for the current conversation are reloaded;
// routes and accepted-but-unclaimed messages bound to another conversation
// are closed and unreachable.
func (r *Receiver) Recover(rp journal.Replay) error {
	self := r.self()
	cur := journal.Binding{HarnessEndpoint: self.Binding.HarnessEndpoint, SessionRef: self.Binding.SessionRef}
	rs, err := r.j.Records()
	if err != nil {
		return err
	}
	order, _ := dedupeEntries(rs)
	senders := map[string][]string{}
	for _, k := range order {
		senders[k.id] = append(senders[k.id], k.sender)
	}
	var errs []error
	for _, d := range rp.Dispatching {
		st := adapter.Unknown
		if !d.Binding.SameConversation(cur) {
			st = adapter.Unreachable
		}
		errs = append(errs, r.j.Resolve(d.MessageID, string(st)))
		for _, s := range senders[d.MessageID] {
			errs = append(errs, r.j.SetDedupeState(s, d.MessageID, string(st)))
		}
	}
	for _, u := range rp.Unresolved {
		switch {
		case u.Type == journal.TypeRoute && u.State == journal.StateOpen:
			rt := route{Session: u.Session, MessageID: u.MessageID, Sender: u.Sender, Name: u.To, Kind: msg.Kind(u.Kind), OpenedAt: u.At}
			if u.Binding.SameConversation(cur) && u.Session == self.Binding.SessionRef && !rt.expired(r.now()) {
				r.mu.Lock()
				r.routes = append(r.routes, rt)
				r.mu.Unlock()
				continue
			}
			errs = append(errs, r.j.Append(journal.Record{Type: journal.TypeRoute, Session: u.Session, MessageID: u.MessageID, Sender: u.Sender, To: u.To, Kind: u.Kind, State: journal.StateClosed}))
		case u.Type == journal.TypeDedupe && u.State == journal.StateSubmitted && !u.Binding.SameConversation(cur):
			if _, claimed := r.j.DispatchState(u.MessageID); claimed {
				continue
			}
			errs = append(errs, r.j.SetDedupeState(u.Sender, u.MessageID, string(adapter.Unreachable)))
		}
	}
	return errors.Join(errs...)
}
