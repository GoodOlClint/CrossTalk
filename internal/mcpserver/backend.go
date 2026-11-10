package mcpserver

import (
	"context"
	"errors"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
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
	// Hint tells the model its next step; Reason stays the stable code.
	Hint string `json:"hint,omitempty"`
	// Inbox and Responder are set for a send to a role inbox: its name and
	// the agent_id of the member that holds the message's reply route.
	Inbox     string `json:"inbox,omitempty"`
	Responder string `json:"responder,omitempty"`
	// Copies is the outcome of each watcher's copy of an inform or ask to an inbox.
	Copies []CopyResult `json:"copies,omitempty"`
}

// CopyResult is one watcher's copy of a message to an inbox.
type CopyResult struct {
	AgentID string        `json:"agent_id"`
	State   adapter.State `json:"state"`
	Reason  string        `json:"reason,omitempty"`
}

type MailboxEntry struct {
	Direction string        `json:"direction"`
	MessageID string        `json:"message_id"`
	TaskID    *string       `json:"task_id"`
	InReplyTo *string       `json:"in_reply_to"`
	From      string        `json:"from"`
	To        string        `json:"to,omitempty"`
	State     adapter.State `json:"state"`
	Reason    string        `json:"reason,omitempty"`
	Text      string        `json:"text,omitempty"`
	// Notice is the harness's warning that Text is peer text with no operator authority.
	Notice     string `json:"notice,omitempty"`
	InstanceID string `json:"instance_id"`
	SessionRef string `json:"session_ref"`
	// PossiblySeen marks a delivered entry returned again because an earlier pull may not have reached the model.
	PossiblySeen bool `json:"possibly_seen,omitempty"`
}

// Session is one harness session a Backend can see.
type Session = harness.Session

// Backend does the messaging work behind the tools. from/of/owner is the bound
// caller; target is a registered agent served by another shim process.
type Backend interface {
	Send(ctx context.Context, from *Agent, req SendRequest) (Result, error)
	Reply(ctx context.Context, from *Agent, messageID, text string) (Result, error)
	Ask(ctx context.Context, from *Agent, req AskRequest) (Result, error)
	// Mailbox returns a page of entries and whether mailboxed entries remain behind the page limit.
	Mailbox(ctx context.Context, of *Agent, after string, limit int) ([]MailboxEntry, bool, error)
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

// HarnessBinder is optionally implemented by a Backend that confirms the
// harness binding before the journal opens, so every journal record carries
// the real harness_endpoint (§5). It runs after the locks are taken and
// before Bind.
type HarnessBinder interface {
	BindHarness(ctx context.Context, a *Agent) (adapter.Binding, error)
}

// AgentCloser is optionally implemented by a Backend holding per-agent
// listeners. CloseAgent runs while the agent's locks are still held, before
// the descriptor is removed, and also when a registration fails after
// BindHarness; it must tolerate an agent it never finished binding. It waits
// on Agent.Drain within its budget, since the journal closes after it returns.
// An error wrapping ErrCloseTimeout means a handler may still dispatch or
// write, so the agent's locks and descriptor are kept until process exit.
type AgentCloser interface {
	CloseAgent(a *Agent) error
}

// Publisher is optionally implemented by a Backend with per-agent work that
// needs the agent's descriptor published first. Published runs after the
// registration publish succeeds and must not block.
type Publisher interface {
	Published(a *Agent)
}

// InboxNotifier is optionally implemented by a Backend that tells an inbox's
// other members about a membership change. It runs after the change is
// committed and the caller's lock is released; change completes the
// sentence "session <name> ...", and st is the inbox
// as the change left it.
type InboxNotifier interface {
	NotifyInbox(ctx context.Context, from *Agent, change string, st registry.InboxState)
}

var ErrCloseTimeout = errors.New("mcpserver: agent handlers did not stop")

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
	Code    string `json:"error"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

func (e *Error) Error() string {
	if e.Reason == "" {
		return e.Code
	}
	return e.Code + ": " + e.Reason
}

func rejected(reason string) *Error { return &Error{Code: CodeRejected, Reason: reason} }

// Unwired is the Backend used until the receiver, adapter and A2A slices are
// integrated: discovery sees no sessions and every send is unreachable.
type Unwired struct{}

var errUnwired = &Error{Code: string(adapter.Unreachable), Reason: "backend_not_wired"}

func (Unwired) Send(context.Context, *Agent, SendRequest) (Result, error) {
	return Result{}, errUnwired
}
func (Unwired) Reply(context.Context, *Agent, string, string) (Result, error) {
	return Result{}, errUnwired
}
func (Unwired) Ask(context.Context, *Agent, AskRequest) (Result, error) { return Result{}, errUnwired }
func (Unwired) Mailbox(context.Context, *Agent, string, int) ([]MailboxEntry, bool, error) {
	return nil, false, nil
}
func (Unwired) Claim(context.Context, *Agent, registry.Descriptor) error   { return errUnwired }
func (Unwired) Release(context.Context, *Agent, registry.Descriptor) error { return errUnwired }
func (Unwired) Discover(context.Context) ([]Session, error)                { return nil, nil }
