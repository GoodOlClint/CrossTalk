package shim

import (
	"context"
	"errors"
	"sync"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// Loopback is an in-process Transport and ClaimTransport between attached
// shims. The sender is the registered agent of from, as A2A's per-agent
// bearer token would prove.
type Loopback struct {
	mu    sync.Mutex
	shims []*Shim
}

var (
	_ Transport      = (*Loopback)(nil)
	_ ClaimTransport = (*Loopback)(nil)
)

func NewLoopback() *Loopback { return &Loopback{} }

func (l *Loopback) Attach(s ...*Shim) {
	l.mu.Lock()
	l.shims = append(l.shims, s...)
	l.mu.Unlock()
}

func (l *Loopback) serving(agentID string) (*Shim, *AgentState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.shims {
		if st := s.Agent(agentID); st != nil {
			return s, st
		}
	}
	return nil, nil
}

func (l *Loopback) Send(ctx context.Context, from, to registry.Descriptor, m msg.Message, _ bool) (adapter.State, error) {
	_, st := l.serving(to.AgentID)
	if st == nil {
		return adapter.Unreachable, errors.New(ReasonNotServedHere)
	}
	res := st.Receive(ctx, receiver.Inbound{
		Ingress: receiver.A2A,
		Message: m,
		Sender:  receiver.Sender{Verified: true, AgentID: from.AgentID, Name: from.Name, Role: from.Role, Harness: from.Harness, PID: from.PID},
	})
	if res.Reason != "" {
		return res.State, errors.New(res.Reason)
	}
	return res.State, nil
}

func (l *Loopback) SetClaim(ctx context.Context, owner, target registry.Descriptor, claimed bool) error {
	s, _ := l.serving(target.AgentID)
	if s == nil {
		return &mcpserver.Error{Code: string(adapter.Unreachable), Reason: ReasonNotServedHere}
	}
	return s.ApplyClaim(ctx, owner, target.AgentID, claimed)
}
