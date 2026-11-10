package a2a

import (
	"errors"
	"fmt"
	"maps"
	"math"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

var errBadMessage = errors.New("a2a: invalid message")

func badMessage(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", sdk.ErrInvalidParams, errBadMessage, fmt.Sprintf(format, args...))
}

// decode normalizes an inbound A2A message (§4). From, To and SentAt are left
// to the receiver; message_id is the client's id only when it is a UUIDv7.
func decode(am *sdk.Message) (msg.Message, error) {
	m := msg.Message{Kind: msg.Inform, Delivery: msg.Immediate, ContextID: am.ContextID}
	if am.Role != sdk.MessageRoleUser {
		return m, badMessage("role %q is not user", am.Role)
	}
	if msg.IsUUIDv7(am.ID) {
		m.MessageID = am.ID
	} else {
		m.MessageID = msg.NewUUIDv7()
	}
	for i, p := range am.Parts {
		t, ok := p.Content.(sdk.Text)
		if !ok {
			return m, badMessage("part %d is not text", i)
		}
		m.Parts = append(m.Parts, msg.Part{Type: "text", Text: string(t)})
	}
	if len(m.Parts) == 0 {
		return m, badMessage("no parts")
	}
	raw, ok := am.Metadata[MetaKey]
	if !ok || raw == nil {
		return m, nil
	}
	ct, ok := raw.(map[string]any)
	if !ok {
		return m, badMessage("metadata.%s is not an object", MetaKey)
	}
	str := func(key string) (string, bool, error) {
		v, ok := ct[key]
		if !ok || v == nil {
			return "", false, nil
		}
		s, ok := v.(string)
		if !ok {
			return "", false, badMessage("metadata.%s.%s is not a string", MetaKey, key)
		}
		return s, true, nil
	}
	if s, ok, err := str("kind"); err != nil {
		return m, err
	} else if ok {
		m.Kind = msg.Kind(s)
	}
	if s, ok, err := str("delivery"); err != nil {
		return m, err
	} else if ok {
		m.Delivery = msg.Delivery(s)
	}
	if s, ok, err := str("in_reply_to"); err != nil {
		return m, err
	} else if ok {
		m.InReplyTo = &s
	}
	if v, ok := ct["hop_chain"]; ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			return m, badMessage("metadata.%s.hop_chain is not an array", MetaKey)
		}
		for _, h := range list {
			s, ok := h.(string)
			if !ok {
				return m, badMessage("metadata.%s.hop_chain entry is not a string", MetaKey)
			}
			m.HopChain = append(m.HopChain, s)
		}
	}
	if v, ok := ct["deadline_seconds"]; ok && v != nil {
		f, ok := v.(float64)
		if !ok || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
			return m, badMessage("metadata.%s.deadline_seconds is not a non-negative integer", MetaKey)
		}
		m.DeadlineSeconds = int(f)
	}
	return m, nil
}

// inboxMark is the role inbox a message was addressed to, from
// metadata.crosstalk.inbox, and whether this one is a watcher's copy, from
// metadata.crosstalk.inbox_copy.
type inboxMark struct {
	name string
	copy bool
}

func inboxOf(am *sdk.Message) (inboxMark, error) {
	ct, _ := am.Metadata[MetaKey].(map[string]any)
	var ib inboxMark
	if v, ok := ct["inbox"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return ib, badMessage("metadata.%s.inbox is not a string", MetaKey)
		}
		if err := registry.ValidInboxName(s); err != nil {
			return ib, badMessage("metadata.%s.inbox: %v", MetaKey, err)
		}
		ib.name = s
	}
	if v, ok := ct["inbox_copy"]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			return ib, badMessage("metadata.%s.inbox_copy is not a boolean", MetaKey)
		}
		ib.copy = b
	}
	if ib.copy && ib.name == "" {
		return ib, badMessage("metadata.%s.inbox_copy needs metadata.%s.inbox", MetaKey, MetaKey)
	}
	return ib, nil
}

// copyWire is the stored form of a copy's first message: its kind reads
// inform, so a reader that ignores the copy markers resumes an inform, and
// orig_kind keeps the kind it was authorized as.
func copyWire(wire *sdk.Message, orig msg.Kind) *sdk.Message {
	w := *wire
	w.Metadata = maps.Clone(wire.Metadata)
	if w.Metadata == nil {
		w.Metadata = map[string]any{}
	}
	ct := map[string]any{}
	if old, ok := w.Metadata[MetaKey].(map[string]any); ok {
		ct = maps.Clone(old)
	}
	ct["kind"] = string(msg.Inform)
	ct["orig_kind"] = string(orig)
	w.Metadata[MetaKey] = ct
	return &w
}

