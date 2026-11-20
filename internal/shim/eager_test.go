package shim

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

const eagerTick = 2 * time.Millisecond

// eager is one shim for the Claude session in ppid's session file, with the
// Claude harness's claim source running and its A2A listeners counted.
type eager struct {
	*node
	claude *claude.Harness
	ppid   int
	open   atomic.Int64
	cancel context.CancelFunc
	done   chan error
}

func (w *world) eagerNode(ppid int) *eager {
	t := w.t
	ch, err := claude.NewHarness(claude.HarnessOptions{SessionsDir: w.sessions, Alive: fakePID, Ingress: udsingress.NewIngress, PPID: ppid, PollInterval: eagerTick})
	if err != nil {
		t.Fatal(err)
	}
	e := &eager{claude: ch, ppid: ppid, done: make(chan error, 1)}
	e.node = w.node(ppid, w.lb, func(o *Options) {
		o.Harnesses = []harness.Harness{codex.NewHarness(codex.HarnessOptions{Adapter: w.codex, Directory: registry.New(w.state)}), ch}
		o.StartA2A = func(_ context.Context, st *AgentState) (A2AListener, error) {
			e.open.Add(1)
			var once sync.Once
			return A2AListener{URL: "http://127.0.0.1:1/" + st.Agent().ID(), Close: func(context.Context) error {
				once.Do(func() { e.open.Add(-1) })
				return nil
			}}, nil
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() { e.done <- ch.ServeClaims(ctx, e.srv) }()
	t.Cleanup(e.stopClaims)
	return e
}

func (e *eager) stopClaims() {
	e.cancel()
	select {
	case err := <-e.done:
		if err != nil {
			e.t.Errorf("ServeClaims: %v", err)
		}
		e.done <- nil
	case <-time.After(5 * time.Second):
		e.t.Fatal("ServeClaims did not stop with its context")
	}
}

// firstCall is the model's first CrossTalk call, retried while the claim
// source's registration of the same session is still running.
func (e *eager) firstCall() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		isErr, out := e.call(nil, "receive_message", map[string]any{})
		if !isErr {
			return
		}
		if out["error"] != "not_ready" || out["reason"] != "binding_in_progress" || time.Now().After(deadline) {
			e.t.Fatalf("first call: %v", out)
		}
		time.Sleep(time.Millisecond)
	}
}

// claudeDescriptors are the live descriptors of the Claude process ppid.
func claudeDescriptors(t *testing.T, w *world, ppid int) []registry.Descriptor {
	t.Helper()
	ds, err := registry.New(w.state).List()
	if err != nil {
		t.Fatal(err)
	}
	var out []registry.Descriptor
	for _, d := range ds {
		if d.Harness == "claude" && d.HarnessPID != nil && *d.HarnessPID == ppid {
			out = append(out, d)
		}
	}
	return out
}

// assertOne checks that sessionID is the one Claude agent: one descriptor
// under the harness name, one live agent state and one open A2A listener.
func (e *eager) assertOne(w *world, sessionID, name string) {
	t := e.t
	t.Helper()
	ds := claudeDescriptors(t, w, e.ppid)
	if len(ds) != 1 || ds[0].AgentID != w.claudeAgentID(sessionID) || ds[0].Name != name {
		t.Fatalf("descriptors %+v, want one for %s named %q", ds, sessionID, name)
	}
	if e.sh.Agent(w.claudeAgentID(sessionID)) == nil {
		t.Fatalf("no agent state for %s", sessionID)
	}
	if n := e.open.Load(); n != 1 {
		t.Fatalf("%d open A2A listeners, want 1", n)
	}
}

// The claim source's registration and the model's first call race for the
// same session, and a poll tick lands while /clear swaps the session: one
// agent, one descriptor, one listener, the harness name unsuffixed.
func TestEagerRegistrationRacesFirstCallAndClear(t *testing.T) {
	for i := range 10 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			w := newWorld(t)
			bob := w.claude(4001, "sess-1", "bob")
			var e *eager
			var wg sync.WaitGroup
			ready := make(chan struct{})
			wg.Go(func() {
				<-ready
				e.firstCall()
			})
			e = w.eagerNode(bob.pid)
			close(ready)
			wg.Wait()
			waitFor(t, "registration", func() bool { return len(claudeDescriptors(t, w, bob.pid)) == 1 })
			e.assertOne(w, "sess-1", "bob")

			wg.Go(func() { w.become(bob, "sess-2", "bob") })
			wg.Go(func() {
				for range 20 {
					e.call(nil, "list_agents", map[string]any{})
				}
			})
			wg.Wait()
			waitFor(t, "registration after /clear", func() bool {
				ds := claudeDescriptors(t, w, bob.pid)
				return len(ds) == 1 && ds[0].AgentID == w.claudeAgentID("sess-2")
			})
			waitFor(t, "old listener closed", func() bool { return e.open.Load() == 1 })
			e.assertOne(w, "sess-2", "bob")

			e.stopClaims()
			e.node.stop()
			if n := e.open.Load(); n != 0 {
				t.Fatalf("%d A2A listeners left open after exit", n)
			}
			if ds := claudeDescriptors(t, w, bob.pid); len(ds) != 0 {
				t.Fatalf("descriptors after exit %+v", ds)
			}
		})
	}
}

