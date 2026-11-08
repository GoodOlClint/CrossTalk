package shim

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

// RemoteFetchBudget is the one deadline all paired hosts share.
const RemoteFetchBudget = 1500 * time.Millisecond

const remoteFetchGrace = 30 * time.Millisecond

const cannotReceive = "this host's gateway is not running; it cannot receive from other hosts"

type fetched struct {
	rows []remote.AgentRow
	err  error
	done bool
}

func (s *Shim) remoteDir() (string, error) {
	if s.opt.RemoteConfigDir != "" {
		return s.opt.RemoteConfigDir, nil
	}
	return remote.ConfigDir()
}

// ListRemote asks every paired host for its listing, in parallel and under
// one shared deadline, and returns what arrived. Nothing is cached.
func (s *Shim) ListRemote(ctx context.Context) mcpserver.RemoteListing {
	dir, err := s.remoteDir()
	if err != nil {
		return mcpserver.RemoteListing{}
	}
	peers, err := remote.NewPeers(dir).List()
	if err != nil {
		return mcpserver.RemoteListing{Warnings: []string{"remote: paired hosts not listed: " + err.Error()}}
	}
	if len(peers) == 0 {
		return mcpserver.RemoteListing{}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Alias < peers[j].Alias })
	host, hostErr := remote.LoadHost(dir)

	ctx, cancel := context.WithTimeout(ctx, RemoteFetchBudget)
	defer cancel()
	var mu sync.Mutex
	res := make([]fetched, len(peers))
	var wg sync.WaitGroup
	if hostErr == nil {
		for i, p := range peers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rows, err := remote.NewClient(host, p).Agents(ctx)
				mu.Lock()
				res[i] = fetched{rows, err, true}
				mu.Unlock()
			}()
		}
		all := make(chan struct{})
		go func() { wg.Wait(); close(all) }()
		select {
		case <-all:
		case <-ctx.Done():
			select {
			case <-all:
			case <-time.After(remoteFetchGrace):
			}
		}
	}

	warn := ""
	if up, err := remote.GatewayRunning(s.opt.StateDir); err == nil && !up {
		warn = cannotReceive
	}
	mu.Lock()
	defer mu.Unlock()
	var out mcpserver.RemoteListing
	for i, p := range peers {
		e := mcpserver.HostEntry{Host: p.Alias, Warning: warn}
		switch {
		case hostErr != nil:
			e.Error = remote.ReasonUnreachable
		case !res[i].done:
			e.Error = remote.ReasonTimeout
		case res[i].err != nil:
			e.Error = remote.Classify(res[i].err)
			var re *remote.Error
			if errors.As(res[i].err, &re) {
				e.Error = re.Reason
			}
			e.Hint = stallHint(res[i].err)
		default:
			e.Reachable, e.Agents = true, len(res[i].rows)
			for _, r := range res[i].rows {
				out.Rows = append(out.Rows, mcpserver.RemoteRow{
					Name: r.Name + "@" + p.Alias, Host: p.Alias, Harness: r.Harness, AgentID: r.AgentID,
					CrossTalk: r.CrossTalk, Messageable: r.Messageable, Reachable: r.Reachable, Status: r.Status, Role: r.Role, Note: r.Note,
				})
			}
		}
		out.Hosts = append(out.Hosts, e)
	}
	return out
}

// RemoteSource is the gateway's view of this host's sessions: every row the
// local list_agents shows, cut to the allowlist in remote.AgentRow. A
// registered agent can be messaged; a native session cannot, from here.
func RemoteSource(srv *mcpserver.Server) remote.AgentsSource {
	return func(ctx context.Context) []remote.AgentRow {
		rows, _ := srv.Listing(ctx)
		out := make([]remote.AgentRow, 0, len(rows))
		for _, r := range rows {
			name := r.Name
			if name == "" {
				name = unnamedFallback(r.AgentID)
			}
			a := remote.AgentRow{AgentID: r.AgentID, Name: name, Harness: r.Harness, CrossTalk: r.CrossTalk, Reachable: r.Reachable, Status: r.Status, Messageable: r.CrossTalk}
			if r.CrossTalk {
				a.Role = r.Role
			} else {
				a.Note = remote.NativeNote
			}
			out = append(out, a)
		}
		return out
	}
}

func unnamedFallback(agentID string) string {
	if len(agentID) > 6 {
		agentID = agentID[:6]
	}
	return "unnamed-" + agentID
}
