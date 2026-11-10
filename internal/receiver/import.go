package receiver

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// Delivered is an inform a sender's shim injected into this session before it
// bound. The caller has already authenticated it.
type Delivered struct {
	Sender      string
	Message     msg.Message
	DeliveredAt time.Time
	State       string
}

var ErrNotImportable = errors.New("receiver: delivery cannot be imported")

// ImportDelivered journals the two effects of a live delivery for d: the
// dedupe record, then an open inform route stamped min(DeliveredAt, now). It
// never injects and never reopens a route that has a closed record; a retry
// writes only the record that is missing.
func (r *Receiver) ImportDelivered(d Delivered) error {
	m := d.Message
	if d.Sender == "" || d.Sender == PrincipalLocalUser || IsRemoteKey(d.Sender) || m.MessageID == "" || m.Kind != msg.Inform || d.State != string(adapter.Accepted) && d.State != string(adapter.Queued) {
		return ErrNotImportable
	}
	now := r.now().UTC()
	at := d.DeliveredAt.UTC()
	if at.IsZero() || at.After(now) {
		at = now
	}
	if now.Sub(at) > InformRouteTTL {
		return nil
	}
	session := r.self().Binding.SessionRef
	rs, err := r.j.Records()
	if err != nil {
		return err
	}
	imported := map[string]bool{}
	held := false
	for _, rec := range rs {
		if rec.Type != journal.TypeDedupe || rec.MessageID != m.MessageID {
			continue
		}
		if rec.Imported {
			imported[rec.Sender] = true
		}
		held = held || rec.Sender != d.Sender
	}
	if held {
		for s := range imported {
			if s != d.Sender {
				r.closeImported(session, m.MessageID, s)
			}
		}
		return nil
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, dup, err := r.j.AcceptRecord(journal.Record{Sender: d.Sender, MessageID: m.MessageID, State: d.State, Message: body, Imported: true})
	if errors.Is(err, journal.ErrIDInUse) {
		return nil
	}
	if err != nil {
		return err
	}
	if dup && !imported[d.Sender] {
		return nil
	}
	rt := route{Session: session, MessageID: m.MessageID, Sender: d.Sender, Name: m.From, Kind: msg.Inform}
	if _, ok := r.j.Latest(rt.record("")); ok {
		return nil
	}
	return r.openRouteAt(rt, at)
}

func (r *Receiver) closeImported(session, messageID, sender string) {
	l, ok := r.j.Latest(route{Session: session, MessageID: messageID, Sender: sender}.record(""))
	if ok && l.State == journal.StateOpen {
		r.closeRoute(route{Session: session, MessageID: messageID, Sender: sender, Name: l.To, Kind: msg.Kind(l.Kind)})
	}
}

// ImportIncomplete reports a message whose import wrote the dedupe record
// but no route record, so a repeat of ImportDelivered would finish it.
func (r *Receiver) ImportIncomplete(messageID string) bool {
	rs, err := r.j.Records()
	if err != nil {
		return false
	}
	session := r.self().Binding.SessionRef
	for _, rec := range rs {
		if rec.Type != journal.TypeDedupe || rec.MessageID != messageID || !rec.Imported {
			continue
		}
		if _, ok := r.j.Latest(route{Session: session, MessageID: messageID, Sender: rec.Sender}.record("")); !ok {
			return true
		}
	}
	return false
}
