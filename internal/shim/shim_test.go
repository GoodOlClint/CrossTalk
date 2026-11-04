package shim

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

func journalRecords(t *testing.T, w *world, agentID string) []journal.Record {
	t.Helper()
	b, err := os.ReadFile(journal.Path(w.state, agentID))
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Record
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var r journal.Record
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestConcurrentFirstCallsShareOneState(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	n := w.node(alice.pid, nil, nil)

	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[string]int{}
	for range 16 {
		wg.Go(func() {
			isErr, out := n.call(nil, "receive_message", map[string]any{})
			mu.Lock()
			defer mu.Unlock()
			if isErr {
				codes[out["error"].(string)+"/"+out["reason"].(string)]++
			} else {
				codes["ok"]++
			}
		})
	}
	wg.Wait()
	for k := range codes {
		if k != "ok" && k != "not_ready/binding_in_progress" {
			t.Fatalf("unexpected outcome %s in %v", k, codes)
		}
	}
	n.ok(nil, "receive_message", map[string]any{})

	n.sh.mu.Lock()
	count := len(n.sh.agents)
	st := n.sh.agents[alice.agentID()]
	n.sh.mu.Unlock()
	if count != 1 || st == nil || !st.started() {
		t.Fatalf("agents = %d, state %v", count, st)
	}
	if fi, err := os.Stat(st.UDSPath()); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("uds socket: %v %v", fi, err)
	}
	if fi, err := os.Stat(udsingress.KeyPath(st.UDSPath())); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("uds key: %v %v", fi, err)
	}
	d, ok := w.descriptor(alice.agentID())
	if !ok || d.Name != "alice" || d.HarnessEndpoint == nil || *d.HarnessEndpoint != alice.sock || d.UDSPath != st.UDSPath() {
		t.Fatalf("descriptor %+v", d)
	}

	n.ok(nil, "send_message", send("nobody", "hi"))
	for _, r := range journalRecords(t, w, alice.agentID()) {
		if r.Binding.HarnessEndpoint != alice.sock || r.Binding.SessionRef != "sess-alice" || r.Binding.InstanceID != d.InstanceID {
			t.Fatalf("journal record bound to %+v, want endpoint %s", r.Binding, alice.sock)
		}
	}
}

func TestSecondShimForSameSessionIsRejected(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	n1 := w.node(alice.pid, nil, nil)
	n2 := w.node(alice.pid, nil, nil)
	n1.ok(nil, "receive_message", map[string]any{})
	n2.fails(nil, "receive_message", map[string]any{}, mcpserver.CodeRejected, "agent_held")
	if _, err := os.Stat(w.state + "/sock/" + alice.agentID() + ".sock"); err != nil {
		t.Fatalf("first shim's socket gone after second's failed bind: %v", err)
	}
}

