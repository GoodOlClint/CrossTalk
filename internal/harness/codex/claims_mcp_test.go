package codex

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type mcpEnv struct {
	reg *registry.Registry
	fb  *mcpserver.FakeBackend
	srv *mcpserver.Server
	cs  *mcp.ClientSession
}

func newMCPEnv(t *testing.T, stateDir string) *mcpEnv {
	t.Helper()
	fake := adapter.NewFake(adapter.Codex)
	h := NewHarness(HarnessOptions{Adapter: threadsFake{fake}})
	reg := registry.New(stateDir)
	fb := mcpserver.NewFakeBackend()
	srv := mcpserver.New(mcpserver.Options{Backend: fb.AsBackend(), Registry: reg, Harnesses: []harness.Harness{h}, PPID: parentPID})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); srv.Close() })
	return &mcpEnv{reg: reg, fb: fb, srv: srv, cs: cs}
}

type threadsFake struct{ *adapter.Fake }

func (threadsFake) LoadedThreads(context.Context, ...string) []LoadedThread { return nil }

func codexAgentID(th string) string { return identity.AgentID("codex", th) }

func TestClaims_TwoThreadsOneShimConcurrentCallsAndRecheckRevokesOne_ExpectNoCrossBindNoCrossRevoke(t *testing.T) {
	e := newMCPEnv(t, t.TempDir())
	sys := newFakeSys()
	sys.own("T1")
	sys.own("T2")
	c := newTestClaims(t, sys, e.srv)
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); err != nil {
		t.Fatal(err)
	}
	cl := claim("T2")
	cl.Source = "clear"
	if err := c.AcceptDurable(context.Background(), cl); err != nil {
		t.Fatal(err)
	}
	for _, th := range []string{"T1", "T2"} {
		res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": th}, Name: "list_agents", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", th, err, res)
		}
	}
	sys.set("T2", false)

	var wg sync.WaitGroup
	errs := make(chan string, 100)
	var revoked []string
	wg.Add(1)
	go func() {
		defer wg.Done()
		revoked = c.Recheck()
	}()
	for i := range 20 {
		for _, th := range []string{"T1", "T2"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{
					Meta: map[string]any{"threadId": th}, Name: "send_message",
					Arguments: map[string]any{"to": "peer", "text": fmt.Sprintf("%s#%d", th, i)},
				})
				if err != nil || res.IsError {
					errs <- fmt.Sprintf("%s: %v %+v", th, err, res)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for s := range errs {
		t.Error(s)
	}
	if !slices.Equal(revoked, []string{"T2"}) || !c.Bound("T1") || c.Bound("T2") {
		t.Fatalf("revoked %v; T1 bound %v, T2 bound %v", revoked, c.Bound("T1"), c.Bound("T2"))
	}
	calls := e.fb.Calls()
	if len(calls) != 40 {
		t.Fatalf("%d backend calls, want 40", len(calls))
	}
	for _, call := range calls {
		th, _, _ := strings.Cut(call.Send.Text, "#")
		if call.From != codexAgentID(th) {
			t.Errorf("message %q from %s, want %s", call.Send.Text, call.From, codexAgentID(th))
		}
	}
	ds, _ := e.reg.List()
	if len(ds) != 2 || ds[0].AgentID == ds[1].AgentID {
		t.Fatalf("descriptors %+v, want one per thread", ds)
	}
}

func TestClaims_RefireThroughRealServer_ExpectOneAgent(t *testing.T) {
	e := newMCPEnv(t, t.TempDir())
	sys := newFakeSys()
	sys.own("T1")
	c := newTestClaims(t, sys, e.srv)
	for _, src := range []string{"startup", "resume", "clear", "compact"} {
		cl := claim("T1")
		cl.Source = src
		if err := c.AcceptLive(context.Background(), hookPID, cl); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
	}
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": "T1"}, Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	ds, _ := e.reg.List()
	if len(ds) != 1 || ds[0].AgentID != codexAgentID("T1") {
		t.Fatalf("descriptors %+v, want one", ds)
	}
}

func TestClaims_UnprovenClaims_ExpectNoAgentNoDescriptorNoRebind(t *testing.T) {
	e := newMCPEnv(t, t.TempDir())
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": "T-existing"}, Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	before, _ := e.reg.List()
	sys := newFakeSys()
	sys.set("T1", false)
	sys.set("T-existing", true, []int{outsider})
	c := newTestClaims(t, sys, e.srv)
	cases := []func() error{
		func() error { return c.AcceptLive(context.Background(), outsider, claim("T1")) },
		func() error { return c.AcceptDurable(context.Background(), claim("T1")) },
		func() error { return c.AcceptLive(context.Background(), hookPID, claim("T-existing")) },
		func() error {
			return c.AcceptDurable(context.Background(), Claim{SessionID: "T-existing", Cwd: "/forged"})
		},
	}
	for i, accept := range cases {
		if err := accept(); err == nil {
			t.Fatalf("case %d: unproven claim accepted", i)
		}
	}
	after, _ := e.reg.List()
	if len(after) != 1 || len(before) != 1 || after[0].InstanceID != before[0].InstanceID || after[0].Cwd != before[0].Cwd {
		t.Fatalf("descriptors before %+v after %+v", before, after)
	}
	if len(c.Threads()) != 0 {
		t.Fatalf("claims %v", c.Threads())
	}
}

func TestClaims_SessionHeldByAnotherLiveShimRealRegistry_ExpectIgnored(t *testing.T) {
	dir := t.TempDir()
	other := newMCPEnv(t, dir)
	res, err := other.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": "T1"}, Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	before, _ := other.reg.List()
	e := newMCPEnv(t, dir)
	sys := newFakeSys()
	sys.own("T1")
	c := newTestClaims(t, sys, e.srv)
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); err == nil || !strings.Contains(err.Error(), "agent_held") {
		t.Fatalf("err %v, want agent_held", err)
	}
	if c.Bound("T1") {
		t.Fatal("claim kept")
	}
	after, _ := e.reg.List()
	if len(after) != 1 || after[0].InstanceID != before[0].InstanceID {
		t.Fatalf("record changed: %+v -> %+v", before, after)
	}
}

