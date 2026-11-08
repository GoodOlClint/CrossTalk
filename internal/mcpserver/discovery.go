package mcpserver

import (
	"context"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/identity"
)

// Row is one list_agents entry (§3 Discovery).
type Row struct {
	Name      string  `json:"name"`
	Harness   string  `json:"harness"`
	Cwd       string  `json:"cwd"`
	Status    string  `json:"status"`
	CrossTalk bool    `json:"crosstalk"`
	Reachable bool    `json:"reachable"`
	AgentID   string  `json:"agent_id"`
	Role      string  `json:"role,omitempty"`
	Owner     *string `json:"owner"`
	You       bool    `json:"you,omitempty"`
}

// RemoteRow is a paired host's agent as list_agents shows it. It has no cwd.
type RemoteRow struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	Harness     string `json:"harness"`
	AgentID     string `json:"agent_id"`
	CrossTalk   bool   `json:"crosstalk"`
	Messageable bool   `json:"messageable"`
	Reachable   bool   `json:"reachable"`
	Status      string `json:"status"`
	Role        string `json:"role,omitempty"`
	Note        string `json:"note,omitempty"`
}

// HostEntry is one paired host in the `hosts` output.
type HostEntry struct {
	Host      string `json:"host"`
	Reachable bool   `json:"reachable"`
	Agents    int    `json:"agents"`
	Error     string `json:"error,omitempty"`
	Hint      string `json:"hint,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

type RemoteListing struct {
	Rows  []RemoteRow
	Hosts []HostEntry
	// Warnings are local configuration problems, shown in the output's warnings.
	Warnings []string
}

// RemoteLister is optionally implemented by a Backend that can list paired hosts.
type RemoteLister interface {
	ListRemote(ctx context.Context) RemoteListing
}

// listAgents serves unbound callers too; a bound caller is registered first
// so it appears in its own listing, and a binding failure becomes a notice.
func (s *Server) listAgentsTool(ctx context.Context, req *mcp.CallToolRequest, _ listIn) (*mcp.CallToolResult, any, error) {
	var notice, self string
	if c, err := s.resolve(ctx, req); err == nil && c.bound() {
		if a, n, e := s.agent(ctx, c); e != nil {
			notice = "not registered: " + describe(e)
		} else {
			a.Done()
			notice, self = n, a.ID()
		}
	}
	var remote chan RemoteListing
	if rl, ok := s.backend.(RemoteLister); ok {
		remote = make(chan RemoteListing, 1)
		go func() { remote <- rl.ListRemote(ctx) }()
	}
	rows, warnings := s.listAgents(ctx)
	agents := make([]any, 0, len(rows))
	for i := range rows {
		rows[i].You = self != "" && rows[i].AgentID == self
		agents = append(agents, rows[i])
	}
	out := map[string]any{"agents": agents}
	if remote != nil {
		l := <-remote
		for _, r := range l.Rows {
			agents = append(agents, r)
		}
		out["agents"] = agents
		warnings = append(warnings, l.Warnings...)
		if len(l.Hosts) > 0 {
			out["hosts"] = l.Hosts
		}
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	if notice != "" {
		out["notice"] = notice
	}
	return nil, out, nil
}

// Listing is the local listing, as the gateway serves it to paired hosts.
func (s *Server) Listing(ctx context.Context) ([]Row, []string) { return s.listAgents(ctx) }

func (s *Server) listAgents(ctx context.Context) ([]Row, []string) {
	rows := map[string]*Row{}
	var warnings []string

	sessions, err := s.backend.Discover(ctx)
	if err != nil {
		warnings = append(warnings, "discover: "+err.Error())
	}
	for _, ss := range sessions {
		id := identity.AgentID(string(ss.Harness), ss.SessionRef)
		rows[id] = &Row{Name: ss.Name, Harness: string(ss.Harness), Cwd: ss.Cwd, Status: ss.Status, Reachable: ss.Reachable, AgentID: id}
	}

	ds, err := s.reg.List()
	if err != nil {
		warnings = append(warnings, "registry: "+err.Error())
	}
	for _, d := range ds {
		r := rows[d.AgentID]
		if r == nil {
			r = &Row{Harness: d.Harness, Cwd: d.Cwd, AgentID: d.AgentID}
			rows[d.AgentID] = r
		}
		r.Name, r.CrossTalk, r.Role, r.Owner = d.Name, true, d.Role, d.Owner
		if r.Cwd == "" {
			r.Cwd = d.Cwd
		}
	}

	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].AgentID < out[j].AgentID
	})
	return out, warnings
}