func TestDiscoverMergesClaudeCodexAndDescriptors(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	if err := os.WriteFile(w.sessions+"/4002.json", []byte(`{"pid":4002,"sessionId":"sess-quiet","name":"quiet","cwd":"/q","status":"idle","messagingSocketPath":"/nonexistent.sock"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.sessions+"/999999.json", []byte(`{"pid":999999,"sessionId":"sess-dead","name":"dead"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := appservertest.New(t)
	srv.AddThread(&appservertest.Thread{ID: "T-live", Name: "cx", Cwd: "/work/cx", Loaded: true})
	srv.AddThread(&appservertest.Thread{ID: "T-stored", Name: "old", Cwd: "/old"})
	cx := &codex.Adapter{ControlSocket: shortDir(t) + "/absent.sock", Endpoints: []string{srv.Endpoint()}}
	n := w.node(alice.pid, nil, func(o *Options) { o.Harnesses = w.harnesses(cx) })

	out := n.ok(nil, "list_agents", map[string]any{})
	rows := map[string]map[string]any{}
	for _, r := range out["agents"].([]any) {
		m := r.(map[string]any)
		rows[m["name"].(string)] = m
	}
	want := map[string][2]bool{"alice": {true, true}, "quiet": {false, false}, "cx": {false, true}}
	if len(rows) != len(want) {
		t.Fatalf("rows %v", rows)
	}
	for name, f := range want {
		r := rows[name]
		if r == nil || r["crosstalk"] != f[0] || r["reachable"] != f[1] {
			t.Errorf("%s: row %v, want crosstalk=%v reachable=%v", name, r, f[0], f[1])
		}
	}
	if rows["cx"]["harness"] != "codex" || rows["cx"]["cwd"] != "/work/cx" {
		t.Errorf("codex row %v", rows["cx"])
	}

	absent := &codex.Adapter{ControlSocket: shortDir(t) + "/absent.sock"}
	sh, err := New(Options{StateDir: w.state, PPID: 1, Harnesses: w.harnesses(absent)})
	if err != nil {
		t.Fatal(err)
	}
	ss, err := sh.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Harness == adapter.Codex {
			t.Fatalf("codex row with no daemon: %+v", s)
		}
	}
}

func TestSendNativeClaudeThroughSocket(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-bob", "bob")
	n := w.node(alice.pid, nil, nil)

	out := n.ok(nil, "send_message", send("bob", "hello <b>"))
	if out["state"] != "accepted" {
		t.Fatalf("send %v", out)
	}
	got := bob.got()
	from := `from="uds:` + w.state + "/sock/" + alice.agentID() + `.sock"`
	if len(got) != 1 || !strings.Contains(got[0], from) || !strings.Contains(got[0], `from-name="alice"`) || !strings.Contains(got[0], "hello &lt;b>") {
		t.Fatalf("bob got %q, want envelope %s", got, from)
	}
	if bob.tokens[0] != bob.token {
		t.Fatalf("auth token %q", bob.tokens[0])
	}

	if out := n.ok(nil, "send_message", map[string]any{"to": "bob", "text": "x", "kind": "command"}); out["state"] != "rejected" || out["reason"] != ReasonNativeInformOnly {
		t.Fatalf("native command %v", out)
	}
	if out := n.ok(nil, "ask", send("bob", "q?")); out["state"] != "rejected" || out["reason"] != ReasonNativeInformOnly {
		t.Fatalf("native ask %v", out)
	}
	if out := n.ok(nil, "send_message", send("ghost", "x")); out["state"] != "unknown" || out["reason"] != ReasonNoSuchAgent {
		t.Fatalf("unknown name %v", out)
	}
	if out := n.ok(nil, "send_message", send("alice", "me")); out["state"] != "rejected" || out["reason"] != ReasonSelf {
		t.Fatalf("self %v", out)
	}
	if len(bob.got()) != 1 {
		t.Fatalf("bob got extra writes %q", bob.got())
	}

	var outbound []journal.Record
	for _, r := range journalRecords(t, w, alice.agentID()) {
		if r.Type == journal.TypeOutbound {
			outbound = append(outbound, r)
		}
	}
	if len(outbound) < 2 || outbound[0].To != "claude:sess-bob" || outbound[0].State != "submitted" || outbound[1].State != "accepted" {
		t.Fatalf("outbound records %+v", outbound)
	}
}

func TestSendNativeCodexThroughAdapter(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-cx", "cx")
	n := w.node(alice.pid, nil, nil)
	if out := n.ok(nil, "send_message", map[string]any{"to": "cx", "text": "hi", "delivery": "next_turn"}); out["state"] != "accepted" {
		t.Fatalf("send %v", out)
	}
	ws := w.codex.Writes()
	if len(ws) != 1 || ws[0].SessionRef != "T-cx" || ws[0].Delivery.Mode != msg.NextTurn || ws[0].Message.From != "alice" {
		t.Fatalf("codex writes %+v", ws)
	}
}

func TestSendToCrossTalkAgentWithoutTransport(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-bob", "bob")
	na := w.node(alice.pid, nil, nil)
	nb := w.node(bob.pid, nil, nil)
	nb.ok(nil, "receive_message", map[string]any{})
	if out := na.ok(nil, "send_message", send("bob", "hi")); out["state"] != "unreachable" || out["reason"] != ReasonNoTransport {
		t.Fatalf("send %v", out)
	}
	if len(bob.got()) != 0 {
		t.Fatalf("crosstalk target injected natively: %q", bob.got())
	}
	na.fails(nil, "claim", map[string]any{"name": "bob"}, "unreachable", ReasonNoTransport)
}

func TestReplyRoutesThroughJournal(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	var mu sync.Mutex
	var hooked []string
	hook := func(o *Options) {
		o.OnReply = func(_ context.Context, st *AgentState, orig, reply msg.Message) error {
			mu.Lock()
			hooked = append(hooked, st.Agent().Name()+":"+orig.MessageID+":"+*reply.InReplyTo)
			mu.Unlock()
			return nil
		}
	}
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, hook)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})

	ask := na.ok(nil, "ask", send("bob", "what is 2+2?"))
	if ask["state"] != "accepted" {
		t.Fatalf("ask %v", ask)
	}
	askID := ask["message_id"].(string)
	ws := w.codex.Writes()
	if len(ws) != 1 || ws[0].Message.MessageID != askID || ws[0].Message.Kind != msg.Ask || ws[0].Message.From != "alice" || !ws[0].Delivery.VerifiedSender {
		t.Fatalf("bob's harness got %+v", ws)
	}

	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": "nope", "text": "x"}, mcpserver.CodeInvalid, ReasonNoSuchMessage)
	rep := nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": askID, "text": "4"})
	if rep["state"] != "accepted" {
		t.Fatalf("reply %v", rep)
	}
	got := alice.got()
	if len(got) != 1 || !strings.Contains(got[0], `from-name="bob"`) || !strings.Contains(got[0], "\n4\n") {
		t.Fatalf("alice got %q", got)
	}
	mu.Lock()
	if len(hooked) != 1 || hooked[0] != "bob:"+askID+":"+askID {
		t.Fatalf("hook calls %v", hooked)
	}
	mu.Unlock()

	bobID := identity.AgentID("codex", "T-bob")
	var obl []string
	for _, r := range journalRecords(t, w, bobID) {
		if r.Type == journal.TypeObligation && r.InReplyTo == askID && r.To == alice.agentID() {
			obl = append(obl, r.State)
		}
	}
	if !slices.Equal(obl, []string{journal.StatePending, journal.StateCompleted, stageReleased, journal.StateDelivered}) {
		t.Fatalf("obligation states %v", obl)
	}
	var inReplyTo string
	for _, r := range journalRecords(t, w, alice.agentID()) {
		var m msg.Message
		if r.Type == journal.TypeDedupe && json.Unmarshal(r.Message, &m) == nil && m.InReplyTo != nil {
			inReplyTo = *m.InReplyTo
		}
	}
	if inReplyTo != askID {
		t.Fatalf("alice's journal in_reply_to %q, want %s", inReplyTo, askID)
	}

	var routeStates []string
	for _, r := range journalRecords(t, w, bobID) {
		if r.Type == journal.TypeRoute && r.MessageID == askID {
			routeStates = append(routeStates, r.State)
		}
	}
	if !slices.Equal(routeStates, []string{journal.StateOpen, journal.StateClosed}) {
		t.Fatalf("route states after reply %v", routeStates)
	}

	nb.fails(thread("T-bob"), "reply", map[string]any{"message_id": askID, "text": "4 again"}, mcpserver.CodeRejected, ReasonAlreadyReplied)
	if again := nb.ok(thread("T-bob"), "reply", map[string]any{"message_id": askID, "text": "4"}); again["message_id"] != rep["message_id"] || again["state"] != "accepted" {
		t.Fatalf("identical retry %v, first %v", again, rep)
	}
	if len(alice.got()) != 1 {
		t.Fatalf("one reply per ask, alice got %q", alice.got())
	}
	mu.Lock()
	if len(hooked) != 1 {
		t.Fatalf("hook calls after retries %v", hooked)
	}
	mu.Unlock()
}

