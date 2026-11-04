package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type env struct {
	t          *testing.T
	stateDir   string
	sessionDir string
	reg        *registry.Registry
	fb         *FakeBackend
	srv        *Server
	cs         *mcp.ClientSession
}

func newEnv(t *testing.T, ppid int, fb *FakeBackend) *env {
	t.Helper()
	e := &env{t: t, stateDir: t.TempDir(), fb: fb}
	sd, err := os.MkdirTemp("", "ctsess")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sd) })
	e.sessionDir = sd
	e.start(ppid)
	return e
}

func (e *env) start(ppid int) {
	t := e.t
	e.reg = registry.New(e.stateDir)
	hs := testHarnesses(t, e.sessionDir)
	e.fb.Harnesses = hs[1:]
	e.srv = New(Options{Backend: e.fb.AsBackend(), Registry: e.reg, Harnesses: hs, PPID: ppid})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := e.srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.cs = cs
	srv := e.srv
	t.Cleanup(func() { cs.Close(); srv.Close() })
}

func (e *env) stop() {
	e.cs.Close()
	if err := e.srv.Close(); err != nil {
		e.t.Fatal(err)
	}
}

func thread(id string) map[string]any { return map[string]any{"threadId": id} }

func (e *env) call(meta map[string]any, tool string, args any) (bool, map[string]any) {
	e.t.Helper()
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: meta, Name: tool, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: protocol error: %v", tool, err)
	}
	out := map[string]any{}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	} else if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			out["text"] = tc.Text
		}
	}
	return res.IsError, out
}

func (e *env) mustOK(meta map[string]any, tool string, args any) map[string]any {
	e.t.Helper()
	isErr, out := e.call(meta, tool, args)
	if isErr {
		e.t.Fatalf("%s %v: error result %v", tool, meta, out)
	}
	return out
}

func (e *env) mustErr(meta map[string]any, tool string, args any, code, reason string) {
	e.t.Helper()
	isErr, out := e.call(meta, tool, args)
	if !isErr || out["error"] != code || (reason != "" && out["reason"] != reason) {
		e.t.Fatalf("%s %v: got isError=%v %v, want %s/%s", tool, meta, isErr, out, code, reason)
	}
}

func (e *env) descriptor(agentID string) registry.Descriptor {
	e.t.Helper()
	ds, err := e.reg.List()
	if err != nil {
		e.t.Fatal(err)
	}
	for _, d := range ds {
		if d.AgentID == agentID {
			return d
		}
	}
	e.t.Fatalf("no live descriptor for %s", agentID)
	return registry.Descriptor{}
}

func codexID(tid string) string  { return identity.AgentID("codex", tid) }
func claudeID(sid string) string { return identity.AgentID("claude", sid) }

func sendArgs(to, text string) map[string]any { return map[string]any{"to": to, "text": text} }

func TestToolSurface(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	res, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var send *mcp.Tool
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if tl.Name == "send_message" {
			send = tl
		}
	}
	slices.Sort(names)
	want := []string{"ask", "claim", "list_agents", "receive_message", "release", "reply", "send_message"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools %v, want %v", names, want)
	}
	b, _ := json.Marshal(send.InputSchema)
	for _, s := range []string{`"enum":["inform","command","ask"]`, `"enum":["immediate","next_turn"]`, `"required":["to","text"]`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("send_message schema %s lacks %s", b, s)
		}
	}
}

func TestIdentityFromThreadID(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	e.mustOK(map[string]any{"threadId": "T-1", "sessionId": "S-1"}, "send_message", sendArgs("bob", "hi"))
	e.mustOK(map[string]any{"threadId": "T-1"}, "send_message", sendArgs("bob", "again"))
	calls := e.fb.Calls()
	if len(calls) != 2 || calls[0].From != codexID("T-1") || calls[1].From != codexID("T-1") {
		t.Fatalf("calls %+v, want both from %s", calls, codexID("T-1"))
	}
	if calls[0].Send.Kind != "inform" || calls[0].Send.Delivery != "immediate" {
		t.Errorf("defaults not applied: %+v", calls[0].Send)
	}
	d := e.descriptor(codexID("T-1"))
	if d.Harness != "codex" || d.SessionRef != "T-1" || d.HarnessPID != nil || d.Role != registry.RolePrimary {
		t.Errorf("descriptor %+v", d)
	}
}

