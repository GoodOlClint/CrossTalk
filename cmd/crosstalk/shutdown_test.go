package main

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type timedOutCloser struct{ *mcpserver.FakeBackend }

func (timedOutCloser) CloseAgent(*mcpserver.Agent) error { return mcpserver.ErrCloseTimeout }

func timedOutServer(t *testing.T, stateDir string) *mcpserver.Server {
	t.Helper()
	sessions, err := os.MkdirTemp("", "ctsess")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sessions) })
	ch, err := claude.NewHarness(claude.HarnessOptions{SessionsDir: sessions, Alive: func(int) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	fb := mcpserver.NewFakeBackend()
	srv := mcpserver.New(mcpserver.Options{
		Backend:   timedOutCloser{fb},
		Registry:  registry.New(stateDir),
		Harnesses: []harness.Harness{codex.NewHarness(codex.HarnessOptions{}), ch},
		PPID:      999999,
	})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Meta:      map[string]any{"threadId": "T-1"},
		Name:      "send_message",
		Arguments: map[string]any{"to": "x", "text": "1"},
	})
	if err != nil || res.IsError {
		t.Fatalf("register: %v %+v", err, res)
	}
	cs.Close()
	if err := srv.Close(); !errors.Is(err, mcpserver.ErrCloseTimeout) {
		t.Fatalf("Close err = %v, want ErrCloseTimeout", err)
	}
	return srv
}

func TestShutdownKeepsTimedOutAgentLockedThroughShimClose(t *testing.T) {
	stateDir := t.TempDir()
	id := identity.AgentID("codex", "T-1")
	var acquireErr error
	closeShim := func() error {
		for range 10 {
			runtime.GC()
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
		_, acquireErr = registry.New(stateDir).Acquire(registry.Request{AgentID: id, HarnessName: "other", Harness: "codex", Cwd: "/w"})
		return nil
	}
	if err := shutdown(timedOutServer(t, stateDir), func() {}, closeShim); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(acquireErr, registry.ErrAgentHeld) {
		t.Fatalf("Acquire during shim close = %v, want ErrAgentHeld", acquireErr)
	}
}
