// Package shim is the real mcpserver Backend: per-agent harness binding,
// journal, receiver and uds ingress, and outbound delivery by name (design
// §3, §5, §8, §9).
package shim

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const (
	ReasonNoTransport       = "no_transport"
	ReasonNoSuchAgent       = "no_such_agent"
	ReasonAmbiguousName     = "ambiguous_name"
	ReasonSelf              = "self"
	ReasonNativeInformOnly  = "native_inform_only"
	ReasonNoSuchMessage     = "no_such_message"
	ReasonSenderUnverified  = "sender_unverified"
	ReasonNoReplyHook       = "no_reply_hook"
	ReasonNotPrimary        = "owner_not_primary"
	ReasonNotServedHere     = "not_served_here"
	DefaultDiscoveryTimeout = 3 * time.Second
	SendCapShort            = 20
	SendWindowShort         = 10 * time.Minute
	SendCapLong             = 60
	SendWindowLong          = time.Hour
)

// Transport carries a message to an agent served by a shim (A2A in
// production). returnImmediately is always true for send_message and ask
// (§8).
type Transport interface {
	Send(ctx context.Context, from, to registry.Descriptor, m msg.Message, returnImmediately bool) (adapter.State, error)
}

// TaskTransport is a Transport that also returns the far side's task id,
// which the outbound journal record keeps.
type TaskTransport interface {
	SendTask(ctx context.Context, from, to registry.Descriptor, m msg.Message, returnImmediately bool) (string, adapter.State, error)
}

// ClaimTransport asks the shim serving target to record or end owner's claim
// on it (§7). A Transport that does not implement it cannot carry claims.
type ClaimTransport interface {
	SetClaim(ctx context.Context, owner, target registry.Descriptor, claimed bool) error
}

// A2AListener is what Options.StartA2A returns for one agent; URL and
// CredentialRef are published in its descriptor.
type A2AListener struct {
	URL           string
	CredentialRef string
	// Close must return by ctx's deadline.
	Close func(ctx context.Context) error
}

type Options struct {
	// StateDir is ~/.local/state/crosstalk; "" means registry.DefaultDir().
	StateDir string
	// Harnesses are tried in order for binding and unioned for discovery
	// (ADR 0008); none means every caller is unbound.
	Harnesses []harness.Harness
	// PPID is the parent pid every request context carries; 0 means
	// os.Getppid().
	PPID int
	// Transport carries messages to shim-served agents; nil makes every such
	// send unreachable with reason no_transport.
	Transport Transport
	// StartA2A, when set, starts the agent's A2A server after its receiver
	// exists and before its descriptor is published.
	StartA2A func(ctx context.Context, st *AgentState) (A2AListener, error)
	// OnReply, when set, runs for every reply this agent's session makes,
	// after the obligation is journaled and before the route closes and the
	// reply is sent; it completes the ask's A2A task. An error leaves the
	// obligation pending and OnReply runs again on the retry or replay.
	OnReply          func(ctx context.Context, st *AgentState, original, reply msg.Message) error
	DiscoveryTimeout time.Duration
	Now              func() time.Time
	Logger           *slog.Logger
}

// Shim implements mcpserver.Backend, mcpserver.Binder,
// mcpserver.HarnessBinder and mcpserver.AgentCloser.
type Shim struct {
	opt     Options
	reg     *registry.Registry
	ingress harness.NativeIngress
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	agents map[string]*AgentState

	udsPeerPID func(*net.UnixConn) (int, error)
}

var (
	_ mcpserver.Backend       = (*Shim)(nil)
	_ mcpserver.Binder        = (*Shim)(nil)
	_ mcpserver.HarnessBinder = (*Shim)(nil)
	_ mcpserver.AgentCloser   = (*Shim)(nil)
	_ mcpserver.Publisher     = (*Shim)(nil)
)

func New(opt Options) (*Shim, error) {
	if opt.StateDir == "" {
		d, err := registry.DefaultDir()
		if err != nil {
			return nil, err
		}
		opt.StateDir = d
	}
	if opt.PPID == 0 {
		opt.PPID = os.Getppid()
	}
	if opt.DiscoveryTimeout == 0 {
		opt.DiscoveryTimeout = DefaultDiscoveryTimeout
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	s := &Shim{opt: opt, reg: registry.New(opt.StateDir), log: opt.Logger, agents: map[string]*AgentState{}}
	s.reg.Superseded = harness.Superseded(opt.Harnesses)
	if s.log == nil {
		s.log = slog.Default()
	}
	for _, h := range opt.Harnesses {
		if ni, ok := h.(harness.NativeIngress); ok {
			s.ingress = ni
			break
		}
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// MCPOptions are the mcpserver options that share this shim's registry,
// harnesses and parent pid.
func (s *Shim) MCPOptions() mcpserver.Options {
	return mcpserver.Options{Backend: s, Registry: s.reg, Harnesses: s.opt.Harnesses, PPID: s.opt.PPID}
}

func (s *Shim) Registry() *registry.Registry { return s.reg }

// Agent returns the bound agent agentID served by this shim, or nil.
func (s *Shim) Agent(agentID string) *AgentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.agents[agentID]
	if st == nil || !st.started() {
		return nil
	}
	return st
}

// Close stops every agent still served. mcpserver.Server.Close normally
// closes each agent first through CloseAgent; Close is the backstop for a
// ctx-cancelled exit.
func (s *Shim) Close() error {
	s.mu.Lock()
	states := make([]*AgentState, 0, len(s.agents))
	for _, st := range s.agents {
		states = append(states, st)
	}
	s.mu.Unlock()
	var errs []error
	for _, st := range states {
		errs = append(errs, s.closeState(st))
	}
	s.cancel()
	return errors.Join(errs...)
}

func (s *Shim) now() time.Time { return s.opt.Now().UTC() }