func TestIdentityFromParentPID(t *testing.T) {
	e := newEnv(t, 4242, NewFakeBackend())
	writeClaudeSession(t, e.sessionDir, 4242, "claude-sess", "alpha", "/work/alpha")
	e.mustOK(nil, "send_message", sendArgs("bob", "hi"))
	calls := e.fb.Calls()
	if len(calls) != 1 || calls[0].From != claudeID("claude-sess") || calls[0].FromName != "alpha" {
		t.Fatalf("calls %+v", calls)
	}
	d := e.descriptor(claudeID("claude-sess"))
	if d.Harness != "claude" || d.Name != "alpha" || d.Cwd != "/work/alpha" || d.HarnessPID == nil || *d.HarnessPID != 4242 {
		t.Errorf("descriptor %+v", d)
	}
}

func TestUnboundServesOnlyListAgents(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	for _, meta := range []map[string]any{nil, {"sessionId": "S-1"}, {"threadId": ""}} {
		e.mustErr(meta, "send_message", sendArgs("bob", "hi"), CodeUnbound, "")
		e.mustErr(meta, "reply", map[string]any{"message_id": "m", "text": "x"}, CodeUnbound, "")
		e.mustErr(meta, "ask", sendArgs("bob", "q"), CodeUnbound, "")
		e.mustErr(meta, "receive_message", map[string]any{}, CodeUnbound, "")
		e.mustErr(meta, "claim", map[string]any{"name": "bob"}, CodeUnbound, "")
		e.mustErr(meta, "release", map[string]any{"name": "bob"}, CodeUnbound, "")
		out := e.mustOK(meta, "list_agents", map[string]any{})
		if _, ok := out["agents"]; !ok {
			t.Fatalf("list_agents %v", out)
		}
	}
	if c := e.fb.Calls(); len(c) != 0 {
		t.Fatalf("backend called for unbound: %+v", c)
	}
	if ds, _ := e.reg.List(); len(ds) != 0 {
		t.Fatalf("unbound call registered %+v", ds)
	}
}

// Test 27 core: one connection, two threads, concurrent calls, no cross-binding.
func TestPooledThreadsNeverCrossBind(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	threads := []string{"01a1a3aa-0001", "01a1a3aa-0002"}
	for _, th := range threads {
		e.mustOK(thread(th), "list_agents", map[string]any{})
	}
	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for i := 0; i < 20; i++ {
		for _, th := range threads {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: thread(th), Name: "send_message", Arguments: sendArgs("peer", th+"#"+itoa(i))})
				if err != nil || res.IsError {
					errs <- fmt.Sprintf("%s: %v %+v", th, err, res)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: sendArgs("peer", "unbound")})
			if err != nil || !res.IsError {
				errs <- fmt.Sprintf("unbound call served: %v %+v", err, res)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for s := range errs {
		t.Error(s)
	}
	calls := e.fb.Calls()
	if len(calls) != 40 {
		t.Fatalf("%d backend calls, want 40", len(calls))
	}
	for _, c := range calls {
		th, _, _ := strings.Cut(c.Send.Text, "#")
		if c.From != codexID(th) {
			t.Errorf("message %q from agent %s, want %s", c.Send.Text, c.From, codexID(th))
		}
	}
	ds, _ := e.reg.List()
	if len(ds) != 2 || ds[0].AgentID == ds[1].AgentID || ds[0].Name == ds[1].Name {
		t.Fatalf("descriptors %+v", ds)
	}
}

