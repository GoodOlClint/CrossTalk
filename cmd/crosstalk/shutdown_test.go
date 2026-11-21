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

type probeWriter struct{ probe func() }

func (w probeWriter) Write(p []byte) (int, error) { w.probe(); return len(p), nil }

func TestFinishKeepsTimedOutAgentLockedUntilReported(t *testing.T) {
	stateDir := t.TempDir()
	id := identity.AgentID("codex", "T-1")
	acquire := func() error {
		for range 10 {
			runtime.GC()
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
		_, err := registry.New(stateDir).Acquire(registry.Request{AgentID: id, HarnessName: "other", Harness: "codex", Cwd: "/w"})
		return err
	}
	var duringClose, duringReport error
	closeShim := func() error { duringClose = acquire(); return nil }
	stderr := probeWriter{func() { duringReport = acquire() }}
	if rc := finish(timedOutServer(t, stateDir), mcpserver.ErrCloseTimeout, func() {}, closeShim, stderr); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	if !errors.Is(duringClose, registry.ErrAgentHeld) {
		t.Fatalf("Acquire during shim close = %v, want ErrAgentHeld", duringClose)
	}
	if !errors.Is(duringReport, registry.ErrAgentHeld) {
		t.Fatalf("Acquire while reporting = %v, want ErrAgentHeld", duringReport)
	}
}
