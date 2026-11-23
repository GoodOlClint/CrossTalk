package main

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

// While a follow-up attempt runs, a refresh tick leaves stdin and ingress
// alone and returns at once; after the attempt it quiesces as before.
func TestRefreshSkipsQuiesceWhileFollowUpAttempt(t *testing.T) {
	_, env, hs := fuSandbox(t, "", "claude")
	leave, ok := followUpRunner(env, hs, fuCfgDir(env), nil).Enter(context.Background())
	if !ok {
		t.Fatal("follow-up attempt not counted")
	}
	conn, w := pipeConn(t)
	ing := &noIngress{}
	start := time.Now()
	undo, err := quiesceWithin(conn, ing, 3*time.Second)
	if err == nil {
		undo()
	}
	if err == nil || ing.paused != 0 || time.Since(start) > time.Second {
		t.Fatalf("err %v, ingress paused %d, took %v: want an immediate refusal with nothing paused", err, ing.paused, time.Since(start))
	}
	w.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := conn.Read(rctx); err != nil {
		t.Fatalf("stdin reader left paused by the skipped tick: %v", err)
	}
	leave()
	undo, err = quiesceWithin(conn, ing, 5*time.Second)
	if err != nil {
		t.Fatalf("not quiescent after the attempt ended: %v", err)
	}
	undo()
}

// runMCP stops the follow-up when the server ends: a running plugin command
// is cancelled before serve returns.
func TestServeStopsFollowUpBeforeReturning(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "claude")
	inStep := make(chan struct{})
	var finished atomic.Bool
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "plugin marketplace add GoodOlClint/CrossTalk" {
			close(inStep)
			<-ctx.Done()
			finished.Store(true)
			return nil, ctx.Err()
		}
		return rec.run(ctx, name, args...)
	}
	stopFollowUp, ok := startFollowUp(context.Background(), true, fuBound, env, hs, fuCfgDir(env), nil)
	if !ok {
		t.Fatal("not started")
	}
	select {
	case <-inStep:
	case <-time.After(5 * time.Second):
		t.Fatal("step never ran")
	}
	tr := mcpserver.NewTransport(strings.NewReader(""), io.Discard)
	serve(context.Background(), mcpserver.New(mcpserver.Options{}), tr, nil, stopFollowUp)
	if !finished.Load() {
		t.Error("serve returned with the follow-up attempt still running")
	}
}