func TestNotReadyWhileBinding(t *testing.T) {
	fb := NewFakeBackend()
	entered, gate := make(chan struct{}), make(chan struct{})
	fb.Bind = func(ctx context.Context, a *Agent) (BindInfo, error) {
		if a.SessionRef() == "slow" {
			close(entered)
			<-gate
		}
		return BindInfo{A2AURL: "http://127.0.0.1:1"}, nil
	}
	e := newEnv(t, 999999, fb)
	done := make(chan bool)
	go func() {
		isErr, _ := e.call(thread("slow"), "send_message", sendArgs("bob", "first"))
		done <- isErr
	}()
	<-entered
	e.mustErr(thread("slow"), "send_message", sendArgs("bob", "second"), CodeNotReady, "binding_in_progress")
	e.mustOK(thread("fast"), "send_message", sendArgs("bob", "other thread"))
	close(gate)
	if isErr := <-done; isErr {
		t.Fatal("first call failed")
	}
	e.mustOK(thread("slow"), "send_message", sendArgs("bob", "third"))
	if d := e.descriptor(codexID("slow")); d.A2AURL != "http://127.0.0.1:1" {
		t.Errorf("binder info not published: %+v", d)
	}
}

func TestPassThroughTools(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	me := thread("T-1")
	out := e.mustOK(me, "ask", map[string]any{"to": "bob", "text": "q?", "deadline_seconds": 30})
	if out["message_id"] == "" || out["state"] != "accepted" {
		t.Fatalf("ask %v", out)
	}
	e.mustOK(me, "send_message", map[string]any{"to": "bob", "text": "do", "kind": "command", "delivery": "next_turn"})
	e.mustOK(me, "reply", map[string]any{"message_id": "m-1", "text": "answer"})
	e.fb.SetMailbox(codexID("T-1"), MailboxEntry{MessageID: "m-2", From: "bob", State: "bounced"})
	out = e.mustOK(me, "receive_message", map[string]any{"limit": 5})
	if ms, ok := out["messages"].([]any); !ok || len(ms) != 1 {
		t.Fatalf("receive_message %v", out)
	}
	if isErr, _ := e.call(me, "send_message", map[string]any{"to": "bob", "text": "x", "kind": "control"}); !isErr {
		t.Fatal("kind=control accepted")
	}
	c := e.fb.Calls()
	if c[0].Op != "ask" || c[0].Ask.DeadlineSeconds != 30 || c[1].Send.Kind != "command" || c[1].Send.Delivery != "next_turn" ||
		c[2].Op != "reply" || c[2].MessageID != "m-1" || c[3].Op != "mailbox" {
		t.Fatalf("calls %+v", c)
	}
	e.fb.SendErr = &Error{Code: "unreachable", Reason: "no_binding"}
	e.mustErr(me, "send_message", sendArgs("bob", "x"), "unreachable", "no_binding")
}

func listen(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
}

func rowsByID(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	rows := map[string]map[string]any{}
	list, _ := out["agents"].([]any)
	for _, r := range list {
		m := r.(map[string]any)
		rows[m["agent_id"].(string)] = m
	}
	return rows
}

