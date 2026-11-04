package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
)

// caller is the session a single tools/call came from (§3).
type caller struct {
	harness    adapter.Harness
	sessionRef string
	name       string
	cwd        string
	harnessPID int
	req        harness.RequestContext
}

func (c caller) bound() bool { return c.harness != "" }

func requestContext(req *mcp.CallToolRequest, ppid int) harness.RequestContext {
	rc := harness.RequestContext{ParentPID: ppid}
	if req == nil {
		return rc
	}
	if req.Params != nil {
		rc.Meta = req.Params.GetMeta()
	}
	if req.Session != nil {
		rc.MCPSessionID = req.Session.ID()
		if ip := req.Session.InitializeParams(); ip != nil && ip.ClientInfo != nil {
			rc.Client = harness.ClientInfo{Name: ip.ClientInfo.Name, Version: ip.ClientInfo.Version}
		}
	}
	return rc
}

// resolveCaller asks the harnesses in trust order; the first that claims the
// call names the caller, else it is unbound.
func resolveCaller(ctx context.Context, hs []harness.Harness, rc harness.RequestContext) (caller, error) {
	s, ok, err := harness.Identify(ctx, hs, rc)
	if err != nil || !ok {
		return caller{}, err
	}
	return caller{harness: s.Harness, sessionRef: s.SessionRef, name: s.Name, cwd: s.Cwd, harnessPID: s.HarnessPID, req: rc}, nil
}
