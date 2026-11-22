package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/quiesce"
)

// flickerIngress starts a short unit of work on every pass after the first,
// and each such pass takes pass to return, so work comes and goes while the
// quiesce loop runs.
type flickerIngress struct {
	pass            time.Duration
	paused, reopens int
}

func (f *flickerIngress) PauseIngress() func() {
	f.paused++
	if f.paused > 1 {
		quiesce.Inc()
		time.AfterFunc(f.pass/4, quiesce.Dec)
		time.Sleep(f.pass)
	}
	return func() { f.reopens++ }
}

// Quiesce gives up with the context error once the cap has passed, even when
// work flickers so that each wait sees its condition hold.
func TestQuiesceStopsAtCapUnderFlickeringWork(t *testing.T) {
	conn, _ := pipeConn(t)
	limit := 100 * time.Millisecond
	ing := &flickerIngress{pass: 2 * limit}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()
	undo, err := quiesceForExec(ctx, ing, conn)
	took := time.Since(start)
	if err == nil {
		undo()
		t.Fatalf("quiescent after %v, past the %v cap", took, limit)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("quiesce: %v, want the context error", err)
	}
	if took > limit+ing.pass+limit {
		t.Fatalf("returned after %v, cap %v", took, limit)
	}
	if ing.reopens != ing.paused {
		t.Fatalf("gave up with ingress paused %d, reopened %d", ing.paused, ing.reopens)
	}
}

type slowFirstIngress struct {
	pass            time.Duration
	paused, reopens int
}

func (f *slowFirstIngress) PauseIngress() func() {
	f.paused++
	if f.paused == 1 {
		time.Sleep(f.pass)
	}
	return func() { f.reopens++ }
}

// A first ingress pass that outlasts the cap gives up without a second pass.
func TestQuiesceStopsAfterSlowFirstPass(t *testing.T) {
	conn, _ := pipeConn(t)
	limit := 50 * time.Millisecond
	ing := &slowFirstIngress{pass: 2 * limit}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	undo, err := quiesceForExec(ctx, ing, conn)
	if err == nil {
		undo()
		t.Fatal("quiescent past the cap")
	}
	if !errors.Is(err, context.DeadlineExceeded) || ing.paused != 1 || ing.reopens != 1 {
		t.Fatalf("err %v, passes %d, reopens %d; want the context error after one pass", err, ing.paused, ing.reopens)
	}
}
