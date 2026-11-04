package codex_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type descriptorDirectory struct{ records []registry.Descriptor }

func (d *descriptorDirectory) List() ([]registry.Descriptor, error) { return d.records, nil }

func TestReviewDescriptorEndpointBinding(t *testing.T) {
	s := appservertest.New(t)
	yes := true
	s.AddThread(&appservertest.Thread{ID: thread, Loaded: true, CanAcceptDirectInput: &yes})
	d := &descriptorDirectory{}
	h := codex.NewHarness(codex.HarnessOptions{Adapter: &codex.Adapter{ControlSocket: t.TempDir() + "/absent.sock"}, Directory: d})
	ep, bare := s.Endpoint(), strings.TrimPrefix(s.Endpoint(), "unix://")
	d.records = []registry.Descriptor{{Harness: "codex", HarnessEndpoint: &ep}, {Harness: "codex", HarnessEndpoint: &bare}}
	ss, err := h.Discover(ctx(t))
	if err != nil || len(ss) != 1 || !ss[0].Reachable {
		t.Fatalf("Discover = %+v, %v", ss, err)
	}
	b, ok, err := h.Bind(ctx(t), harness.RequestContext{Meta: map[string]any{"threadId": thread}})
	if err != nil || !ok || b.HarnessEndpoint != ep {
		t.Fatalf("descriptor-discovered thread cannot bind: binding=%+v ok=%v err=%v", b, ok, err)
	}
	if st, err := h.Status(ctx(t), thread); err != nil || !st.Reachable {
		t.Fatalf("bound Status = %+v, %v", st, err)
	}
	for _, mode := range []msg.Delivery{msg.Immediate, msg.NextTurn} {
		state, err := h.Inject(ctx(t), message("descriptor endpoint"), adapter.Delivery{Target: b, Mode: mode, VerifiedSender: true})
		want := adapter.Accepted
		if mode == msg.NextTurn {
			want = adapter.Queued
		}
		if err != nil || state != want {
			t.Fatalf("Inject(%s) = %s, %v", mode, state, err)
		}
	}
}

func TestDescriptorEndpointParticipatesInAmbiguity(t *testing.T) {
	a, b := appservertest.New(t), appservertest.New(t)
	for _, s := range []*appservertest.Server{a, b} {
		s.AddThread(&appservertest.Thread{ID: thread, Loaded: true})
	}
	ep := b.Endpoint()
	h := codex.NewHarness(codex.HarnessOptions{Adapter: &codex.Adapter{ControlSocket: a.Endpoint()}, Directory: &descriptorDirectory{records: []registry.Descriptor{{Harness: "codex", HarnessEndpoint: &ep}}}})
	ss, err := h.Discover(ctx(t))
	if err != nil || len(ss) != 1 || ss[0].Reachable {
		t.Fatalf("ambiguous Discover = %+v, %v", ss, err)
	}
	bound, ok, err := h.Bind(ctx(t), harness.RequestContext{Meta: map[string]any{"threadId": thread}})
	if !ok || !errors.Is(err, harness.ErrAmbiguous) || bound.HarnessEndpoint != "" {
		t.Fatalf("ambiguous Bind = %+v, %v, %v", bound, ok, err)
	}
}

func TestDirectInputRequiresPositiveEvidence(t *testing.T) {
	for _, evidence := range []string{"missing", "null", "false", "true"} {
		t.Run(evidence, func(t *testing.T) {
			r := newRig(t, idleThread())
			r.srv.Hook = func(_ int, method string, _ json.RawMessage) appservertest.Reply {
				if method != "thread/read" {
					return appservertest.Reply{}
				}
				th := map[string]any{"id": thread, "status": map[string]any{"type": "idle"}}
				if evidence != "missing" {
					var value any
					if err := json.Unmarshal([]byte(evidence), &value); err != nil {
						panic(err)
					}
					th["canAcceptDirectInput"] = value
				}
				return appservertest.Reply{Handled: true, Result: map[string]any{"thread": th}}
			}
			want := evidence == "true"
			if st, err := r.a.Status(ctx(t), thread); err != nil || st.Reachable != want {
				t.Errorf("Status = %+v, %v; reachable want %v", st, err, want)
			}
			ss := r.a.LoadedThreads(ctx(t))
			if len(ss) != 1 || ss[0].Session.Reachable != want {
				t.Errorf("LoadedThreads = %+v; reachable want %v", ss, want)
			}
			for _, mode := range []msg.Delivery{msg.Immediate, msg.NextTurn} {
				st, err := r.a.Inject(ctx(t), message("evidence"), r.delivery(true, mode))
				if !want && (st != adapter.Unreachable || err == nil) {
					t.Errorf("Inject(%s) = %s, %v; want unreachable", mode, st, err)
				}
				wantState := adapter.Accepted
				if mode == msg.NextTurn {
					wantState = adapter.Queued
				}
				if want && (err != nil || st != wantState) {
					t.Errorf("Inject(%s) = %s, %v; want %s", mode, st, err, wantState)
				}
			}
			if !want && (len(r.srv.CallsTo("turn/start")) != 0 || len(r.srv.CallsTo("thread/inject_items")) != 0) {
				t.Error("delivery attempted without positive direct-input evidence")
			}
		})
	}
}

func TestCodexNeverClaimsSupersession(t *testing.T) {
	var h harness.Harness = codex.NewHarness(codex.HarnessOptions{Adapter: &codex.Adapter{ControlSocket: t.TempDir() + "/absent.sock"}})
	if _, ok := h.(harness.Supersession); ok {
		t.Fatal("a pooled process serves several live threads; Codex has no proof that one ended")
	}
}
