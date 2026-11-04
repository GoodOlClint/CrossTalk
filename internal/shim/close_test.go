package shim

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

type stuckIngress struct{ release chan struct{} }

func (stuckIngress) Listen() error { return nil }
func (i stuckIngress) Serve(ctx context.Context) error {
	<-ctx.Done()
	<-i.release
	return nil
}
func (i stuckIngress) Close() error  { <-i.release; return nil }
func (stuckIngress) Files() []string { return nil }

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
