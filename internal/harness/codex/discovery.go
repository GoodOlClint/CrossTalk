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
	dup      bool
}

// LoadedThreads lists every loaded thread on the known endpoints plus extra
// (§3 Discovery). An endpoint that cannot be reached contributes nothing.
// A thread loaded on more than one endpoint is listed once, unreachable
// (§3 CQ10). Backend probes run concurrently and never delay thread
// discovery; a probe unfinished when ctx ends leaves Backend unknown.
func (a *Adapter) LoadedThreads(ctx context.Context, extra ...string) []LoadedThread {
	var out []LoadedThread
	idx := map[string]int{}
	type span struct {
		first, end int
		p          *probeCall
	}
	var spans []span
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
		first := len(out)
		for _, id := range ids {
			if i, ok := idx[id]; ok {
				out[i].Session.Reachable = false
				out[i].Endpoint = ""
				out[i].dup = true
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
		spans = append(spans, span{first, len(out), a.probe(ep, c)})
	}
	for _, s := range spans {
		state := s.p.wait(ctx)
		for i := s.first; i < s.end; i++ {
			out[i].Backend = harness.BackendUnknown
			if !out[i].dup && out[i].provider == openaiProvider {
				out[i].Backend = state
			}
		}
	}
	return out
}
