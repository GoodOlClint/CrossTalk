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
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
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
)

// Transport carries a message to an agent served by a shim (A2A in
// production). returnImmediately is always true for send_message and ask
// (§8).
type Transport interface {
	Send(ctx context.Context, from, to registry.Descriptor, m msg.Message, returnImmediately bool) (adapter.State, error)
}

// ClaimTransport asks the shim serving target to record or end owner's claim
// on it (§7). A Transport that does not implement it cannot carry claims.
type ClaimTransport interface {
	SetClaim(ctx context.Context, owner, target registry.Descriptor, claimed bool) error
}

// CodexHarness is the Codex adapter plus loaded-thread discovery.
type CodexHarness interface {
	adapter.Adapter
	LoadedThreads(ctx context.Context, extraEndpoints ...string) []codex.LoadedThread
}

// A2AListener is what Options.StartA2A returns for one agent; URL and
// CredentialRef are published in its descriptor.
type A2AListener struct {
	URL           string
	CredentialRef string
	Close         func() error
}

type Options struct {
	// StateDir is ~/.local/state/crosstalk; "" means registry.DefaultDir().
	StateDir string
	// SessionsDir is ~/.claude/sessions; "" means claude.DefaultSessionsDir().
	SessionsDir string
	// PPID is the Claude session pid; 0 means os.Getppid().
	PPID int
	// Alive reports whether a Claude session pid is running; nil means the
	// harness and mcpserver defaults.
	Alive func(pid int) bool
	// Codex is the Codex harness; nil means a codex.Adapter on the default
	// control socket.
	Codex CodexHarness
	// Transport carries messages to shim-served agents; nil makes every such
	// send unreachable with reason no_transport.
	Transport Transport
	// StartA2A, when set, starts the agent's A2A server after its receiver
	// exists and before its descriptor is published.
	StartA2A func(ctx context.Context, st *AgentState) (A2AListener, error)
	// OnReply, when set, runs for every reply this agent's session makes
	// (reply tool or a correlated native reply), after the obligation is
	// journaled and before the reply is sent; it completes the ask's A2A task.
	OnReply          func(ctx context.Context, st *AgentState, original, reply msg.Message) error
	DiscoveryTimeout time.Duration
	Now              func() time.Time
	Logger           *slog.Logger
}

// Shim implements mcpserver.Backend, mcpserver.Binder,
// mcpserver.HarnessBinder and mcpserver.AgentCloser.
type Shim struct {
	opt   Options
	reg   *registry.Registry
	codex CodexHarness
	log   *slog.Logger

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
)

func New(opt Options) (*Shim, error) {
	if opt.StateDir == "" {
		d, err := registry.DefaultDir()
		if err != nil {
			return nil, err
		}
		opt.StateDir = d
	}
	if opt.SessionsDir == "" {
		d, err := claude.DefaultSessionsDir()
		if err != nil {
			return nil, err
		}
		opt.SessionsDir = d
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
	s := &Shim{opt: opt, reg: registry.New(opt.StateDir), codex: opt.Codex, log: opt.Logger, agents: map[string]*AgentState{}}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.codex == nil {
		s.codex = &codex.Adapter{Logger: s.log, ClientInfo: codexClientInfo}
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// MCPOptions are the mcpserver options that share this shim's registry,
// sessions directory, parent pid and liveness check.
func (s *Shim) MCPOptions() mcpserver.Options {
	return mcpserver.Options{Backend: s, Registry: s.reg, SessionDir: s.opt.SessionsDir, PPID: s.opt.PPID, Alive: s.opt.Alive}
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

func (s *Shim) claudeAdapter(udsPath string) *claude.Adapter {
	ppid := s.opt.PPID
	return &claude.Adapter{SessionsDir: s.opt.SessionsDir, PPID: func() int { return ppid }, UDSPath: udsPath, Alive: s.opt.Alive}
}

func (s *Shim) now() time.Time { return s.opt.Now().UTC() }
