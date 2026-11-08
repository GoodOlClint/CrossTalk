package remote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"unicode"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const (
	NativeNote   = "no CrossTalk in this session; it cannot be messaged from another host"
	agentsPath   = "/v1/agents"
	maxStatusLen = 32
)

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

// NewMux routes /v1/a2a/ to forward and GET /v1/agents to the listing; any
// other path is 404.
func NewMux(forward http.Handler, agents AgentsSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case strings.HasPrefix(p, forwardPrefix) && forward != nil:
			forward.ServeHTTP(w, r)
		case p == agentsPath:
			serveAgents(w, r, agents)
		default:
			http.NotFound(w, r)
		}
	})
}

func serveAgents(w http.ResponseWriter, r *http.Request, src AgentsSource) {
	if _, ok := PeerFrom(r.Context()); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rows := []AgentRow{}
	if src != nil {
		rows = append(rows, src(r.Context())...)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Agents []AgentRow `json:"agents"`
	}{rows})
}

// cleanRow validates a row from a peer's listing, which is untrusted text.
// Identity fields go through the same check the gateway applies to a
// sender's attested fields.
func cleanRow(r AgentRow) (AgentRow, bool) {
	role := r.Role
	if !r.CrossTalk {
		role = registry.RolePrimary
	}
	if _, ok := a2a.ValidateAttested(r.AgentID, base64.RawURLEncoding.EncodeToString([]byte(r.Name)), r.Harness, role); !ok {
		return AgentRow{}, false
	}
	if !r.CrossTalk {
		r.Role, r.Messageable, r.Note = "", false, NativeNote
	} else {
		r.Note = ""
	}
	r.Status = cleanStatus(r.Status)
	return r, true
}

func cleanStatus(s string) string {
	s = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || c == unicode.ReplacementChar {
			return -1
		}
		return c
	}, s)
	if len(s) > maxStatusLen {
		s = strings.ToValidUTF8(s[:maxStatusLen], "")
	}
	return s
}
