package codex

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

// BackendProbeTimeout bounds one backend probe; a timeout yields unknown.
var BackendProbeTimeout = 2 * time.Second

// BackendTTL is how long a probe result is reused for an endpoint.
var BackendTTL = 30 * time.Second

const openaiProvider = "openai"

type backendEntry struct {
	state string
	at    time.Time
}

// classifyBackend maps the error of account/rateLimits/read to a backend
// state. Only a connection-level failure from the app-server is unreachable.
func classifyBackend(err error) string {
	if err == nil {
		return harness.BackendReachable
	}
	var rpc *appserver.RPCError
	if errors.As(err, &rpc) && (strings.Contains(rpc.Message, "error sending request") || strings.Contains(rpc.Message, "Connection failed")) {
		return harness.BackendUnreachable
	}
	return harness.BackendUnknown
}

// probeBackend asks the server behind c whether it reaches its backend. The
// usage endpoint it tests belongs to ChatGPT auth, so any other account is
// unknown without a second call.
func probeBackend(ctx context.Context, c *appserver.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, BackendProbeTimeout)
	defer cancel()
	typ, err := c.AccountType(ctx)
	if err != nil || typ != "chatgpt" {
		return harness.BackendUnknown, err
	}
	err = c.RateLimits(ctx)
	return classifyBackend(err), err
}

type probeCall struct {
	done  chan struct{}
	state string
}

func (p *probeCall) wait(ctx context.Context) string {
	select {
	case <-p.done:
		return p.state
	case <-ctx.Done():
		return harness.BackendUnknown
	}
}

func doneProbe(state string) *probeCall {
	p := &probeCall{done: make(chan struct{}), state: state}
	close(p.done)
	return p
}

func (a *Adapter) cachedBackend(ep string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.backends[ep]
	if !ok || time.Since(e.at) >= BackendTTL {
		return "", false
	}
	return e.state, true
}

// probe returns ep's backend state as a cached result, the probe already in
// flight for ep, or a new probe on c. It takes ownership of c: a new probe
// closes c when it finishes, otherwise c is closed before probe returns. The
// probe runs detached from the caller's context, bounded by
// BackendProbeTimeout, so its result is cached for later callers.
func (a *Adapter) probe(ep string, c *appserver.Client) *probeCall {
	a.mu.Lock()
	if e, ok := a.backends[ep]; ok && time.Since(e.at) < BackendTTL {
		a.mu.Unlock()
		c.Close()
		return doneProbe(e.state)
	}
	if p := a.probes[ep]; p != nil {
		a.mu.Unlock()
		c.Close()
		return p
	}
	p := &probeCall{done: make(chan struct{})}
	if a.probes == nil {
		a.probes = map[string]*probeCall{}
	}
	a.probes[ep] = p
	a.mu.Unlock()
	go func() {
		state, _ := probeBackend(context.Background(), c)
		c.Close()
		a.mu.Lock()
		if a.backends == nil {
			a.backends = map[string]backendEntry{}
		}
		a.backends[ep] = backendEntry{state: state, at: time.Now()}
		delete(a.probes, ep)
		p.state = state
		a.mu.Unlock()
		close(p.done)
	}()
	return p
}

func (a *Adapter) endpointBackend(ctx context.Context, ep string) string {
	if v, ok := a.cachedBackend(ep); ok {
		return v
	}
	c, err := a.connect(ctx, ep)
	if err != nil {
		return harness.BackendUnknown
	}
	return a.probe(ep, c).wait(ctx)
}

// Backend is the backend state of the session sessionRef on the app-server
// at endpoint. The endpoint's state is cached; the thread's provider is read
// only when the endpoint is unreachable. Any failure is unknown.
func (a *Adapter) Backend(ctx context.Context, endpoint, sessionRef string) string {
	ep, err := appserver.NormalizeEndpoint(endpoint)
	if err != nil {
		return harness.BackendUnknown
	}
	cctx, cancel := context.WithTimeout(ctx, EndpointProbeTimeout)
	defer cancel()
	state := a.endpointBackend(cctx, ep)
	if state != harness.BackendUnreachable {
		return state
	}
	if sessionRef == "" {
		return harness.BackendUnknown
	}
	c, err := a.connect(cctx, ep)
	if err != nil {
		return harness.BackendUnknown
	}
	defer c.CloseContext(cctx)
	th, err := c.ReadThread(cctx, sessionRef)
	if err != nil || th.ModelProvider != openaiProvider {
		return harness.BackendUnknown
	}
	return state
}

type backendCheck struct {
	endpoint, state string
	err             error
}

// checkBackends probes every known endpoint that accepts a connection,
// bypassing the cache.
func (a *Adapter) checkBackends(ctx context.Context) []backendCheck {
	var out []backendCheck
	for _, ep := range a.KnownEndpoints() {
		cctx, cancel := context.WithTimeout(ctx, EndpointProbeTimeout)
		c, err := a.connect(cctx, ep)
		if err != nil {
			cancel()
			continue
		}
		state, perr := probeBackend(cctx, c)
		c.CloseContext(cctx)
		cancel()
		out = append(out, backendCheck{endpoint: ep, state: state, err: perr})
	}
	return out
}

var _ harness.BackendChecker = (*Harness)(nil)

// Backend implements harness.BackendChecker for adapters that can tell.
func (h *Harness) Backend(ctx context.Context, endpoint, sessionRef string) string {
	if b, ok := h.Threads.(harness.BackendChecker); ok {
		return b.Backend(ctx, endpoint, sessionRef)
	}
	return harness.BackendUnknown
}
