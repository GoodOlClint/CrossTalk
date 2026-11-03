package shim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

var codexClientInfo = appserver.ClientInfo{Name: "crosstalk", Version: mcpserver.Version}

var ErrSessionMismatch = errors.New("shim: parent Claude session does not match the caller")

// AgentState is one bound agent's per-agent state in this process (§9):
// harness binding, receiver over its journal, uds ingress and A2A listener.
type AgentState struct {
	shim    *Shim
	agent   *mcpserver.Agent
	binding adapter.Binding
	harness adapter.Adapter
	claude  *claude.Adapter
	udsPath string

	rcv *receiver.Receiver
	uds *udsingress.Server
	a2a A2AListener

	ctx    context.Context
	cancel context.CancelFunc
	serve  chan struct{}

	mu       sync.Mutex
	up       bool
	closed   bool
	deadline map[string]*time.Timer
}

func (st *AgentState) Agent() *mcpserver.Agent      { return st.agent }
func (st *AgentState) Binding() adapter.Binding     { return st.binding }
func (st *AgentState) Receiver() *receiver.Receiver { return st.rcv }
func (st *AgentState) UDSPath() string              { return st.udsPath }

// Receive is the single entry into this agent's receiver chain for an
// ingress (the A2A server calls it).
func (st *AgentState) Receive(ctx context.Context, in receiver.Inbound) receiver.Result {
	return st.rcv.Receive(ctx, in)
}

func (st *AgentState) started() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.up && !st.closed
}

// self is read per message: a claim changes role and owner (§7).
func (st *AgentState) self() receiver.Recipient {
	d := st.agent.Descriptor()
	r := receiver.Recipient{AgentID: st.agent.ID(), Name: st.agent.Name(), Role: d.Role, Binding: st.binding}
	if c, ok := st.agent.Journal().Latest(journal.Record{Type: journal.TypeClaim}); ok && c.Claimed {
		r.Owner = c.Owner
	}
	if r.Role == "" {
		r.Role = registry.RolePrimary
	}
	return r
}

func (s *Shim) state(a *mcpserver.Agent) (*AgentState, error) {
	s.mu.Lock()
	st := s.agents[a.ID()]
	s.mu.Unlock()
	if st == nil || !st.started() {
		return nil, &mcpserver.Error{Code: mcpserver.CodeNotReady, Reason: "agent_not_bound"}
	}
	return st, nil
}

// BindHarness confirms which conversation the agent is (§3 F2): Claude from
// the parent pid's session file, Codex from the one endpoint that has the
// thread loaded.
func (s *Shim) BindHarness(ctx context.Context, a *mcpserver.Agent) (adapter.Binding, error) {
	st := &AgentState{shim: s, agent: a, udsPath: s.reg.UDSPath(a.ID()), deadline: map[string]*time.Timer{}}
	st.claude = s.claudeAdapter(st.udsPath)
	var b adapter.Binding
	switch a.Harness() {
	case adapter.Claude:
		sess, err := st.claude.BindFromParent()
		if err != nil {
			return adapter.Binding{}, err
		}
		if sess.SessionID != a.SessionRef() {
			return adapter.Binding{}, fmt.Errorf("%w: %s != %s", ErrSessionMismatch, sess.SessionID, a.SessionRef())
		}
		b, st.harness = sess.Binding(), st.claude
	case adapter.Codex:
		var err error
		b, err = s.codex.Register(ctx, a.SessionRef())
		if errors.Is(err, codex.ErrAmbiguousEndpoint) {
			s.log.Warn("shim: codex thread loaded on several endpoints; inbound unreachable", "thread", a.SessionRef(), "err", err)
			b, err = adapter.Binding{Harness: adapter.Codex, SessionRef: a.SessionRef()}, nil
		}
		if err != nil {
			return adapter.Binding{}, err
		}
		st.harness = s.codex
	default:
		return adapter.Binding{}, fmt.Errorf("shim: unknown harness %q", a.Harness())
	}
	st.binding = b
	s.mu.Lock()
	if old := s.agents[a.ID()]; old != nil && old.started() {
		s.mu.Unlock()
		return adapter.Binding{}, &mcpserver.Error{Code: mcpserver.CodeRejected, Reason: "agent_held"}
	}
	s.agents[a.ID()] = st
	s.mu.Unlock()
	return b, nil
}

// Bind builds the receiver on the opened journal, recovers it, and binds the
// uds ingress and the A2A listener before mcpserver publishes the
// descriptor (§3 order).
func (s *Shim) Bind(ctx context.Context, a *mcpserver.Agent) (mcpserver.BindInfo, error) {
	s.mu.Lock()
	st := s.agents[a.ID()]
	s.mu.Unlock()
	if st == nil || st.agent != a {
		return mcpserver.BindInfo{}, errors.New("shim: Bind before BindHarness")
	}
	st.rcv = receiver.New(receiver.Config{
		Journal:   a.Journal(),
		Adapter:   st.harness,
		Self:      st.self,
		Forwarder: forwarder{st},
		Now:       s.opt.Now,
	})
	if err := st.rcv.Recover(a.Replay()); err != nil {
		return mcpserver.BindInfo{}, err
	}
	st.ctx, st.cancel = context.WithCancel(s.ctx)
	u := &udsingress.Server{Path: st.udsPath, Directory: s.reg, Receiver: st.rcv, StartTime: s.reg.StartTime, PeerPID: s.udsPeerPID}
	if err := u.Listen(); err != nil {
		return mcpserver.BindInfo{}, err
	}
	st.uds, st.serve = u, make(chan struct{})
	go func() {
		defer close(st.serve)
		if err := st.uds.Serve(st.ctx); err != nil {
			s.log.Warn("shim: uds ingress stopped", "agent", a.ID(), "err", err)
		}
	}()
	st.mu.Lock()
	st.up = true
	st.mu.Unlock()
	info := mcpserver.BindInfo{HarnessEndpoint: st.binding.HarnessEndpoint, HarnessVersion: st.binding.HarnessVersion}
	if s.opt.StartA2A != nil {
		l, err := s.opt.StartA2A(ctx, st)
		if err != nil {
			return mcpserver.BindInfo{}, err
		}
		st.a2a = l
		info.A2AURL, info.CredentialRef = l.URL, l.CredentialRef
	}
	go st.retryObligations()
	return info, nil
}

// CloseAgent stops the agent's listeners and removes its socket while
// mcpserver still holds the agent lock (§9 Exit). The journal is kept.
func (s *Shim) CloseAgent(a *mcpserver.Agent) error {
	s.mu.Lock()
	st := s.agents[a.ID()]
	if st != nil && st.agent == a {
		delete(s.agents, a.ID())
	} else {
		st = nil
	}
	s.mu.Unlock()
	if st == nil {
		return nil
	}
	return s.closeState(st)
}

func (s *Shim) closeState(st *AgentState) error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	for _, t := range st.deadline {
		t.Stop()
	}
	st.mu.Unlock()
	var errs []error
	if st.a2a.Close != nil {
		errs = append(errs, st.a2a.Close())
	}
	if st.uds != nil {
		if err := st.uds.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
		<-st.serve
		if err := os.Remove(st.udsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if st.cancel != nil {
		st.cancel()
	}
	return errors.Join(errs...)
}
