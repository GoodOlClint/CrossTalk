package mcpserver

import (
	"context"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type SendRequest struct {
	To       string
	Text     string
	Kind     msg.Kind
	Delivery msg.Delivery
}

type AskRequest struct {
	To              string
	Text            string
	DeadlineSeconds int
}

// Result is the §8 `{message_id, state}` pair.
type Result struct {
	MessageID string        `json:"message_id"`
	State     adapter.State `json:"state"`
	Reason    string        `json:"reason,omitempty"`
}

type MailboxEntry struct {
	MessageID  string        `json:"message_id"`
	TaskID     *string       `json:"task_id"`
	InReplyTo  *string       `json:"in_reply_to"`
	From       string        `json:"from"`
	State      adapter.State `json:"state"`
	Text       string        `json:"text,omitempty"`
	InstanceID string        `json:"instance_id"`
	SessionRef string        `json:"session_ref"`
}

// Session is one harness session a Backend can see (today: loaded Codex threads).
type Session struct {
	Harness    adapter.Harness
	SessionRef string
	Name       string
	Cwd        string
	Status     string
	Reachable  bool
}

// Backend does the messaging work behind the tools. from/of/owner is the bound
// caller; target is a registered agent served by another shim process.
type Backend interface {
	Send(ctx context.Context, from *Agent, req SendRequest) (Result, error)
	Reply(ctx context.Context, from *Agent, messageID, text string) (Result, error)
	Ask(ctx context.Context, from *Agent, req AskRequest) (Result, error)
	Mailbox(ctx context.Context, of *Agent, after string, limit int) ([]MailboxEntry, error)
	Claim(ctx context.Context, owner *Agent, target registry.Descriptor) error
	Release(ctx context.Context, owner *Agent, target registry.Descriptor) error
	Discover(ctx context.Context) ([]Session, error)
}

// Binder is optionally implemented by a Backend that binds per-agent
// listeners and the harness binding. Bind runs once per agent registration,
// after the journal replay and before the descriptor is published (§3 order).
type Binder interface {
	Bind(ctx context.Context, a *Agent) (BindInfo, error)
}

type BindInfo struct {
	A2AURL          string
	HarnessEndpoint string
	HarnessVersion  string
	CredentialRef   string
}

const (
	CodeUnbound  = "unbound"
	CodeNotReady = "not_ready"
	CodeRejected = "rejected"
	CodeInvalid  = "invalid"
	CodeFailed   = "failed"
)

// Error is a tool error result. A Backend may return one to choose the code
// and reason the model sees.
type Error struct {
	Code   string `json:"error"`
	Reason string `json:"reason,omitempty"`
}

func (e *Error) Error() string {
	if e.Reason == "" {
		return e.Code
	}
	return e.Code + ": " + e.Reason
}

func rejected(reason string) *Error { return &Error{Code: CodeRejected, Reason: reason} }

// Unwired is the Backend used until the receiver, adapter and A2A slices are
// integrated: discovery sees no Codex threads and every send is unreachable.
type Unwired struct{}

var errUnwired = &Error{Code: string(adapter.Unreachable), Reason: "backend_not_wired"}

func (Unwired) Send(context.Context, *Agent, SendRequest) (Result, error) {
	return Result{}, errUnwired
}
func (Unwired) Reply(context.Context, *Agent, string, string) (Result, error) {
	return Result{}, errUnwired
}
func (Unwired) Ask(context.Context, *Agent, AskRequest) (Result, error) { return Result{}, errUnwired }
func (Unwired) Mailbox(context.Context, *Agent, string, int) ([]MailboxEntry, error) {
	return nil, nil
}
func (Unwired) Claim(context.Context, *Agent, registry.Descriptor) error   { return errUnwired }
func (Unwired) Release(context.Context, *Agent, registry.Descriptor) error { return errUnwired }
func (Unwired) Discover(context.Context) ([]Session, error)                { return nil, nil }
