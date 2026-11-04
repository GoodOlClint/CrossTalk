package wire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/a2a"
	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/shim"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

type fakeCodex struct {
	*adapter.Fake
	mu      sync.Mutex
	threads []codex.LoadedThread
}

func (f *fakeCodex) LoadedThreads(context.Context, ...string) []codex.LoadedThread {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]codex.LoadedThread(nil), f.threads...)
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "wr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type node struct {
	t      *testing.T
	name   string
	thread string
	codex  *fakeCodex
	w      *Wire
	srv    *mcpserver.Server
	cs     *mcp.ClientSession
}

// newNode is one shim process serving the Codex thread T-<name>, with its
// own fake harness, sharing stateDir with its peers.
func newNode(t *testing.T, stateDir string, ppid int, name string) *node {
	t.Helper()
	th := "T-" + name
	fc := &fakeCodex{Fake: adapter.NewFake(adapter.Codex)}
	fc.threads = []codex.LoadedThread{{Endpoint: "fake://" + th, ID: th, Name: name, Cwd: "/work/" + name, Status: "idle",
		Session: adapter.SessionStatus{Loaded: true, Reachable: true}}}
	ch, err := claude.NewHarness(claude.HarnessOptions{SessionsDir: shortDir(t), Ingress: udsingress.NewIngress})
	if err != nil {
		t.Fatal(err)
	}
	hs := []harness.Harness{codex.NewHarness(codex.HarnessOptions{Adapter: fc, Directory: registry.New(stateDir)}), ch}
	w, err := New(shim.Options{StateDir: stateDir, PPID: ppid, Harnesses: hs})
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpserver.New(w.Shim().MCPOptions())
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := &node{t: t, name: name, thread: th, codex: fc, w: w, srv: srv, cs: cs}
	t.Cleanup(func() {
		cs.Close()
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
		if err := w.Shim().Close(); err != nil {
			t.Error(err)
		}
	})
	n.ok("receive_message", map[string]any{})
	return n
}

func (n *node) id() string { return identity.AgentID("codex", n.thread) }

func (n *node) call(tool string, args any) (bool, map[string]any) {
	n.t.Helper()
	res, err := n.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": n.thread}, Name: tool, Arguments: args})
	if err != nil {
		n.t.Fatalf("%s: protocol error: %v", tool, err)
	}
	out := map[string]any{}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	}
	return res.IsError, out
}

func (n *node) ok(tool string, args any) map[string]any {
	n.t.Helper()
	isErr, out := n.call(tool, args)
	if isErr {
		n.t.Fatalf("%s %s: error result %v", n.name, tool, out)
	}
	return out
}

func descriptor(t *testing.T, stateDir, agentID string) registry.Descriptor {
	t.Helper()
	ds, err := registry.New(stateDir).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.AgentID == agentID {
			return d
		}
	}
	t.Fatalf("no descriptor for %s", agentID)
	return registry.Descriptor{}
}

func outboundTask(t *testing.T, stateDir, agentID, messageID string) string {
	t.Helper()
	b, err := os.ReadFile(journal.Path(stateDir, agentID))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r journal.Record
		if json.Unmarshal([]byte(l), &r) == nil && r.Type == journal.TypeOutbound && r.MessageID == messageID && r.TaskID != "" {
			return r.TaskID
		}
	}
	t.Fatalf("no outbound task id for %s in %s's journal", messageID, agentID)
	return ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pair(t *testing.T) (string, *node, *node) {
	dir := shortDir(t)
	return dir, newNode(t, dir, 1, "alice"), newNode(t, dir, 2, "bob")
}

func TestSendMessageOverA2A(t *testing.T) {
	dir, alice, bob := pair(t)
	d := descriptor(t, dir, bob.id())
	if !strings.HasPrefix(d.A2AURL, "http://127.0.0.1:") || d.CredentialRef != bob.w.TokenPath(bob.id()) {
		t.Fatalf("bob descriptor a2a_url %q credential_ref %q", d.A2AURL, d.CredentialRef)
	}
	if fi, err := os.Stat(d.CredentialRef); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bob token %v %v", fi, err)
	}
	if fi, err := os.Stat(bob.w.PeerTokenPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("peer token %v %v", fi, err)
	}

	out := alice.ok("send_message", map[string]any{"to": "bob", "text": "hello bob"})
	if out["state"] != "accepted" {
		t.Fatalf("send %v", out)
	}
	ws := bob.codex.Writes()
	if len(ws) != 1 || ws[0].SessionRef != "T-bob" || ws[0].Message.From != "alice" || ws[0].Message.Parts[0].Text != "hello bob" || !ws[0].Delivery.VerifiedSender || ws[0].Message.Kind != msg.Inform {
		t.Fatalf("bob's harness got %+v", ws)
	}
	if len(alice.codex.Writes()) != 0 {
		t.Fatalf("alice's harness written: %+v", alice.codex.Writes())
	}
	outboundTask(t, dir, alice.id(), out["message_id"].(string))
}

