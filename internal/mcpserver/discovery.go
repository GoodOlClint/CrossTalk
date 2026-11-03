package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
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
}

// listAgents serves unbound callers too; a bound caller is registered first
// so it appears in its own listing, and a binding failure becomes a notice.
func (s *Server) listAgentsTool(ctx context.Context, req *mcp.CallToolRequest, _ listIn) (*mcp.CallToolResult, any, error) {
	var notice string
	if c, err := s.resolve(ctx, req); err == nil && c.bound() {
		if _, n, e := s.agent(ctx, c); e != nil {
			notice = "not registered: " + e.Error()
		} else {
			notice = n
		}
	}
	rows, warnings := s.listAgents(ctx)
	out := map[string]any{"agents": rows}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	if notice != "" {
		out["notice"] = notice
	}
	return nil, out, nil
}

func (s *Server) listAgents(ctx context.Context) ([]Row, []string) {
	rows := map[string]*Row{}
	var warnings []string

	paths, _ := filepath.Glob(filepath.Join(s.sessionDir, "*.json"))
	for _, p := range paths {
		cs, err := readClaudeSession(p)
		if err != nil || cs.SessionID == "" || !s.alive(cs.PID) {
			continue
		}
		id := identity.AgentID(string(adapter.Claude), cs.SessionID)
		reachable := false
		if cs.MessagingSocketPath != "" {
			if fi, err := os.Stat(cs.MessagingSocketPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
				reachable = true
			}
		}
		rows[id] = &Row{Name: cs.Name, Harness: string(adapter.Claude), Cwd: cs.Cwd, Status: cs.Status, Reachable: reachable, AgentID: id}
	}

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
