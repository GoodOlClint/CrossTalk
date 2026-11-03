// Package codex is the Codex harness adapter (design §6): binding to the
// app-server endpoint that hosts a thread, immediate delivery over
// turn/steer and turn/start, and next-turn delivery through `codex queue`.
package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"sync"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

// ReasonAmbiguousEndpoint is the §3 CQ10 reason for a thread loaded on more
// than one endpoint.
const ReasonAmbiguousEndpoint = "ambiguous_endpoint"

var (
	ErrAmbiguousEndpoint = errors.New("codex: " + ReasonAmbiguousEndpoint)
	ErrNoEndpoint        = errors.New("codex: no harness endpoint bound")
	ErrNoThread          = errors.New("codex: no thread id")
)

// Runner runs an external command. err is non-nil only when the command
// could not be started; exitCode is -1 when it was killed by a signal.
type Runner func(ctx context.Context, name string, args ...string) (exitCode int, output []byte, err error)

// ExecRunner runs the command with os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) (int, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil {
		return -1, out, err
	}
	return cmd.ProcessState.ExitCode(), out, nil
}

type Adapter struct {
	// ControlSocket is the managed daemon endpoint; "" means
	// appserver.DefaultControlSocket().
	ControlSocket string
	// Endpoints are further known endpoints (harness_endpoint values from
	// descriptors), queried alongside ControlSocket (§3 CQ10).
	Endpoints []string
	// CodexBin is the codex executable for `codex queue`; "" means "codex".
	CodexBin string
	// Run executes `codex queue`; nil means ExecRunner.
	Run        Runner
	Logger     *slog.Logger
	ClientInfo appserver.ClientInfo

	mu       sync.Mutex
	bindings map[string]adapter.Binding
}

var _ adapter.Adapter = (*Adapter)(nil)

func (a *Adapter) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

func (a *Adapter) clientInfo() appserver.ClientInfo {
	if a.ClientInfo.Name != "" {
		return a.ClientInfo
	}
	return appserver.ClientInfo{Name: "crosstalk", Version: "dev"}
}

// KnownEndpoints returns the control socket plus Endpoints, normalized and
// deduplicated, control socket first.
func (a *Adapter) KnownEndpoints() []string {
	cs := a.ControlSocket
	if cs == "" {
		var err error
		if cs, err = appserver.DefaultControlSocket(); err != nil {
			a.logger().Warn("codex: no home directory for control socket", "err", err)
		}
	}
	var out []string
	for _, ep := range append([]string{cs}, a.Endpoints...) {
		if ep == "" {
			continue
		}
		n, err := appserver.NormalizeEndpoint(ep)
		if err != nil {
			a.logger().Warn("codex: skipping endpoint", "endpoint", ep, "err", err)
			continue
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func (a *Adapter) connect(ctx context.Context, ep string) (*appserver.Client, error) {
	c, err := appserver.Dial(ctx, ep, appserver.Options{})
	if err != nil {
		return nil, err
	}
	if _, err := c.Initialize(ctx, a.clientInfo()); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

type host struct {
	endpoint string
	version  string
}

// hosts returns the endpoints whose thread/loaded/list includes threadID.
// thread/read is not consulted: it may return stored history from a server
// that does not host the live conversation (§3 CQ10).
func (a *Adapter) hosts(ctx context.Context, threadID string) []host {
	var hs []host
	for _, ep := range a.KnownEndpoints() {
		c, err := a.connect(ctx, ep)
		if err != nil {
			a.logger().Debug("codex: endpoint unavailable", "endpoint", ep, "err", err)
			continue
		}
		ids, err := c.LoadedThreads(ctx)
		v := c.ServerVersion()
		c.Close()
		if err != nil {
			a.logger().Debug("codex: thread/loaded/list failed", "endpoint", ep, "err", err)
			continue
		}
		if slices.Contains(ids, threadID) {
			hs = append(hs, host{endpoint: ep, version: v})
		}
	}
	return hs
}

// Register binds threadID to the one endpoint that has it loaded (§3 CQ10).
// No host → a Binding with empty HarnessEndpoint (sending works, inbound is
// unreachable). More than one → ErrAmbiguousEndpoint and no binding. A host
// whose version fails CheckVersion → ErrUntestedVersion (V11).
func (a *Adapter) Register(ctx context.Context, threadID string) (adapter.Binding, error) {
	if threadID == "" {
		return adapter.Binding{}, ErrNoThread
	}
	b := adapter.Binding{Harness: adapter.Codex, SessionRef: threadID}
	hs := a.hosts(ctx, threadID)
	switch len(hs) {
	case 0:
	case 1:
		warn, err := CheckVersion(hs[0].version)
		if err != nil {
			return adapter.Binding{}, fmt.Errorf("%w (endpoint %s)", err, hs[0].endpoint)
		}
		if warn {
			a.logger().Warn("codex: untested app-server minor version", "version", hs[0].version, "tested", TestedMinors, "endpoint", hs[0].endpoint)
		}
		b.HarnessEndpoint, b.HarnessVersion = hs[0].endpoint, hs[0].version
	default:
		eps := make([]string, len(hs))
		for i, h := range hs {
			eps[i] = h.endpoint
		}
		return adapter.Binding{}, fmt.Errorf("%w: thread %s loaded on %v", ErrAmbiguousEndpoint, threadID, eps)
	}
	a.mu.Lock()
	if a.bindings == nil {
		a.bindings = map[string]adapter.Binding{}
	}
	a.bindings[threadID] = b
	a.mu.Unlock()
	return b, nil
}

// Status reads the thread on its registered endpoint, or on the single
// loaded host when it was never registered here.
func (a *Adapter) Status(ctx context.Context, threadID string) (adapter.SessionStatus, error) {
	a.mu.Lock()
	b, ok := a.bindings[threadID]
	a.mu.Unlock()
	ep := b.HarnessEndpoint
	if !ok {
		hs := a.hosts(ctx, threadID)
		if len(hs) > 1 {
			return adapter.SessionStatus{}, fmt.Errorf("%w: thread %s", ErrAmbiguousEndpoint, threadID)
		}
		if len(hs) == 1 {
			ep = hs[0].endpoint
		}
	}
	if ep == "" {
		return adapter.SessionStatus{}, nil
	}
	c, err := a.connect(ctx, ep)
	if err != nil {
		return adapter.SessionStatus{}, nil
	}
	defer c.Close()
	th, err := c.ReadThread(ctx, threadID)
	if err != nil {
		return adapter.SessionStatus{}, err
	}
	return statusOf(th), nil
}

func statusOf(th appserver.Thread) adapter.SessionStatus {
	var s adapter.SessionStatus
	switch th.Status.Type {
	case appserver.ThreadIdle:
		s.Loaded = true
	case appserver.ThreadActive:
		s.Loaded, s.Busy = true, true
	case appserver.ThreadSystemError:
		s.Loaded = true
		return s
	default:
		return s
	}
	s.Reachable = th.CanAcceptDirectInput == nil || *th.CanAcceptDirectInput
	return s
}
