package mcpserver

import (
	"context"
	"sync"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// FakeCall is one recorded Backend call. From is the caller's agent_id and
// FromName its alias at call time.
type FakeCall struct {
	Op        string
	From      string
	FromName  string
	Send      SendRequest
	Ask       AskRequest
	MessageID string
	Text      string
	Target    string
}

// FakeBackend records calls and answers with scripted values. Bind, when
// set, makes it a Binder.
type FakeBackend struct {
	mu       sync.Mutex
	calls    []FakeCall
	sessions []Session
	mailbox  map[string][]MailboxEntry
	SendErr  error
	ClaimErr error
	Bind     func(ctx context.Context, a *Agent) (BindInfo, error)
	// Harnesses' sessions are listed by Discover after the scripted ones.
	Harnesses []harness.Harness
}

var _ Backend = (*FakeBackend)(nil)

func NewFakeBackend() *FakeBackend {
	return &FakeBackend{mailbox: map[string][]MailboxEntry{}}
}

func (f *FakeBackend) SetSessions(s ...Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append([]Session(nil), s...)
}

func (f *FakeBackend) SetMailbox(agentID string, e ...MailboxEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mailbox[agentID] = e
}

func (f *FakeBackend) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.calls...)
}

func (f *FakeBackend) record(c FakeCall, a *Agent) {
	c.From, c.FromName = a.ID(), a.Name()
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *FakeBackend) Send(_ context.Context, a *Agent, r SendRequest) (Result, error) {
	f.record(FakeCall{Op: "send", Send: r}, a)
	if f.SendErr != nil {
		return Result{}, f.SendErr
	}
	return Result{MessageID: msg.NewUUIDv7(), State: adapter.Accepted}, nil
}

func (f *FakeBackend) Reply(_ context.Context, a *Agent, id, text string) (Result, error) {
	f.record(FakeCall{Op: "reply", MessageID: id, Text: text}, a)
	return Result{MessageID: msg.NewUUIDv7(), State: adapter.Accepted}, nil
}

func (f *FakeBackend) Ask(_ context.Context, a *Agent, r AskRequest) (Result, error) {
	f.record(FakeCall{Op: "ask", Ask: r}, a)
	return Result{MessageID: msg.NewUUIDv7(), State: adapter.Accepted}, nil
}

func (f *FakeBackend) Mailbox(_ context.Context, a *Agent, after string, limit int) ([]MailboxEntry, bool, error) {
	f.record(FakeCall{Op: "mailbox", MessageID: after}, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	es := f.mailbox[a.ID()]
	more := false
	if limit > 0 && len(es) > limit {
		es, more = es[:limit], true
	}
	return es, more, nil
}

func (f *FakeBackend) Claim(_ context.Context, a *Agent, t registry.Descriptor) error {
	f.record(FakeCall{Op: "claim", Target: t.AgentID}, a)
	return f.ClaimErr
}

func (f *FakeBackend) Release(_ context.Context, a *Agent, t registry.Descriptor) error {
	f.record(FakeCall{Op: "release", Target: t.AgentID}, a)
	return f.ClaimErr
}

func (f *FakeBackend) Discover(ctx context.Context) ([]Session, error) {
	f.mu.Lock()
	out := append([]Session(nil), f.sessions...)
	hs := f.Harnesses
	f.mu.Unlock()
	for _, h := range hs {
		ss, err := h.Discover(ctx)
		if err != nil {
			return out, err
		}
		out = append(out, ss...)
	}
	return out, nil
}

// fakeBinder wraps a FakeBackend whose Bind hook is set.
type fakeBinder struct{ *FakeBackend }

func (b fakeBinder) Bind(ctx context.Context, a *Agent) (BindInfo, error) {
	return b.FakeBackend.Bind(ctx, a)
}

// AsBackend returns f, or a Binder wrapping f when f.Bind is set.
func (f *FakeBackend) AsBackend() Backend {
	if f.Bind != nil {
		return fakeBinder{f}
	}
	return f
}
