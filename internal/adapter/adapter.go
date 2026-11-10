package adapter

import (
	"context"
	"errors"

	"github.com/GoodOlClint/crosstalk/internal/msg"
)

// ErrNoEndpoint is what Inject wraps when the target session has no harness
// endpoint to deliver to; the receiver refuses an ask on it (§8).
var ErrNoEndpoint = errors.New("no harness endpoint bound")

type State string

const (
	Accepted    State = "accepted"
	Queued      State = "queued"
	Rejected    State = "rejected"
	Unreachable State = "unreachable"
	Unknown     State = "unknown"
	// Bounced is a mailbox-entry state only; Inject never returns it.
	Bounced State = "bounced"
)

// Injectable reports whether s is a state Inject may return.
func (s State) Injectable() bool {
	switch s {
	case Accepted, Queued, Rejected, Unreachable, Unknown:
		return true
	}
	return false
}

type Harness string

const (
	Claude Harness = "claude"
	Codex  Harness = "codex"
)

type Binding struct {
	Harness         Harness
	SessionRef      string
	HarnessEndpoint string
	HarnessVersion  string
	HarnessPID      int
}

type Delivery struct {
	Mode           msg.Delivery
	Target         Binding
	VerifiedSender bool
	// SenderHarness is the sender's harness name as the transport proved it; "" when unknown.
	SenderHarness string
	// SenderAgentID is the sending agent's agent_id, set only when the sender's shim injects into a session with no CrossTalk binding; "" otherwise.
	SenderAgentID string
	// Inbox is the role inbox the message was addressed to; "" for a direct message.
	Inbox string
	// ReplyAddress is the target harness's NativeReplyAddress for the
	// injecting agent; "" when it has none.
	ReplyAddress string
}

type SessionStatus struct {
	Loaded    bool
	Busy      bool
	Reachable bool
}

type Adapter interface {
	Register(ctx context.Context, sessionRef string) (Binding, error)
	Inject(ctx context.Context, m msg.Message, d Delivery) (State, error)
	Status(ctx context.Context, sessionRef string) (SessionStatus, error)
}