func bobRow(t *testing.T, n *node) map[string]any {
	t.Helper()
	got := rows(t, n.ok(nil, "list_agents", map[string]any{}), "bob")
	if len(got) != 1 {
		t.Fatalf("bob rows %v", got)
	}
	return got[0]
}

// Done when 1 and 2 of #145: with no call by its model, the session is a
// CrossTalk agent in a peer's list_agents and accepts an ask.
func TestEagerRegistrationListsSessionAndAcceptsAskBeforeFirstCall(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-1", "bob")
	na := w.node(alice.pid, w.lb, nil)
	recv(na, nil)
	w.eagerNode(bob.pid)

	waitFor(t, "bob listed as a CrossTalk agent", func() bool { return bobRow(t, na)["crosstalk"] == true })
	if r := bobRow(t, na); r["agent_id"] != bob.agentID() || r["reachable"] != true {
		t.Fatalf("bob row %v", r)
	}
	if res := na.ok(nil, "ask", send("bob", "question?")); res["state"] != "accepted" {
		t.Fatalf("ask before bob's first call %v", res)
	}
	waitFor(t, "ask delivered", func() bool { return len(bob.got()) == 1 && strings.Contains(bob.got()[0], "question?") })
}

// Done when 3 of #145 and design test 32: after /clear the new session is an
// agent under the same name with no call, and the old one is gone.
func TestEagerRegistrationAfterClearKeepsNameWithoutCall(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-1", "bob")
	na := w.node(alice.pid, w.lb, nil)
	recv(na, nil)
	e := w.eagerNode(bob.pid)
	waitFor(t, "first registration", func() bool { _, ok := w.descriptor(bob.agentID()); return ok })
	oldID := bob.agentID()

	w.become(bob, "sess-2", "bob")
	waitFor(t, "registration after /clear", func() bool {
		d, ok := w.descriptor(bob.agentID())
		_, old := w.descriptor(oldID)
		return ok && d.Name == "bob" && !old
	})
	e.assertOne(w, "sess-2", "bob")
	if r := bobRow(t, na); r["agent_id"] != bob.agentID() || r["crosstalk"] != true {
		t.Fatalf("bob row %v", r)
	}
	if res := na.ok(nil, "ask", send("bob", "after clear?")); res["state"] != "accepted" {
		t.Fatalf("ask after /clear %v", res)
	}
}

func TestEagerRegistrationFollowsHarnessRename(t *testing.T) {
	w := newWorld(t)
	bob := w.claude(4002, "sess-1", "bob")
	w.eagerNode(bob.pid)
	waitFor(t, "registration", func() bool { _, ok := w.descriptor(bob.agentID()); return ok })
	w.become(bob, "sess-1", "robert")
	waitFor(t, "rename followed", func() bool { d, _ := w.descriptor(bob.agentID()); return d.Name == "robert" })
}

func TestEagerRegistrationWaitsForSessionFile(t *testing.T) {
	w := newWorld(t)
	e := w.eagerNode(4002)
	time.Sleep(20 * eagerTick)
	if ds := claudeDescriptors(t, w, 4002); len(ds) != 0 {
		t.Fatalf("registered with no session file: %+v", ds)
	}
	w.claude(4002, "sess-1", "bob")
	waitFor(t, "registration once the file appears", func() bool { return len(claudeDescriptors(t, w, 4002)) == 1 })
	e.assertOne(w, "sess-1", "bob")
}

func TestEagerRegistrationStopsWithContext(t *testing.T) {
	w := newWorld(t)
	e := w.eagerNode(4002)
	e.stopClaims()
	w.claude(4002, "sess-1", "bob")
	time.Sleep(20 * eagerTick)
	if ds := claudeDescriptors(t, w, 4002); len(ds) != 0 {
		t.Fatalf("registered after ServeClaims stopped: %+v", ds)
	}
}

// A second shim for the same session (a duplicate install) leaves the first
// holder's record alone and keeps the existing agent_held answer to calls.
func TestEagerRegistrationDuplicateShimLeavesFirstHolder(t *testing.T) {
	w := newWorld(t)
	bob := w.claude(4002, "sess-1", "bob")
	first := w.eagerNode(bob.pid)
	waitFor(t, "first registration", func() bool { _, ok := w.descriptor(bob.agentID()); return ok })
	before, _ := w.descriptor(bob.agentID())

	second := w.eagerNode(bob.pid)
	time.Sleep(20 * eagerTick)
	after, ok := w.descriptor(bob.agentID())
	if !ok || after.InstanceID != before.InstanceID || after.Name != "bob" {
		t.Fatalf("record changed: %+v -> %+v", before, after)
	}
	if second.sh.Agent(bob.agentID()) != nil || second.open.Load() != 0 {
		t.Fatalf("second shim holds an agent")
	}
	second.fails(nil, "receive_message", map[string]any{}, "rejected", "agent_held")
	first.assertOne(w, "sess-1", "bob")
}
