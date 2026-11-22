package shim

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/quiesce"
)

// An ask's deadline notice blocked in its inject keeps a refresh waiting.
func TestDeadlineExpiryHoldsQuiesce(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.node(alice.pid, w.lb, nil).ok(nil, "receive_message", map[string]any{})
	nb := w.node(1, w.lb, nil)
	entered, gate := make(chan struct{}, 1), make(chan struct{})
	w.codex.ScriptInject(adapter.Outcome{State: adapter.Accepted, Entered: entered, Gate: gate})
	nb.ok(thread("T-bob"), "ask", map[string]any{"to": "alice", "text": "now?", "deadline_seconds": 1})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no deadline notice within 5s")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err := quiesce.Wait(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("quiescent while the deadline notice is injecting: %v", err)
	}
	close(gate)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := quiesce.Wait(ctx); err != nil {
		t.Fatalf("busy after the notice: %v", err)
	}
}

// PauseIngress runs while calls bind new agents; the race detector checks
// the listener fields.
func TestPauseIngressDuringFirstBind(t *testing.T) {
	w := newWorld(t)
	for _, id := range []string{"T-a", "T-b", "T-c", "T-d"} {
		w.codex.addThread(id, id)
	}
	n := w.node(1, w.lb, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				n.sh.PauseIngress()()
			}
		}
	})
	for _, id := range []string{"T-a", "T-b", "T-c", "T-d"} {
		n.ok(thread(id), "receive_message", map[string]any{})
	}
	close(stop)
	wg.Wait()
}
