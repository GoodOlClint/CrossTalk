package codex

import (
	"context"
	"slices"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

// LoadedThread is one thread an endpoint reports in thread/loaded/list.
type LoadedThread struct {
	Endpoint string
	ID       string
	Name     string
	Cwd      string
	Status   string
	Session  adapter.SessionStatus
}

// LoadedThreads lists every loaded thread on the known endpoints plus extra
// (§3 Discovery). An endpoint that cannot be reached contributes nothing.
// A thread loaded on more than one endpoint is listed once, unreachable
// (§3 CQ10).
func (a *Adapter) LoadedThreads(ctx context.Context, extra ...string) []LoadedThread {
	eps := a.KnownEndpoints()
	for _, ep := range extra {
		if n, err := appserver.NormalizeEndpoint(ep); err == nil && !slices.Contains(eps, n) {
			eps = append(eps, n)
		}
	}
	var out []LoadedThread
	idx := map[string]int{}
	for _, ep := range eps {
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
		for _, id := range ids {
			if i, ok := idx[id]; ok {
				out[i].Session.Reachable = false
				out[i].Endpoint = ""
				continue
			}
			lt := LoadedThread{Endpoint: ep, ID: id}
			if th, err := c.ReadThread(ctx, id); err == nil {
				lt.Cwd, lt.Status, lt.Session = th.Cwd, th.Status.Type, statusOf(th)
				if th.Name != nil {
					lt.Name = *th.Name
				}
			}
			idx[id] = len(out)
			out = append(out, lt)
		}
		c.Close()
	}
	return out
}
