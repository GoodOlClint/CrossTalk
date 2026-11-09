package codex_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func unloadedThread() *appservertest.Thread {
	th := idleThread()
	th.Loaded = false
	return th
}

func load(s *appservertest.Server) {
	s.Mutate(thread, func(th *appservertest.Thread) { th.Loaded = true })
}

func bindUnhosted(t *testing.T, a *codex.Adapter) adapter.Delivery {
	t.Helper()
	b, err := a.Register(ctx(t), thread)
	if err != nil || b.HarnessEndpoint != "" {
		t.Fatalf("Register = %+v, %v; want a binding with no endpoint", b, err)
	}
	return adapter.Delivery{Mode: msg.Immediate, VerifiedSender: true, Target: b}
}

func TestInjectRebindsWhenThreadLoadsAfterBind(t *testing.T) {
	s := appservertest.New(t)
	s.AddThread(unloadedThread())
	a := &codex.Adapter{ControlSocket: s.Endpoint(), RebindInterval: time.Nanosecond}
	d := bindUnhosted(t, a)
	if st, err := a.Inject(ctx(t), message("early"), d); st != adapter.Unreachable || !errors.Is(err, adapter.ErrNoEndpoint) {
		t.Fatalf("before load: %s, %v", st, err)
	}
	load(s)
	if st, err := a.Inject(ctx(t), message("late"), d); st != adapter.Accepted || err != nil {
		t.Fatalf("after load: %s, %v", st, err)
	}
	if got := s.Thread(thread).ToolOutputs; len(got) != 1 {
		t.Fatalf("tool outputs = %+v", got)
	}
	if st, err := a.Status(ctx(t), thread); err != nil || !st.Reachable {
		t.Fatalf("Status after rebind = %+v, %v", st, err)
	}
}

func TestInjectNeverHostedStaysNoEndpointAndLookupIsBounded(t *testing.T) {
	s := appservertest.New(t)
	s.AddThread(unloadedThread())
	a := &codex.Adapter{ControlSocket: s.Endpoint()}
	d := bindUnhosted(t, a)
	for range 20 {
		if st, err := a.Inject(ctx(t), message("x"), d); st != adapter.Unreachable || !errors.Is(err, adapter.ErrNoEndpoint) {
			t.Fatalf("Inject = %s, %v", st, err)
		}
	}
	if n := len(s.CallsTo("thread/loaded/list")); n != 2 {
		t.Fatalf("thread/loaded/list calls = %d, want 2 (bind + one rebind per %s)", n, codex.DefaultRebindInterval)
	}
}

