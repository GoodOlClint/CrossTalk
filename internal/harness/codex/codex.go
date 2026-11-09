// Package codex binds sessions and delivers peer data through the owning app-server.
package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// ReasonAmbiguousEndpoint is the §3 CQ10 reason for a thread loaded on more
// than one endpoint.
const ReasonAmbiguousEndpoint = "ambiguous_endpoint"

var (
	ErrAmbiguousEndpoint = errors.New("codex: " + ReasonAmbiguousEndpoint)
	ErrNoEndpoint        = fmt.Errorf("codex: %w", adapter.ErrNoEndpoint)
	ErrNoThread          = errors.New("codex: no thread id")
)

type Adapter struct {
	// ControlSocket is the managed daemon endpoint; "" means
	// appserver.DefaultControlSocket().
	ControlSocket string
	// Endpoints are further known endpoints (harness_endpoint values from
	// descriptors), queried alongside ControlSocket (§3 CQ10).
	Endpoints []string
	Logger    *slog.Logger
	// InjectTimeout bounds one Inject; 0 means DefaultInjectTimeout.
	InjectTimeout time.Duration
	ClientInfo    appserver.ClientInfo
	// RebindInterval is the least time between two endpoint lookups for one
	// thread from Inject; 0 means DefaultRebindInterval.
	RebindInterval time.Duration
	Directory      interface {
		List() ([]registry.Descriptor, error)
	}

	mu       sync.Mutex
	bindings map[string]adapter.Binding
	// rebinding holds one token per thread while Inject looks it up; it and
	// lookedUp are guarded by mu.
	rebinding map[string]chan struct{}
	lookedUp  map[string]time.Time
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

func (a *Adapter) KnownEndpoints(extra ...string) []string {
	cs := a.ControlSocket
	if cs == "" {
		var err error
		if cs, err = appserver.DefaultControlSocket(); err != nil {
			a.logger().Warn("codex: no home directory for control socket", "err", err)
		}
	}
	eps := append([]string{cs}, a.Endpoints...)
	if a.Directory != nil {
		if ds, err := a.Directory.List(); err == nil {
			for _, d := range ds {
				if d.Harness == string(adapter.Codex) && d.HarnessEndpoint != nil {
					eps = append(eps, *d.HarnessEndpoint)
				}
			}
		} else {
			a.logger().Warn("codex: endpoint directory unavailable", "err", err)
		}
	}
	var out []string
	for _, ep := range append(eps, extra...) {
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
		c.CloseContext(ctx)
		return nil, err
	}
	return c, nil
}

// EndpointProbeTimeout bounds the connect, initialize and thread listing of
// one endpoint during registration, so a server that accepts a connection
// and never answers cannot stall a session that lives elsewhere.
var EndpointProbeTimeout = 5 * time.Second

// DefaultRebindInterval bounds Inject's endpoint lookups to one per thread
// per interval.
const DefaultRebindInterval = 10 * time.Second

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
		ectx, cancel := context.WithTimeout(ctx, EndpointProbeTimeout)
		c, err := a.connect(ectx, ep)
		if err != nil {
			cancel()
			a.logger().Debug("codex: endpoint unavailable", "endpoint", ep, "err", err)
			continue
		}
		ids, err := c.LoadedThreads(ectx)
		v := c.ServerVersion()
		c.Close()
		cancel()
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

// rebind returns the endpoint now hosting threadID, other than failed, or ""
// (none, several, or a lookup for the thread ran within RebindInterval). A
// found endpoint replaces the adapter's cached binding; the caller's binding,
// the journal key and the descriptor keep the bind-time value.
func (a *Adapter) rebind(ctx context.Context, threadID, failed string) string {
	interval := a.RebindInterval
	if interval == 0 {
		interval = DefaultRebindInterval
	}
	a.mu.Lock()
	if ep := a.bindings[threadID].HarnessEndpoint; ep != "" && ep != failed {
		a.mu.Unlock()
		return ep
	}
	if a.rebinding == nil {
		a.rebinding = map[string]chan struct{}{}
	}
	tok := a.rebinding[threadID]
	if tok == nil {
		tok = make(chan struct{}, 1)
		a.rebinding[threadID] = tok
	}
	a.mu.Unlock()
	select {
	case tok <- struct{}{}:
		defer func() { <-tok }()
	case <-ctx.Done():
		return ""
	}
	a.mu.Lock()
	b, last := a.bindings[threadID], a.lookedUp[threadID]
	if b.HarnessEndpoint != "" && b.HarnessEndpoint != failed {
		a.mu.Unlock()
		return b.HarnessEndpoint
	}
	if time.Since(last) < interval {
		a.mu.Unlock()
		return ""
	}
	if a.lookedUp == nil {
		a.lookedUp = map[string]time.Time{}
	}
	a.lookedUp[threadID] = time.Now()
	a.mu.Unlock()
	hs := a.hosts(ctx, threadID)
	if len(hs) != 1 || hs[0].endpoint == failed {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bindings == nil {
		a.bindings = map[string]adapter.Binding{}
	}
	a.bindings[threadID] = adapter.Binding{Harness: adapter.Codex, SessionRef: threadID, HarnessEndpoint: hs[0].endpoint, HarnessVersion: hs[0].version}
	a.logger().Info("codex: thread rebound", "thread", threadID, "endpoint", hs[0].endpoint)
	return hs[0].endpoint
}

// current is the endpoint the adapter last bound threadID to, or "".
func (a *Adapter) current(threadID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bindings[threadID].HarnessEndpoint
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
	s.Reachable = th.CanAcceptDirectInput != nil && *th.CanAcceptDirectInput
	return s
}
