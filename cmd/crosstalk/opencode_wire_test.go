package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/opencode"
	"github.com/GoodOlClint/crosstalk/internal/harness/opencode/opencodetest"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const (
	ocSession = "ses_target1"
	ocSelfID  = "b0b0b0b0b0b0b0b0"
)

var ocAlice = receiver.Sender{Verified: true, AgentID: "a1a1a1a1a1a1a1a1", Name: "alice", Role: registry.RolePrimary}

func ocMsg(kind msg.Kind, text string) msg.Message {
	return msg.Message{MessageID: msg.NewUUIDv7(), ContextID: "ctx", From: "whoever", To: "bob", Kind: kind, Delivery: msg.Immediate, SentAt: time.Now(), Parts: []msg.Part{{Type: "text", Text: text}}}
}

type ocEnv struct {
	t    *testing.T
	srv  *opencodetest.Server
	path string
	r    *receiver.Receiver
	j    *journal.Journal
}

func newOCEnv(t *testing.T, wrap func(adapter.Adapter) adapter.Adapter) *ocEnv {
	t.Helper()
	srv := opencodetest.Start(t, opencodetest.Session{ID: ocSession, Title: "oc", Updated: time.Now()})
	path := opencodetest.WriteService(t, t.TempDir(), srv.URL, "2.4.0", os.Getpid())
	h := opencode.NewHarness(opencode.Options{ServicePath: path})
	b, err := h.Register(context.Background(), ocSession)
	if err != nil {
		t.Fatal(err)
	}
	var ad adapter.Adapter = h
	if wrap != nil {
		ad = wrap(ad)
	}
	j, _, err := journal.Open(t.TempDir(), ocSelfID, journal.Binding{InstanceID: "inst-1", HarnessEndpoint: b.HarnessEndpoint, SessionRef: b.SessionRef}, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	me := receiver.Recipient{AgentID: ocSelfID, Name: "bob", Role: registry.RolePrimary, Binding: b}
	return &ocEnv{t: t, srv: srv, path: path, j: j, r: receiver.New(receiver.Config{Journal: j, Adapter: ad, Self: func() receiver.Recipient { return me }})}
}

func (e *ocEnv) recv(m msg.Message) receiver.Result {
	return e.r.Receive(context.Background(), receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: ocAlice})
}

func TestOpenCodeDedupeReplayNoSecondPrompt(t *testing.T) {
	e := newOCEnv(t, nil)
	e.srv.Set(func(s *opencodetest.Service) { s.Prompt.Status = 500 })
	m := ocMsg(msg.Inform, "hello")
	if r := e.recv(m); r.State != adapter.Unknown {
		t.Fatalf("first send %+v, want unknown", r)
	}
	if got := len(e.srv.Prompts()); got != 1 {
		t.Fatalf("prompts after first send: %d", got)
	}
	if !e.r.RouteOpen(m.MessageID) {
		t.Fatal("route closed after unknown")
	}
	e.srv.Set(func(s *opencodetest.Service) { s.Prompt.Status = 0 })
	if r := e.recv(m); r.State != adapter.Unknown {
		t.Fatalf("replay %+v", r)
	}
	if got := len(e.srv.Prompts()); got != 1 {
		t.Fatalf("prompts after replay: %d, want exactly one", got)
	}
	if !e.r.RouteOpen(m.MessageID) {
		t.Fatal("replay closed the route")
	}
}

type noEndpoint struct{ adapter.Adapter }

func (n noEndpoint) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	st, err := n.Adapter.Inject(ctx, m, d)
	if st == adapter.Unreachable {
		err = errors.Join(err, adapter.ErrNoEndpoint)
	}
	return st, err
}

func TestOpenCodeReplyRouteAfterUnknown(t *testing.T) {
	e := newOCEnv(t, nil)
	e.srv.Set(func(s *opencodetest.Service) { s.Prompt.Drop = true })
	ask := ocMsg(msg.Ask, "q?")
	if r := e.recv(ask); r.State != adapter.Unknown {
		t.Fatalf("ask %+v, want unknown", r)
	}
	if !e.r.RouteOpen(ask.MessageID) {
		t.Fatal("route closed after unknown")
	}
	if ok, err := e.r.CloseRoute(ask.MessageID); err != nil || !ok {
		t.Fatalf("reply could not use the route: %v %v", ok, err)
	}

	for _, tc := range []struct {
		name   string
		break_ func(*ocEnv)
	}{
		{"401", func(e *ocEnv) { e.srv.Set(func(s *opencodetest.Service) { s.Prompt.Status = 401 }) }},
		{"refused", func(e *ocEnv) { e.srv.Close() }},
		{"missing service.json", func(e *ocEnv) {
			if err := os.Remove(e.path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOCEnv(t, nil)
			tc.break_(e)
			before := len(e.srv.Prompts())
			m := ocMsg(msg.Inform, "x")
			if r := e.recv(m); r.State != adapter.Unreachable {
				t.Fatalf("send %+v, want unreachable", r)
			}
			if e.r.RouteOpen(m.MessageID) {
				t.Fatal("route left open for an unreachable target")
			}
			if tc.name != "401" && len(e.srv.Prompts()) != before {
				t.Fatalf("a prompt was sent")
			}
		})
	}

	t.Run("receiver sentinel: an ask whose adapter wraps ErrNoEndpoint is rejected", func(t *testing.T) {
		e := newOCEnv(t, func(a adapter.Adapter) adapter.Adapter { return noEndpoint{a} })
		if err := os.Remove(e.path); err != nil {
			t.Fatal(err)
		}
		m := ocMsg(msg.Ask, "q?")
		if r := e.recv(m); r.State != adapter.Rejected || r.Reason != receiver.ReasonNoEndpoint {
			t.Fatalf("ask %+v", r)
		}
		if e.r.RouteOpen(m.MessageID) {
			t.Fatal("route left open")
		}
	})
}

func TestOpenCodeConcurrentPullWakeDeadline(t *testing.T) {
	e := newOCEnv(t, nil)
	unverified := receiver.Sender{PID: 31337}
	e.srv.Set(func(s *opencodetest.Service) { s.Prompt.Delay = 60 * time.Second })
	m := ocMsg(msg.Inform, "mailboxed body")
	done := make(chan receiver.Result, 1)
	go func() {
		done <- e.r.Receive(context.Background(), receiver.Inbound{Ingress: receiver.UDS, Message: m, Sender: unverified})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for len(e.srv.Prompts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(e.srv.Prompts()) == 0 {
		t.Fatal("no wake prompt reached the service")
	}
	type pulled struct {
		es  []receiver.Entry
		err error
	}
	pc := make(chan pulled, 1)
	go func() {
		es, _, err := e.r.Pull("", 0)
		pc <- pulled{es, err}
	}()
	var p pulled
	select {
	case p = <-pc:
	case <-time.After(15 * time.Second):
		t.Fatal("pull still blocked on the stalled prompt after 15 s")
	}
	if p.err != nil {
		t.Fatal(p.err)
	}
	if len(p.es) != 1 || p.es[0].MessageID != m.MessageID {
		t.Fatalf("pull %+v", p.es)
	}
	if r := <-done; r.State != adapter.Mailboxed {
		t.Fatalf("receive %+v, want mailboxed", r)
	}
}