func TestListAgentsDiscovery(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(
		Session{Harness: "codex", SessionRef: "T-cx", Name: "cx", Cwd: "/w/cx", Status: "loaded", Reachable: true},
		Session{Harness: "codex", SessionRef: "T-plain", Name: "plain-codex", Cwd: "/w/p", Status: "loaded"},
	)
	e := newEnv(t, 4242, fb)
	writeClaudeSession(t, e.sessionDir, 4242, "c-self", "alpha", "/w/alpha")
	writeClaudeSession(t, e.sessionDir, 5151, "c-other", "other", "/w/other")
	writeClaudeSession(t, e.sessionDir, 950000, "c-dead", "dead", "/w/dead")
	b, _ := os.ReadFile(filepath.Join(e.sessionDir, "5151.json"))
	sock := filepath.Join(e.sessionDir, "o.sock")
	os.WriteFile(filepath.Join(e.sessionDir, "5151.json"), []byte(strings.Replace(string(b), filepath.Join(e.sessionDir, "none.sock"), sock, 1)), 0o600)
	listen(t, sock)

	e.mustOK(thread("T-cx"), "list_agents", map[string]any{})
	rows := rowsByID(t, e.mustOK(nil, "list_agents", map[string]any{}))

	want := map[string]struct {
		name, harness        string
		crosstalk, reachable bool
		role                 any
	}{
		claudeID("c-self"):  {"alpha", "claude", true, false, "primary"},
		claudeID("c-other"): {"other", "claude", false, true, nil},
		codexID("T-cx"):     {"cx", "codex", true, true, "primary"},
		codexID("T-plain"):  {"plain-codex", "codex", false, false, nil},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows %v", rows)
	}
	for id, w := range want {
		r := rows[id]
		if r == nil || r["name"] != w.name || r["harness"] != w.harness || r["crosstalk"] != w.crosstalk || r["reachable"] != w.reachable || r["role"] != w.role {
			t.Errorf("row %s = %v, want %+v", id, r, w)
		}
		if _, ok := r["owner"]; !ok {
			t.Errorf("row %s has no owner field", id)
		}
	}
}

func TestNamingFallbackWhenHeld(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(
		Session{Harness: "codex", SessionRef: "01a1a3c0-a", Name: "dup", Cwd: "/w/proj"},
		Session{Harness: "codex", SessionRef: "01a1a3c0-b", Name: "dup", Cwd: "/w/proj"},
	)
	e := newEnv(t, 999999, fb)
	e.mustOK(thread("01a1a3c0-a"), "send_message", sendArgs("x", "1"))
	out := e.mustOK(thread("01a1a3c0-b"), "send_message", sendArgs("x", "2"))
	if n, _ := out["notice"].(string); !strings.Contains(n, "held") {
		t.Errorf("no held notice: %v", out)
	}
	if d := e.descriptor(codexID("01a1a3c0-a")); d.Name != "dup" {
		t.Errorf("first name %q", d.Name)
	}
	id := codexID("01a1a3c0-b")
	if d := e.descriptor(id); d.Name != "codex-proj-"+id[:8] {
		t.Errorf("fallback name %q", d.Name)
	}
	e.mustOK(thread("01a1a3c0-c"), "send_message", sendArgs("x", "3"))
	id = codexID("01a1a3c0-c")
	if d := e.descriptor(id); d.Name != "codex-root-"+id[:8] {
		t.Errorf("unnamed thread fallback %q", d.Name)
	}
}