func TestInjectRebindConcurrentSendsShareOneLookup(t *testing.T) {
	s := appservertest.New(t)
	s.AddThread(unloadedThread())
	a := &codex.Adapter{ControlSocket: s.Endpoint(), RebindInterval: time.Nanosecond}
	d := bindUnhosted(t, a)
	load(s)
	before := len(s.CallsTo("thread/loaded/list"))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			m := message("x")
			m.Delivery = msg.NextTurn
			if st, err := a.Inject(ctx(t), m, adapter.Delivery{Mode: msg.NextTurn, VerifiedSender: true, Target: d.Target}); st != adapter.Queued {
				errs <- errors.Join(errors.New(string(st)), err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := len(s.CallsTo("thread/loaded/list")) - before; n != 1 {
		t.Fatalf("lookups during concurrent sends = %d, want 1", n)
	}
	if got := len(s.Thread(thread).Injected); got != 16 {
		t.Fatalf("injected = %d", got)
	}
}

func TestInjectRebindAmbiguousStaysNoEndpoint(t *testing.T) {
	s1, s2 := appservertest.New(t), appservertest.New(t)
	s1.AddThread(unloadedThread())
	s2.AddThread(unloadedThread())
	a := &codex.Adapter{ControlSocket: s1.Endpoint(), Endpoints: []string{s2.Endpoint()}, RebindInterval: time.Nanosecond}
	d := bindUnhosted(t, a)
	load(s1)
	load(s2)
	if st, err := a.Inject(ctx(t), message("x"), d); st != adapter.Unreachable || !errors.Is(err, adapter.ErrNoEndpoint) {
		t.Fatalf("Inject = %s, %v", st, err)
	}
	if n := len(s1.Thread(thread).ToolOutputs) + len(s2.Thread(thread).ToolOutputs); n != 0 {
		t.Fatalf("delivered %d to an ambiguous thread", n)
	}
}

func TestInjectRebindsWhenBoundEndpointIsGone(t *testing.T) {
	old, moved := appservertest.New(t), appservertest.New(t)
	old.AddThread(idleThread())
	moved.AddThread(unloadedThread())
	a := &codex.Adapter{ControlSocket: old.Endpoint(), Endpoints: []string{moved.Endpoint()}, RebindInterval: time.Nanosecond}
	b, err := a.Register(ctx(t), thread)
	if err != nil || b.HarnessEndpoint != old.Endpoint() {
		t.Fatalf("Register = %+v, %v", b, err)
	}
	old.Close()
	load(moved)
	if st, err := a.Inject(ctx(t), message("x"), adapter.Delivery{Mode: msg.Immediate, VerifiedSender: true, Target: b}); st != adapter.Accepted || err != nil {
		t.Fatalf("Inject = %s, %v", st, err)
	}
	if got := moved.Thread(thread).ToolOutputs; len(got) != 1 {
		t.Fatalf("tool outputs on moved server = %+v", got)
	}
}

func TestInjectRebindsAcrossTwoMoves(t *testing.T) {
	a1, b1, c1 := appservertest.New(t), appservertest.New(t), appservertest.New(t)
	a1.AddThread(idleThread())
	b1.AddThread(unloadedThread())
	c1.AddThread(unloadedThread())
	a := &codex.Adapter{ControlSocket: a1.Endpoint(), Endpoints: []string{b1.Endpoint(), c1.Endpoint()}, RebindInterval: time.Nanosecond}
	b, err := a.Register(ctx(t), thread)
	if err != nil || b.HarnessEndpoint != a1.Endpoint() {
		t.Fatalf("Register = %+v, %v", b, err)
	}
	d := adapter.Delivery{Mode: msg.Immediate, VerifiedSender: true, Target: b}
	for i, step := range []struct{ gone, next *appservertest.Server }{{a1, b1}, {b1, c1}} {
		step.gone.Close()
		load(step.next)
		if st, err := a.Inject(ctx(t), message("x"), d); st != adapter.Accepted || err != nil {
			t.Fatalf("move %d: Inject = %s, %v", i+1, st, err)
		}
		if got := step.next.Thread(thread).ToolOutputs; len(got) != 1 {
			t.Fatalf("move %d: tool outputs = %+v", i+1, got)
		}
	}
}

func TestStalledLookupBlocksNeitherOtherThreadsNorCanceledSends(t *testing.T) {
	old := codex.EndpointProbeTimeout
	codex.EndpointProbeTimeout = 3 * time.Second
	defer func() { codex.EndpointProbeTimeout = old }()
	s := appservertest.New(t)
	s.AddThread(unloadedThread())
	a := &codex.Adapter{ControlSocket: s.Endpoint(), RebindInterval: time.Nanosecond}
	d := bindUnhosted(t, a)
	load(s)
	if st, err := a.Inject(ctx(t), message("recover"), d); st != adapter.Accepted || err != nil {
		t.Fatalf("recovery: %s, %v", st, err)
	}

	dir, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "stall.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	a.Endpoints = []string{"unix://" + sock}
	other := adapter.Delivery{Mode: msg.Immediate, VerifiedSender: true, Target: adapter.Binding{Harness: adapter.Codex, SessionRef: "019a0000-0000-7000-8000-0000000000ff"}}
	stalled := make(chan struct{})
	go func() {
		a.Inject(context.Background(), message("x"), other)
		close(stalled)
	}()
	select {
	case c := <-accepted:
		defer c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("lookup never reached the stalled endpoint")
	}

	start := time.Now()
	if st, err := a.Inject(ctx(t), message("during"), d); st != adapter.Accepted || err != nil || time.Since(start) > time.Second {
		t.Fatalf("recovered thread during a stalled lookup: %s, %v after %s", st, err, time.Since(start))
	}
	cctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	if st, _ := a.Inject(cctx, message("y"), other); st != adapter.Unreachable || time.Since(start) > time.Second {
		t.Fatalf("canceled waiter: %s after %s", st, time.Since(start))
	}
	select {
	case <-stalled:
		t.Fatal("stalled lookup finished early; the test did not overlap it")
	default:
	}
	<-stalled
}
