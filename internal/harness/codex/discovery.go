package codex

import (
	"context"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
)

// LoadedThread is one thread an endpoint reports in thread/loaded/list.
type LoadedThread struct {
	Endpoint string
	ID       string
	Name     string
	Cwd      string
	Status   string
	Session  adapter.SessionStatus
	// Backend is the endpoint's backend state for a thread on the openai
	// provider, else unknown.
	Backend  string
	provider string
}

// LoadedThreads lists every loaded thread on the known endpoints plus extra
// (§3 Discovery). An endpoint that cannot be reached contributes nothing.
// A thread loaded on more than one endpoint is listed once, unreachable
// (§3 CQ10).
func (a *Adapter) LoadedThreads(ctx context.Context, extra ...string) []LoadedThread {
	var out []LoadedThread
	idx := map[string]int{}
	for _, ep := range a.KnownEndpoints(extra...) {
		c, err := a.connect(ctx, ep)
		if err != nil {
			a.logger().Debug("codex: endpoint unavailable", "endpoint", ep, "err", err)
			continue
		}
		ids, err := c.LoadedThreads(ctx)
		if err != nil {
			c.Close()
			continue
		}
		be := make(chan string, 1)
		go func() { be <- a.backendOn(ctx, ep, c) }()
		first := len(out)
		for _, id := range ids {
			if i, ok := idx[id]; ok {
				out[i].Session.Reachable = false
				out[i].Endpoint = ""
				out[i].Backend = harness.BackendUnknown
				continue
			}
			lt := LoadedThread{Endpoint: ep, ID: id}
			if th, err := c.ReadThread(ctx, id); err == nil {
				lt.Cwd, lt.Status, lt.Session, lt.provider = th.Cwd, th.Status.Type, statusOf(th), th.ModelProvider
				if th.Name != nil {
					lt.Name = *th.Name
				}
			}
			idx[id] = len(out)
			out = append(out, lt)
		}
		state := <-be
		for i := first; i < len(out); i++ {
			out[i].Backend = harness.BackendUnknown
			if out[i].provider == openaiProvider {
				out[i].Backend = state
			}
		}
		c.Close()
	}
	return out
}