func TestNativeReplyBouncedToOwnSession(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	bob := w.claude(4002, "sess-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	w.node(bob.pid, w.lb, nil)
	na.sh.udsPeerPID = func(*net.UnixConn) (int, error) { return alice.pid, nil }
	na.ok(nil, "receive_message", map[string]any{})

	st := na.sh.Agent(alice.agentID())
	key, err := udsingress.EnsureKey(udsingress.KeyPath(st.UDSPath()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", st.UDSPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(`{"type":"auth","token":"` + key + `"}` + "\n" + `{"type":"user","message":{"role":"user","content":"pong"}}` + "\n"))
	ack := make([]byte, 512)
	nr, _ := c.Read(ack)
	if !strings.Contains(string(ack[:nr]), `"state":"bounced"`) {
		t.Fatalf("native frame ack %s", ack[:nr])
	}
	got := alice.got()
	if len(got) != 1 || !strings.Contains(got[0], "not delivered") {
		t.Fatalf("alice got %q", got)
	}
	if len(bob.got()) != 0 {
		t.Fatalf("bob got %q", bob.got())
	}
}

func TestClaimReleaseAcrossShims(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	w.codex.addThread("T-carol", "carol")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nc := w.node(2, w.lb, nil)
	na.ok(nil, "receive_message", map[string]any{})
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	nc.ok(thread("T-carol"), "receive_message", map[string]any{})
	bobID := identity.AgentID("codex", "T-bob")

	out := na.ok(nil, "claim", map[string]any{"name": "bob"})
	if out["owner"] != "alice" {
		t.Fatalf("claim %v", out)
	}
	d, _ := w.descriptor(bobID)
	if d.Role != registry.RoleSubagent || d.Owner == nil || *d.Owner != "alice" {
		t.Fatalf("bob descriptor %+v", d)
	}
	ws := w.codex.Writes()
	if len(ws) == 0 || !strings.Contains(ws[len(ws)-1].Message.Parts[0].Text, "alice claimed you") {
		t.Fatalf("bob not told: %+v", ws)
	}

	na.fails(nil, "claim", map[string]any{"name": "bob"}, mcpserver.CodeRejected, "already_claimed")
	nc.fails(thread("T-carol"), "release", map[string]any{"name": "bob"}, mcpserver.CodeRejected, "not_owner")
	nb.fails(thread("T-bob"), "claim", map[string]any{"name": "bob"}, mcpserver.CodeRejected, "self")
	nb.fails(thread("T-bob"), "claim", map[string]any{"name": "carol"}, mcpserver.CodeRejected, "caller_not_primary")

	sub, _ := w.descriptor(bobID)
	if err := nc.sh.ApplyClaim(context.Background(), sub, identity.AgentID("codex", "T-carol"), true); !isReason(err, ReasonNotPrimary) {
		t.Fatalf("claim by a subagent owner: %v", err)
	}

	na.ok(nil, "release", map[string]any{"name": "bob"})
	d, _ = w.descriptor(bobID)
	if d.Role != registry.RolePrimary || d.Owner != nil {
		t.Fatalf("bob after release %+v", d)
	}
	ws = w.codex.Writes()
	if !strings.Contains(ws[len(ws)-1].Message.Parts[0].Text, "released you") {
		t.Fatalf("bob not told of release: %+v", ws[len(ws)-1])
	}
}

func isReason(err error, reason string) bool {
	var e *mcpserver.Error
	return errors.As(err, &e) && e.Reason == reason
}

func TestAskDeadlineInformsOwnSession(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	ask := na.ok(nil, "ask", map[string]any{"to": "bob", "text": "slow?", "deadline_seconds": 1})
	id := ask["message_id"].(string)
	waitFor(t, "deadline inform", func() bool { return len(alice.got()) == 1 })
	if got := alice.got()[0]; !strings.Contains(got, id) || !strings.Contains(got, "expired") || !strings.Contains(got, `from-name="crosstalk"`) {
		t.Fatalf("alice got %q", got)
	}
}

func TestExitRemovesSocketsKeysAndDescriptorsKeepsJournal(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	n := w.node(alice.pid, nil, nil)
	n.ok(nil, "receive_message", map[string]any{})
	n.ok(thread("T-bob"), "receive_message", map[string]any{})
	ids := []string{alice.agentID(), identity.AgentID("codex", "T-bob")}
	reg := registry.New(w.state)
	for _, id := range ids {
		if _, err := os.Stat(reg.UDSPath(id)); err != nil {
			t.Fatal(err)
		}
	}
	n.stop()
	for _, id := range ids {
		if _, err := os.Stat(reg.UDSPath(id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("socket %s survives exit: %v", id, err)
		}
		if _, err := os.Stat(reg.DescriptorPath(id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("descriptor %s survives exit: %v", id, err)
		}
		if _, err := os.Stat(journal.Path(w.state, id)); err != nil {
			t.Errorf("journal %s removed: %v", id, err)
		}
		if _, err := os.Stat(udsingress.KeyPath(reg.UDSPath(id))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("uds key %s survives exit: %v", id, err)
		}
	}

	n2 := w.node(alice.pid, nil, nil)
	n2.ok(nil, "receive_message", map[string]any{})
	if d, ok := w.descriptor(alice.agentID()); !ok || d.Name != "alice" {
		t.Fatalf("re-registration %+v %v", d, ok)
	}
}

type pendingTransport struct{ taskID string }

func (p pendingTransport) Send(context.Context, registry.Descriptor, registry.Descriptor, msg.Message, bool) (adapter.State, error) {
	return adapter.Unknown, errors.New("pending")
}

func (p pendingTransport) SendTask(context.Context, registry.Descriptor, registry.Descriptor, msg.Message, bool) (string, adapter.State, error) {
	return p.taskID, adapter.Unknown, errors.New("pending")
}

func TestMailboxListsOutboundUnknown(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.addThread("T-bob", "bob")
	na := w.node(alice.pid, pendingTransport{taskID: "task-far"}, nil)
	nb := w.node(1, w.lb, nil)
	nb.ok(thread("T-bob"), "receive_message", map[string]any{})
	sent := na.ok(nil, "send_message", send("bob", "hello"))
	id := sent["message_id"].(string)
	if sent["state"] != "unknown" {
		t.Fatalf("send %v", sent)
	}
	ms := na.ok(nil, "receive_message", map[string]any{})["messages"].([]any)
	if len(ms) != 1 {
		t.Fatalf("mailbox %v", ms)
	}
	e := ms[0].(map[string]any)
	if e["direction"] != "outbound" || e["message_id"] != id || e["task_id"] != "task-far" || e["state"] != "unknown" || e["reason"] != "pending" || e["to"] != identity.AgentID("codex", "T-bob") {
		t.Fatalf("entry %v", e)
	}
	if ms := nb.ok(thread("T-bob"), "receive_message", map[string]any{})["messages"].([]any); len(ms) != 0 {
		t.Fatalf("bob mailbox %v", ms)
	}
}

func fakeClock(t0 time.Time) func() time.Time { return func() time.Time { return t0 } }

func TestAskDeadlineRearmedAcrossRestart(t *testing.T) {
	setup := func() (*world, *fakeClaude, time.Time) {
		w := newWorld(t)
		alice := w.claude(4001, "sess-alice", "alice")
		w.codex.addThread("T-bob", "bob")
		t0 := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
		na := w.node(alice.pid, pendingTransport{taskID: "task-far"}, func(o *Options) { o.Now = fakeClock(t0) })
		nb := w.node(1, w.lb, nil)
		nb.ok(thread("T-bob"), "receive_message", map[string]any{})
		na.ok(nil, "ask", map[string]any{"to": "bob", "text": "slow?", "deadline_seconds": 60})
		na.stop()
		return w, alice, t0
	}

	t.Run("past", func(t *testing.T) {
		w, alice, t0 := setup()
		w.node(alice.pid, nil, func(o *Options) { o.Now = fakeClock(t0.Add(2 * time.Minute)) }).ok(nil, "receive_message", map[string]any{})
		waitFor(t, "expiry after restart", func() bool { return len(alice.got()) == 1 })
		if got := alice.got()[0]; !strings.Contains(got, "expired") {
			t.Fatalf("alice got %q", got)
		}
	})

	t.Run("future", func(t *testing.T) {
		w, alice, t0 := setup()
		n := w.node(alice.pid, nil, func(o *Options) { o.Now = fakeClock(t0.Add(10 * time.Second)) })
		n.ok(nil, "receive_message", map[string]any{})
		st := n.sh.Agent(alice.agentID())
		st.mu.Lock()
		armed := len(st.deadline)
		st.mu.Unlock()
		if armed != 1 || len(alice.got()) != 0 {
			t.Fatalf("armed %d, delivered %v", armed, alice.got())
		}
	})
}
