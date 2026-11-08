package remote

import "net/http"

const (
	RemoteAgentIDHeader = "X-CrossTalk-Remote-Agent-ID"
	RemoteNameHeader    = "X-CrossTalk-Remote-Name"
	RemoteHarnessHeader = "X-CrossTalk-Remote-Harness"
	RemoteRoleHeader    = "X-CrossTalk-Remote-Role"
)

// Forwarder is the gateway Handler for /v1/a2a/{agent}.
type Forwarder struct{}

// NewForwarder returns a Forwarder that reads the gateway token from stateDir
// and resolves agent_ids to loopback A2A URLs with agents.
func NewForwarder(stateDir string, agents func(agentID string) (a2aURL string, ok bool)) *Forwarder {
	return &Forwarder{}
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