func TestRenameUpdatesAliasOnly(t *testing.T) {
	e := newEnv(t, 4242, NewFakeBackend())
	writeClaudeSession(t, e.sessionDir, 4242, "c-1", "alpha", "/w/a")
	e.mustOK(nil, "send_message", sendArgs("x", "1"))
	id := claudeID("c-1")
	before := e.descriptor(id)

	writeClaudeSession(t, e.sessionDir, 4242, "c-1", "beta", "/w/a")
	e.mustOK(nil, "send_message", sendArgs("x", "2"))
	after := e.descriptor(id)
	if after.Name != "beta" || after.AgentID != before.AgentID || after.InstanceID != before.InstanceID ||
		after.UDSPath != before.UDSPath || after.SessionRef != before.SessionRef || after.StartedAt != before.StartedAt {
		t.Fatalf("rename changed more than the alias:\nbefore %+v\nafter  %+v", before, after)
	}
	if _, ok, _ := e.reg.Lookup("alpha"); ok {
		t.Error("old alias still resolves")
	}
	if _, err := os.Stat(journal.Path(e.stateDir, id)); err != nil {
		t.Errorf("journal path moved: %v", err)
	}
	if c := e.fb.Calls(); c[1].FromName != "beta" || c[1].From != id {
		t.Errorf("second send %+v", c[1])
	}

	other, err := registry.New(e.stateDir).Acquire(registry.Request{AgentID: "ffffffffffffffff", HarnessName: "gamma", Harness: "codex", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	writeClaudeSession(t, e.sessionDir, 4242, "c-1", "gamma", "/w/a")
	out := e.mustOK(nil, "send_message", sendArgs("x", "3"))
	if n, _ := out["notice"].(string); !strings.Contains(n, "held") {
		t.Errorf("no held notice: %v", out)
	}
	if d := e.descriptor(id); d.Name != "beta" {
		t.Errorf("alias moved onto a held name: %q", d.Name)
	}
}

func TestCloseRemovesOwnDescriptors(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	e.mustOK(thread("T-1"), "send_message", sendArgs("x", "1"))
	e.stop()
	if ds, _ := e.reg.List(); len(ds) != 0 {
		t.Fatalf("descriptors after close: %+v", ds)
	}
	if _, err := os.Stat(journal.Path(e.stateDir, codexID("T-1"))); err != nil {
		t.Fatalf("journal removed: %v", err)
	}
}

type timedOutCloser struct{ *FakeBackend }

func (timedOutCloser) CloseAgent(*Agent) error { return ErrCloseTimeout }

func TestCloseKeepsOwnershipWhenAgentCloseTimesOut(t *testing.T) {
	e := newEnv(t, 999999, NewFakeBackend())
	e.srv.backend = timedOutCloser{e.fb}
	e.mustOK(thread("T-1"), "send_message", sendArgs("x", "1"))
	id := codexID("T-1")
	e.cs.Close()
	if err := e.srv.Close(); !errors.Is(err, ErrCloseTimeout) {
		t.Fatalf("Close err = %v, want ErrCloseTimeout", err)
	}
	if d := e.descriptor(id); d.AgentID != id {
		t.Fatalf("descriptor %+v", d)
	}
	_, err := registry.New(e.stateDir).Acquire(registry.Request{AgentID: id, HarnessName: "other", Harness: "codex", Cwd: "/w"})
	if !errors.Is(err, registry.ErrAgentHeld) {
		t.Fatalf("Acquire err = %v, want ErrAgentHeld while a handler may still run", err)
	}
}

func TestRetireReleasesNameWhenCloseTimesOut(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(
		Session{Harness: "codex", SessionRef: "T-old", Name: "shared", Cwd: "/w"},
		Session{Harness: "codex", SessionRef: "T-new", Name: "shared", Cwd: "/w"},
	)
	e := newEnv(t, 999999, fb)
	e.srv.backend = timedOutCloser{e.fb}
	e.mustOK(thread("T-old"), "send_message", sendArgs("x", "1"))
	oldID := codexID("T-old")
	e.reg.Superseded = func(d registry.Descriptor) bool { return d.AgentID == oldID }

	out := e.mustOK(thread("T-new"), "send_message", sendArgs("x", "2"))
	if out["notice"] != nil {
		t.Errorf("notice %v", out["notice"])
	}
	if d := e.descriptor(codexID("T-new")); d.Name != "shared" {
		t.Fatalf("new holder %+v", d)
	}
	_, err := registry.New(e.stateDir).Acquire(registry.Request{AgentID: oldID, Harness: "codex", Cwd: "/w"})
	if !errors.Is(err, registry.ErrAgentHeld) {
		t.Fatalf("Acquire err = %v, want ErrAgentHeld while a handler may still run", err)
	}
	isErr, res := e.call(thread("T-old"), "send_message", sendArgs("x", "3"))
	if !isErr || res["reason"] != "agent_held" {
		t.Fatalf("resume during timeout: %v %v", isErr, res)
	}
}