func TestAskReplyCompletesTaskAndInformsAsker(t *testing.T) {
	dir, alice, bob := pair(t)
	ask := alice.ok("ask", map[string]any{"to": "bob", "text": "what is 2+2?"})
	if ask["state"] != "accepted" {
		t.Fatalf("ask %v", ask)
	}
	askID := ask["message_id"].(string)
	ws := bob.codex.Writes()
	if len(ws) != 1 || ws[0].Message.MessageID != askID || ws[0].Message.Kind != msg.Ask || ws[0].Message.From != "alice" {
		t.Fatalf("bob's harness got %+v", ws)
	}

	if rep := bob.ok("reply", map[string]any{"message_id": askID, "text": "4"}); rep["state"] != "accepted" {
		t.Fatalf("reply %v", rep)
	}
	aw := alice.codex.Writes()
	if len(aw) != 1 || aw[0].Message.From != "bob" || aw[0].Message.InReplyTo == nil || *aw[0].Message.InReplyTo != askID || aw[0].Message.Parts[0].Text != "4" {
		t.Fatalf("alice's harness got %+v", aw)
	}

	taskID := outboundTask(t, dir, alice.id(), askID)
	ad := descriptor(t, dir, alice.id())
	tok, err := readOwnToken(ad.CredentialRef)
	if err != nil {
		t.Fatal(err)
	}
	task, err := (&a2a.Client{Name: "alice"}).GetTask(context.Background(), descriptor(t, dir, bob.id()).A2AURL, tok, taskID)
	if err != nil || task.TaskState != sdk.TaskStateCompleted || task.Reply != "4" || task.MessageID != askID {
		t.Fatalf("ask task %+v %v", task, err)
	}
}

func TestWrongTokenRefused(t *testing.T) {
	dir, alice, bob := pair(t)
	bd := descriptor(t, dir, bob.id())
	m := msg.Message{MessageID: msg.NewUUIDv7(), Kind: msg.Inform, Delivery: msg.Immediate, Parts: []msg.Part{{Type: "text", Text: "forged"}}}
	if _, st, err := (&a2a.Client{Name: "alice"}).Send(context.Background(), bd.A2AURL, "not-alices-token", m, true); err == nil || st != adapter.Rejected {
		t.Fatalf("wrong token: %s %v", st, err)
	}

	forged := shortDir(t) + "/alice.token"
	if err := os.WriteFile(forged, []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	from := descriptor(t, dir, alice.id())
	from.CredentialRef = forged
	m.MessageID = msg.NewUUIDv7()
	if st, err := alice.w.Send(context.Background(), from, bd, m, true); err == nil || st != adapter.Rejected {
		t.Fatalf("forged credential_ref: %s %v", st, err)
	}
	if ws := bob.codex.Writes(); len(ws) != 0 {
		t.Fatalf("refused sends reached bob: %+v", ws)
	}
}

func TestClaimOverA2A(t *testing.T) {
	dir, alice, bob := pair(t)
	if out := alice.ok("claim", map[string]any{"name": "bob"}); out["owner"] != "alice" {
		t.Fatalf("claim %v", out)
	}
	d := descriptor(t, dir, bob.id())
	if d.Role != registry.RoleSubagent || d.Owner == nil || *d.Owner != "alice" {
		t.Fatalf("bob descriptor %+v", d)
	}
	waitFor(t, "bob told of claim", func() bool {
		ws := bob.codex.Writes()
		return len(ws) > 0 && strings.Contains(ws[len(ws)-1].Message.Parts[0].Text, "alice claimed you")
	})
	isErr, out := alice.call("claim", map[string]any{"name": "bob"})
	if !isErr || out["error"] != mcpserver.CodeRejected || out["reason"] != "already_claimed" {
		t.Fatalf("second claim %v %v", isErr, out)
	}
	peer, err := readOwnToken(bob.w.PeerTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	var ce *a2a.ClaimError
	if err := (&a2a.Client{}).Claim(context.Background(), d.A2AURL, peer, false); !errors.As(err, &ce) || ce.Reason != a2a.ReasonClaimSender {
		t.Fatalf("release by local-user: %v", err)
	}
	alice.ok("release", map[string]any{"name": "bob"})
	if d := descriptor(t, dir, bob.id()); d.Role != registry.RolePrimary || d.Owner != nil {
		t.Fatalf("bob after release %+v", d)
	}
}