func TestRecheck_RevokedClaimOnlyThread_ExpectRetiredAndNewOwnerRegisters(t *testing.T) {
	dir := t.TempDir()
	a := newMCPEnv(t, dir)
	sysA := newFakeSys()
	sysA.own("T1")
	sysA.own("T2")
	ca := newTestClaims(t, sysA, a.srv)
	for _, th := range []string{"T1", "T2"} {
		if err := ca.AcceptLive(context.Background(), hookPID, claim(th)); err != nil {
			t.Fatal(err)
		}
	}
	sysA.set("T1", false)
	if got := ca.Recheck(); !slices.Equal(got, []string{"T1"}) {
		t.Fatalf("revoked %v, want [T1]", got)
	}
	ds, _ := a.reg.List()
	if len(ds) != 1 || ds[0].AgentID != codexAgentID("T2") {
		t.Fatalf("descriptors after revoke %+v, want only T2", ds)
	}

	b := newMCPEnv(t, dir)
	sysB := newFakeSys()
	sysB.own("T1")
	cb := newTestClaims(t, sysB, b.srv)
	if err := cb.AcceptLive(context.Background(), hookPID, claim("T1")); err != nil {
		t.Fatalf("new owner: %v", err)
	}
	ds, _ = b.reg.List()
	if len(ds) != 2 {
		t.Fatalf("descriptors %+v, want T1 under the new owner and T2", ds)
	}
	if !ca.Bound("T2") {
		t.Fatal("T2 claim revoked by T1's transfer")
	}
}

func TestRecheck_RevokedThreadBoundByToolsCall_ExpectAgentKept(t *testing.T) {
	e := newMCPEnv(t, t.TempDir())
	sys := newFakeSys()
	sys.own("T1")
	c := newTestClaims(t, sys, e.srv)
	if err := c.AcceptLive(context.Background(), hookPID, claim("T1")); err != nil {
		t.Fatal(err)
	}
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Meta: map[string]any{"threadId": "T1"}, Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	sys.set("T1", false)
	if got := c.Recheck(); !slices.Equal(got, []string{"T1"}) {
		t.Fatalf("revoked %v, want [T1]", got)
	}
	if ds, _ := e.reg.List(); len(ds) != 1 || ds[0].AgentID != codexAgentID("T1") {
		t.Fatalf("descriptors %+v, want T1 kept", ds)
	}
}
