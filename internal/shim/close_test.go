package shim

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

type stuckIngress struct {
	release chan struct{}
	files   []string
}

func (stuckIngress) Listen() error { return nil }
func (i stuckIngress) Serve(ctx context.Context) error {
	<-ctx.Done()
	<-i.release
	return nil
}
func (i stuckIngress) Close() error    { <-i.release; return nil }
func (i stuckIngress) Files() []string { return i.files }

func TestCloseStateBoundedWhenHandlerNeverReturns(t *testing.T) {
	old := HandlerCloseWait
	HandlerCloseWait = 200 * time.Millisecond
	t.Cleanup(func() { HandlerCloseWait = old })

	ing := stuckIngress{release: make(chan struct{})}
	t.Cleanup(func() { close(ing.release) })
	s := &Shim{log: slog.Default()}
	st := &AgentState{shim: s, uds: ing, serve: make(chan struct{})}
	st.ctx, st.cancel = context.WithCancel(context.Background())
	go func() {
		defer close(st.serve)
		ing.Serve(st.ctx)
	}()

	start := time.Now()
	s.closeState(st)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("closeState took %v past a %v wait", el, HandlerCloseWait)
	}
	if st.ctx.Err() == nil {
		t.Fatal("handler context not cancelled")
	}
}

func TestCloseStateSharedBudgetCoversA2AAndKeepsOwnership(t *testing.T) {
	old := HandlerCloseWait
	HandlerCloseWait = 30 * time.Millisecond
	t.Cleanup(func() { HandlerCloseWait = old })

	sock := filepath.Join(t.TempDir(), "agent.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ing := stuckIngress{release: make(chan struct{}), files: []string{sock}}
	release := make(chan struct{})
	t.Cleanup(func() { close(release); close(ing.release) })
	s := &Shim{log: slog.Default()}
	st := &AgentState{shim: s, uds: ing, serve: make(chan struct{})}
	st.ctx, st.cancel = context.WithCancel(context.Background())
	st.a2a.Close = func(context.Context) error { <-release; return nil }
	go func() {
		defer close(st.serve)
		ing.Serve(st.ctx)
	}()

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.closeState(st) }()
	select {
	case err := <-done:
		if el := time.Since(start); el > 5*HandlerCloseWait {
			t.Fatalf("closeState took %v with a %v shared budget", el, HandlerCloseWait)
		}
		if !errors.Is(err, mcpserver.ErrCloseTimeout) {
			t.Fatalf("err = %v, want ErrCloseTimeout", err)
		}
	case <-time.After(5 * HandlerCloseWait):
		t.Fatalf("closeState still blocked after 5x the %v shared budget", HandlerCloseWait)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket released while a handler may still run: %v", err)
	}
	if err := s.closeState(st); !errors.Is(err, mcpserver.ErrCloseTimeout) {
		t.Fatalf("repeat closeState err = %v, want the first close's ErrCloseTimeout", err)
	}
}
