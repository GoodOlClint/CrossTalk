package receiver

import (
	"encoding/json"
	"errors"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

var ErrUnknownCursor = errors.New("receiver: after names no journaled message")

// ErrNotCollected is what Confirm returns for an entry no pull has returned yet.
var ErrNotCollected = errors.New("receiver: entry is mailboxed, not collected")

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
	// PossiblySeen marks a delivered entry returned again: an earlier pull may not have reached the model.
	PossiblySeen bool `json:"possibly_seen,omitempty"`
}

// MailboxStates are the states receive_message returns by default (§8);
// accepted and queued are delivered by the harness itself, and confirmed
// entries have been seen.
var MailboxStates = []adapter.State{adapter.Unreachable, adapter.Unknown, adapter.Bounced, adapter.State(journal.StateRecovered), adapter.Mailboxed, adapter.State(journal.StateDelivered)}

// Admitted reports whether a dedupe state is one the receiver writes only
// after the message passed hop, policy and size admission and was claimed
// for dispatch. submitted precedes policy; rejected, canceled, bounced,
// recovered and unreachable (the last two written by Recover for unclaimed
// messages) are not evidence of admission.
func Admitted(state string) bool {
	switch adapter.State(state) {
	case adapter.Accepted, adapter.Queued, adapter.Unknown, adapter.Mailboxed, adapter.State(journal.StateDelivered), adapter.State(journal.StateConfirmed):
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

// Mailbox is Pull without the drain flag.
func (r *Receiver) Mailbox(after string, limit int) ([]Entry, error) {
	es, _, err := r.Pull(after, limit)
	return es, err
}

// Confirm records that a reply named messageID: a delivered entry becomes
// confirmed, a confirmed one stays so, and a mailboxed one is ErrNotCollected.
// An id that names no inbound entry is a no-op.
func (r *Receiver) Confirm(messageID string) error {
	r.pullMu.Lock()
	defer r.pullMu.Unlock()
	sender, ok := r.j.DedupeSender(messageID)
	if !ok {
		return nil
	}
	switch st, _ := r.j.Dedupe(sender, messageID); st {
	case journal.StateMailboxed:
		return ErrNotCollected
	case journal.StateDelivered:
		return r.j.SetDedupeState(sender, messageID, journal.StateConfirmed)
	}
	return nil
}

// Pull returns, in journal order, the inbound messages whose latest state
// is unreachable, unknown, bounced, recovered, mailboxed or delivered, and the outbound messages whose
// latest state is unknown. A mailboxed entry is marked delivered by the pull
// that returns it; a delivered one is returned again with PossiblySeen and
// marked confirmed (ADR 0028, R191). more is true when the page limit left
// mailboxed entries behind. after is a message_id cursor over the whole
// journal and settles nothing; limit <= 0 means no limit.
func (r *Receiver) Pull(after string, limit int) ([]Entry, bool, error) {
	r.pullMu.Lock()
	defer r.pullMu.Unlock()
	rs, err := r.j.Records()
	if err != nil {
		return nil, false, err
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
			return nil, false, ErrUnknownCursor
		}
	}
	var out []Entry
	var keys []dedupeKey
	stopped := len(order)
	for oi := start; oi < len(order); oi++ {
		mk := order[oi]
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
			out, keys = append(out, en), append(keys, dedupeKey{})
			if limit > 0 && len(out) == limit {
				stopped = oi + 1
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
		if e.latest.State == journal.StateDelivered {
			en.PossiblySeen = true
		}
		out, keys = append(out, en), append(keys, k)
		if limit > 0 && len(out) == limit {
			stopped = oi + 1
			break
		}
	}
	more := false
	for _, mk := range order[stopped:] {
		if !mk.out && idx[mk.dedupeKey].latest.State == journal.StateMailboxed {
			more = true
		}
	}
	for i := range out {
		if keys[i] == (dedupeKey{}) || out[i].State != journal.StateMailboxed && !out[i].PossiblySeen {
			continue
		}
		next := journal.StateDelivered
		if out[i].PossiblySeen {
			next = journal.StateConfirmed
		}
		if err := r.j.SetDedupeState(keys[i].sender, keys[i].id, next); err != nil {
			if i == 0 {
				return nil, false, err
			}
			return out[:i], true, nil
		}
		out[i].State = journal.StateDelivered
	}
	return out, more, nil
}

// Recover applies the §5 replay rules after journal.Open: an unresolved
// dispatch is never re-injected; it becomes unknown (the Claude adapter has
// no evidence to reconcile with) or unreachable when it was bound to another
// conversation. Open routes for the current conversation are reloaded;
// routes and accepted-but-unclaimed messages bound to another conversation
// are closed and unreachable. A submitted message with no claim takes the
// state strandedState gives it.
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
	tasked := map[string]bool{}
	for _, rec := range rs {
		if rec.Type == journal.TypeTask && rec.MessageID != "" {
			tasked[rec.MessageID] = true
		}
	}
	var errs []error
	for _, d := range rp.Dispatching {
		st := adapter.Unknown
		if !d.Binding.SameConversation(cur) {
			st = adapter.Unreachable
		}
		errs = append(errs, r.j.ResolveFor(d.Sender, d.MessageID, string(st)))
		for _, s := range senders[d.MessageID] {
			if scopeOfKey(s) == d.Sender {
				errs = append(errs, r.j.SetDedupeState(s, d.MessageID, string(st)))
			}
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
		case u.Type == journal.TypeDedupe && u.State == journal.StateSubmitted:
			if l, claimed := r.j.DispatchStateFor(scopeOfKey(u.Sender), u.MessageID); claimed {
				if l.Type == journal.TypeOutcome && l.State == journal.StateMailboxed {
					errs = append(errs, r.j.SetDedupeState(u.Sender, u.MessageID, journal.StateMailboxed))
				}
				continue
			}
			if st := r.strandedState(u, u.Binding.SameConversation(cur), tasked[u.MessageID], self); st != "" {
				errs = append(errs, r.j.SetDedupeState(u.Sender, u.MessageID, st))
			}
		}
	}
	return errors.Join(errs...)
}

// strandedState returns the dedupe state for a submitted record with no
// dispatch claim, or "" to leave it submitted. A record with a task record or
// a task_id came through A2A: a first message belongs to a2a.Resume, and a
// continuation stays submitted because its SendMessage failed with the crash.
// Any other record came through uds, whose policy reads only the kind, so an
// empty Sender stands in for the identity the journal does not keep.
func (r *Receiver) strandedState(u journal.Record, sameConversation, tasked bool, self Recipient) string {
	var m msg.Message
	parsed := json.Unmarshal(u.Message, &m) == nil
	viaA2A := tasked || parsed && m.TaskID != nil
	switch {
	case viaA2A && sameConversation:
		return ""
	case !viaA2A && (!parsed || r.admission(UDS, m, Sender{}, self) != ""):
		return string(adapter.Rejected)
	case !sameConversation:
		return string(adapter.Unreachable)
	}
	return journal.StateRecovered
}
