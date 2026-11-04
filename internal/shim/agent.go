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
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

var ErrSessionMismatch = errors.New("shim: harness session does not match the caller")

// AgentState is one bound agent's per-agent state in this process (§9):
// harness binding, receiver over its journal, uds ingress and A2A listener.
type AgentState struct {
	shim    *Shim
	agent   *mcpserver.Agent
	binding adapter.Binding
	harness harness.Harness
	udsPath string

	rcv *receiver.Receiver
	uds harness.Ingress
	a2a A2AListener

	ctx    context.Context
	cancel context.CancelFunc
	serve  chan struct{}

	replyMu sync.Mutex

	mu       sync.Mutex
	up       bool
	closed   bool
	deadline map[string]*time.Timer

	closeDone chan struct{}
	closeErr  error
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

// BindHarness confirms which conversation the agent is (§3 F2) through the
// harness that identified it.
func (s *Shim) BindHarness(ctx context.Context, a *mcpserver.Agent) (adapter.Binding, error) {
	st := &AgentState{shim: s, agent: a, udsPath: s.reg.UDSPath(a.ID()), deadline: map[string]*time.Timer{}}
	h := harness.Lookup(s.opt.Harnesses, a.Harness())
	if h == nil {
		return adapter.Binding{}, fmt.Errorf("shim: unknown harness %q", a.Harness())
	}
	b, ok, err := h.Bind(ctx, a.Request())
	if errors.Is(err, harness.ErrAmbiguous) {
		s.log.Warn("shim: session reachable through several endpoints; inbound unreachable", "harness", a.Harness(), "session", a.SessionRef(), "err", err)
		err = nil
	}
	if err != nil {
		return adapter.Binding{}, err
	}
	if !ok {
		return adapter.Binding{}, fmt.Errorf("shim: harness %q does not claim the caller", a.Harness())
	}
	if b.SessionRef != a.SessionRef() {
		return adapter.Binding{}, fmt.Errorf("%w: %s != %s", ErrSessionMismatch, b.SessionRef, a.SessionRef())
	}
	st.harness = h
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
		Journal: a.Journal(),
		Adapter: st.injector(st.harness),
		Self:    st.self,
		Now:     s.opt.Now,
	})
	if err := st.rcv.Recover(a.Replay()); err != nil {
		return mcpserver.BindInfo{}, err
	}
	st.ctx, st.cancel = context.WithCancel(s.ctx)
	var u harness.Ingress
	if s.ingress != nil {
		u = s.ingress.NewIngress(harness.IngressConfig{Path: st.udsPath, Directory: s.reg, Receiver: st.rcv, StartTime: s.reg.StartTime, PeerPID: s.udsPeerPID})
	}
	if u != nil {
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
	}
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
	st.rearmDeadlines()
	return info, nil
}

// Published starts replaying a's reply obligations; mcpserver calls it once
// a's descriptor is published.
func (s *Shim) Published(a *mcpserver.Agent) {
	s.mu.Lock()
	st := s.agents[a.ID()]
	s.mu.Unlock()
	if st != nil && st.agent == a && st.started() {
		go st.retryObligations()
	}
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

// HandlerCloseWait bounds how long closeState waits, in total, for the A2A
// listener and the UDS handlers to stop after their context is cancelled.
var HandlerCloseWait = 5 * time.Second

func (s *Shim) closeState(st *AgentState) error {
	st.mu.Lock()
	if st.closed {
		done := st.closeDone
		st.mu.Unlock()
		<-done
		return st.closeErr
	}
	st.closed = true
	st.closeDone = make(chan struct{})
	defer close(st.closeDone)
	for _, t := range st.deadline {
		t.Stop()
	}
	st.mu.Unlock()
	if st.cancel != nil {
		st.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), HandlerCloseWait)
	defer cancel()
	a2aDone, udsDone := make(chan error, 1), make(chan error, 1)
	if st.a2a.Close != nil {
		go func() { a2aDone <- st.a2a.Close(ctx) }()
	} else {
		a2aDone <- nil
	}
	if st.uds != nil {
		go func() {
			err := st.uds.Close()
			<-st.serve
			udsDone <- err
		}()
	} else {
		udsDone <- nil
	}
	var errs []error
	timedOut := false
	for range 2 {
		select {
		case err := <-a2aDone:
			timedOut = timedOut || errors.Is(err, context.DeadlineExceeded)
			errs = append(errs, err)
		case err := <-udsDone:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		case <-ctx.Done():
			timedOut = true
		}
		if timedOut {
			break
		}
	}
	if timedOut {
		s.log.Warn("shim: handlers did not stop; keeping agent ownership", "uds", st.udsPath, "wait", HandlerCloseWait)
		st.closeErr = errors.Join(append(errs, fmt.Errorf("%w after %v", mcpserver.ErrCloseTimeout, HandlerCloseWait))...)
		return st.closeErr
	}
	if st.uds != nil {
		for _, p := range st.uds.Files() {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// injector is h as this agent injects into it: Delivery.ReplyAddress is h's
// native reply address for this agent.
func (st *AgentState) injector(h harness.Harness) adapter.Adapter {
	return injector{Harness: h, reply: h.NativeReplyAddress(harness.Self{Binding: st.binding, UDSPath: st.udsPath})}
}

type injector struct {
	harness.Harness
	reply string
}

func (i injector) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	d.ReplyAddress = i.reply
	return i.Harness.Inject(ctx, m, d)
}
