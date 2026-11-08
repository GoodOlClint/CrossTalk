package remote

import (
	"context"
	"net/http"
)

const NativeNote = "no CrossTalk in this session; it cannot be messaged from another host"

// AgentRow is one row of GET /v1/agents: an allowlist, never a copy of the local row.
type AgentRow struct {
	AgentID     string `json:"agent_id"`
	Name        string `json:"name"`
	Harness     string `json:"harness"`
	CrossTalk   bool   `json:"crosstalk"`
	Role        string `json:"role,omitempty"`
	Reachable   bool   `json:"reachable"`
	Status      string `json:"status"`
	Messageable bool   `json:"messageable"`
	Note        string `json:"note,omitempty"`
}

// AgentsSource lists this host's sessions for the gateway.
type AgentsSource func(ctx context.Context) []AgentRow

func NewMux(forward http.Handler, agents AgentsSource) http.Handler { return forward }
