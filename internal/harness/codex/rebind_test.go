package codex_test

import (
	"errors"
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
