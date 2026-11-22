package main

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A client at protocol 2026-07-28 (SEP-2575) that wants tools/list_changed,
// as Claude Code's TUI does, keeps a subscriptions/listen request open for
// the life of the session; the server answers it only when the subscription
// ends.
func TestRefreshExecsWithOpenListenStream(t *testing.T) {
	bin, home := firstImage(t)
	cmd := exec.Command(bin, "mcp")
	cmd.Env = shimEnv(home)
	cmd.Stderr = &testWriter{t}
	changed := make(chan struct{}, 8)
	c := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "2.1.330"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(_ context.Context, r *mcp.ToolListChangedRequest) {
			if r.Params == nil || r.Params.Meta["io.modelcontextprotocol/subscriptionId"] == nil {
				t.Errorf("tools/list_changed without the listen's subscription id: %+v", r.Params)
			}
			changed <- struct{}{}
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cs, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}}); err != nil || r.IsError {
		t.Fatalf("list_agents before refresh: %v %+v", err, r)
	}
	p := &mcpProc{t: t, home: home}
	before := p.descriptorWhere("for the first image", func(d descriptor) bool { return d.A2AURL != "" })
	replaceByRename(t, bin, "9.9.2")
	select {
	case <-changed:
	case <-time.After(20 * time.Second):
		t.Fatal("no tools/list_changed on the listen stream within 20s: the shim did not refresh")
	}
	after := p.descriptorWhere("rewritten by the new image", func(d descriptor) bool { return d.A2AURL != "" && d.A2AURL != before.A2AURL })
	if v := cardVersion(t, after.A2AURL); v != "9.9.2" {
		t.Fatalf("after refresh card version = %q, want 9.9.2", v)
	}
	if r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}}); err != nil || r.IsError {
		t.Fatalf("list_agents after refresh: %v %+v", err, r)
	}
	replaceByRename(t, bin, "9.9.3")
	select {
	case <-changed:
	case <-time.After(20 * time.Second):
		t.Fatal("no second tools/list_changed: the listen was not carried into the next handoff")
	}
	again := p.descriptorWhere("rewritten by the third image", func(d descriptor) bool { return d.A2AURL != "" && d.A2AURL != after.A2AURL })
	if v := cardVersion(t, again.A2AURL); v != "9.9.3" {
		t.Fatalf("after second refresh card version = %q, want 9.9.3", v)
	}
}