// encode is the client side of decode.
func encode(m msg.Message) *sdk.Message {
	id := m.MessageID
	if id == "" {
		id = msg.NewUUIDv7()
	}
	am := &sdk.Message{ID: id, Role: sdk.MessageRoleUser, ContextID: m.ContextID}
	if m.TaskID != nil {
		am.TaskID = sdk.TaskID(*m.TaskID)
	}
	for _, p := range m.Parts {
		am.Parts = append(am.Parts, sdk.NewTextPart(p.Text))
	}
	ct := map[string]any{}
	if m.Kind != "" {
		ct["kind"] = string(m.Kind)
	}
	if m.Delivery != "" {
		ct["delivery"] = string(m.Delivery)
	}
	if m.InReplyTo != nil {
		ct["in_reply_to"] = *m.InReplyTo
	}
	if len(m.HopChain) > 0 {
		hc := make([]any, len(m.HopChain))
		for i, h := range m.HopChain {
			hc[i] = h
		}
		ct["hop_chain"] = hc
	}
	if m.DeadlineSeconds > 0 {
		ct["deadline_seconds"] = m.DeadlineSeconds
	}
	if m.Inbox != "" {
		ct["inbox"] = m.Inbox
		if m.InboxCopy {
			ct["inbox_copy"] = true
		}
	}
	if len(ct) > 0 {
		am.Metadata = map[string]any{MetaKey: ct}
	}
	return am
}

var admissionReasons = map[string]bool{
	receiver.ReasonTooLarge:       true,
	receiver.ReasonTooManyPending: true,
	ReasonTooManyWaits:            true,
	ReasonTooManyAsks:             true,
}

// rejection is the JSON-RPC error for a message that created no task: an
// admission rejection (§5 C9) or a rejected continuation.
// Rejection is the JSON-RPC error a definite refusal travels as, so the
// gateway's refusals read exactly like the loopback server's.
func Rejection(messageID, reason string) error { return rejection(messageID, reason) }

func rejection(messageID, reason string) error {
	base := sdk.ErrInvalidParams
	switch {
	case admissionReasons[reason]:
		base = sdk.ErrServerError
	case reason == receiver.ReasonPolicy || reason == receiver.ReasonUDSKind:
		base = sdk.ErrUnauthorized
	}
	return sdk.NewError(base, "rejected: "+reason).WithDetails(map[string]any{
		MetaKey: map[string]any{"state": string(adapter.Rejected), "reason": reason, "message_id": messageID},
	})
}

// outcome maps a receiver state to a task state (§5 task mapping). ask
// leaves an accepted or queued task working until CompleteAsk.
func outcome(k msg.Kind, st adapter.State) sdk.TaskState {
	switch st {
	case adapter.Accepted, adapter.Queued:
		if k == msg.Ask {
			return sdk.TaskStateWorking
		}
		return sdk.TaskStateCompleted
	case adapter.Rejected, adapter.Unreachable:
		return sdk.TaskStateFailed
	case journal.StateCanceled:
		return sdk.TaskStateCanceled
	}
	return sdk.TaskStateWorking
}

// DeliveryState maps a task back to the delivery state it reports:
// metadata.crosstalk.state when present, else one derived from the task state
// and metadata.crosstalk.reason.
func DeliveryState(t *sdk.Task) adapter.State {
	ct, _ := t.Metadata[MetaKey].(map[string]any)
	if s, ok := ct["state"].(string); ok && s != "" {
		return adapter.State(s)
	}
	reason, _ := ct["reason"].(string)
	switch t.Status.State {
	case sdk.TaskStateSubmitted:
		return journal.StateSubmitted
	case sdk.TaskStateWorking:
		return journal.StateDispatching
	case sdk.TaskStateCompleted:
		return adapter.Accepted
	case sdk.TaskStateCanceled:
		return journal.StateCanceled
	case sdk.TaskStateFailed, sdk.TaskStateRejected:
		if reason == string(adapter.Unreachable) {
			return adapter.Unreachable
		}
		if reason == ReasonDeadline {
			return adapter.Accepted
		}
		return adapter.Rejected
	}
	return adapter.Unknown
}
