package adapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func message(id string) msg.Message {
	return msg.Message{MessageID: id, ContextID: "c", From: "a", To: "b", Kind: msg.Inform, Delivery: msg.Immediate, SentAt: time.Now(), Parts: []msg.Part{{Type: "text", Text: "x"}}}
}

func target(ref string) Delivery {
	return Delivery{Mode: msg.Immediate, Target: Binding{Harness: Codex, SessionRef: ref}}
}

func TestStateInjectable(t *testing.T) {
	for _, s := range []State{Accepted, Queued, Rejected, Unreachable, Unknown} {
		if !s.Injectable() {
			t.Errorf("%s not injectable", s)
		}
	}
	for _, s := range []State{Uncorrelated, "dispatching", ""} {
		if s.Injectable() {
			t.Errorf("%q injectable", s)
		}
	}
}

func TestFakeDefaultAccepts(t *testing.T) {
	f := NewFake(Claude)
	st, err := f.Inject(context.Background(), message("m1"), target("s1"))
	if st != Accepted || err != nil {
		t.Fatalf("got %s %v", st, err)
	}
	w := f.Writes()
	if len(w) != 1 || w[0].Message.MessageID != "m1" || w[0].SessionRef != "s1" {
		t.Fatalf("writes = %+v", w)
	}
}

func TestFakeScriptedOrderAndLog(t *testing.T) {
	f := NewFake(Codex)
	f.ScriptInject(
		Outcome{State: Unreachable, NoWrite: true},
		Outcome{State: Unknown, Err: ErrDisconnected},
		Outcome{State: Queued},
	)
	ctx := context.Background()
	want := []struct {
		s     State
		err   error
		wrote bool
	}{{Unreachable, nil, false}, {Unknown, ErrDisconnected, true}, {Queued, nil, true}, {Accepted, nil, true}}
	for i, w := range want {
		st, err := f.Inject(ctx, message("m"), target("t"))
		if st != w.s || !errors.Is(err, w.err) {
			t.Fatalf("call %d: got %s %v, want %s %v", i, st, err, w.s, w.err)
		}
	}
	calls := f.Calls()
	if len(calls) != 4 {
		t.Fatalf("calls = %d", len(calls))
	}
	for i, w := range want {
		if calls[i].Wrote != w.wrote || calls[i].State != w.s {
			t.Errorf("call %d logged %+v", i, calls[i])
		}
	}
	if len(f.Writes()) != 3 {
		t.Fatalf("writes = %d, want 3", len(f.Writes()))
	}
}

func TestFakeGateBlocksUntilReleased(t *testing.T) {
	f := NewFake(Codex)
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	f.ScriptInject(Outcome{State: Accepted, Entered: entered, Gate: gate})
	done := make(chan State)
	go func() {
		st, _ := f.Inject(context.Background(), message("m"), target("t"))
		done <- st
	}()
	<-entered
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("write recorded before gate: %d", n)
	}
	close(gate)
	if st := <-done; st != Accepted {
		t.Fatalf("got %s", st)
	}
	if n := len(f.Writes()); n != 1 {
		t.Fatalf("writes = %d", n)
	}
}

func TestFakeGateCancelRecordsNoWrite(t *testing.T) {
	f := NewFake(Codex)
	entered := make(chan struct{}, 1)
	f.ScriptInject(Outcome{State: Accepted, Entered: entered, Gate: make(chan struct{})})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := f.Inject(ctx, message("m"), target("t"))
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.Writes()); n != 0 {
		t.Fatalf("writes = %d, want 0", n)
	}
}

func TestFakeStatusRaceViaAfter(t *testing.T) {
	f := NewFake(Codex)
	f.ScriptStatus("t", SessionStatus{Loaded: true, Busy: true, Reachable: true})
	f.ScriptInject(Outcome{State: Accepted, After: func(Call) {
		f.mu.Lock()
		f.statuses["t"] = []SessionStatus{{Loaded: true, Reachable: true}}
		f.mu.Unlock()
	}})
	ctx := context.Background()
	if s, _ := f.Status(ctx, "t"); !s.Busy {
		t.Fatal("want busy before inject")
	}
	_, _ = f.Inject(ctx, message("m"), target("t"))
	if s, _ := f.Status(ctx, "t"); s.Busy {
		t.Fatal("want idle after inject")
	}
}

func TestFakeStatusSequenceLastSticks(t *testing.T) {
	f := NewFake(Codex)
	f.ScriptStatus("t", SessionStatus{Busy: true}, SessionStatus{Busy: false, Loaded: true})
	ctx := context.Background()
	got := []bool{}
	for range 3 {
		s, _ := f.Status(ctx, "t")
		got = append(got, s.Busy)
	}
	if got[0] != true || got[1] != false || got[2] != false {
		t.Fatalf("got %v", got)
	}
	if s, _ := f.Status(ctx, "other"); !s.Loaded || !s.Reachable {
		t.Fatalf("default status %+v", s)
	}
}

func TestFakeRegister(t *testing.T) {
	f := NewFake(Codex)
	ctx := context.Background()
	b, err := f.Register(ctx, "t1")
	if err != nil || b.SessionRef != "t1" || b.Harness != Codex || b.HarnessEndpoint == "" {
		t.Fatalf("got %+v %v", b, err)
	}
	f.SetBinding(Binding{Harness: Codex, SessionRef: "t2", HarnessEndpoint: "/sock", HarnessVersion: "0.166.0"})
	if b, _ := f.Register(ctx, "t2"); b.HarnessVersion != "0.166.0" {
		t.Fatalf("got %+v", b)
	}
	boom := errors.New("ambiguous_endpoint")
	f.SetRegisterError("t3", boom)
	if _, err := f.Register(ctx, "t3"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestFakeRejectsUncorrelatedScript(t *testing.T) {
	f := NewFake(Codex)
	f.ScriptInject(Outcome{State: Uncorrelated})
	defer func() {
		if recover() == nil {
			t.Fatal("scripting uncorrelated as an inject result did not panic")
		}
	}()
	_, _ = f.Inject(context.Background(), message("m"), target("t"))
}
