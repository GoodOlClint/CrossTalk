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

func (a *Adapter) cachedBackend(ep string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.backends[ep]
	if !ok || time.Since(e.at) >= BackendTTL {
		return "", false
	}
	return e.state, true
}

// backendOn serves ep's backend state from the cache or probes it on c;
// probes are serialized so one endpoint is probed once per TTL.
func (a *Adapter) backendOn(ctx context.Context, ep string, c *appserver.Client) string {
	if v, ok := a.cachedBackend(ep); ok {
		return v
	}
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if v, ok := a.cachedBackend(ep); ok {
		return v
	}
	v, _ := probeBackend(ctx, c)
	if ctx.Err() == nil {
		a.mu.Lock()
		if a.backends == nil {
			a.backends = map[string]backendEntry{}
		}
		a.backends[ep] = backendEntry{state: v, at: time.Now()}
		a.mu.Unlock()
	}
	return v
}

// Backend is the cached backend state of the app-server at endpoint,
// connecting only on a miss. An endpoint that cannot be dialed is unknown.
func (a *Adapter) Backend(ctx context.Context, endpoint string) string {
	ep, err := appserver.NormalizeEndpoint(endpoint)
	if err != nil {
		return harness.BackendUnknown
	}
	if v, ok := a.cachedBackend(ep); ok {
		return v
	}
	cctx, cancel := context.WithTimeout(ctx, EndpointProbeTimeout)
	defer cancel()
	c, err := a.connect(cctx, ep)
	if err != nil {
		return harness.BackendUnknown
	}
	defer c.CloseContext(cctx)
	return a.backendOn(cctx, ep, c)
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
func (h *Harness) Backend(ctx context.Context, endpoint string) string {
	if b, ok := h.Threads.(harness.BackendChecker); ok {
		return b.Backend(ctx, endpoint)
	}
	return harness.BackendUnknown
}
