package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func harnessReq(ppid int) harness.RequestContext { return harness.RequestContext{ParentPID: ppid} }

// rig is a client on the shim's own transport over pipes.
type rig struct {
	t    *testing.T
	srv  *Server
	tr   *Transport
	in   io.WriteCloser
	out  *bufio.Reader
	done chan error
}

func startRig(t *testing.T, srv *Server, opts ...RunOption) *rig {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &rig{t: t, srv: srv, tr: NewTransport(inR, outW), in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() { r.done <- srv.Run(context.Background(), r.tr, opts...) }()
	t.Cleanup(func() { r.stop() })
	return r
}

func (r *rig) stop() {
	r.in.Close()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		r.t.Error("Run did not return after stdin closed")
	}
	r.done <- nil
}

func (r *rig) send(line string) {
	r.t.Helper()
	if _, err := io.WriteString(r.in, line+"\n"); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) frame() map[string]json.RawMessage {
	r.t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() { l, err := r.out.ReadString('\n'); ch <- res{l, err} }()
	select {
	case x := <-ch:
		if x.err != nil {
			r.t.Fatalf("reading a frame: %v", x.err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(x.line), &m); err != nil {
			r.t.Fatalf("frame %q: %v", x.line, err)
		}
		return m
	case <-time.After(5 * time.Second):
		r.t.Fatal("no frame within 5s")
		return nil
	}
}

func (r *rig) rpc(id int, method, params string) map[string]json.RawMessage {
	r.t.Helper()
	r.send(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":` + params + `}`)
	return r.frame()
}

func (r *rig) handshake() {
	r.t.Helper()
	res := r.rpc(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"rig-client","version":"9"}}`)
	if res["error"] != nil {
		r.t.Fatalf("initialize: %s", res["error"])
	}
	r.send(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	if res := r.rpc(2, "logging/setLevel", `{"level":"warning"}`); res["error"] != nil {
		r.t.Fatalf("setLevel: %s", res["error"])
	}
}

func (r *rig) quiet() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.tr.Conn().WaitQuiescent(ctx); err != nil {
		r.t.Fatalf("not quiescent: %v", err)
	}
}

func newRigServer(t *testing.T, stateDir, sessionDir string, ppid int, fb *FakeBackend) *Server {
	t.Helper()
	hs := testHarnesses(t, sessionDir)
	fb.Harnesses = hs[1:]
	srv := New(Options{Backend: fb.AsBackend(), Registry: registry.New(stateDir), Harnesses: hs, PPID: ppid})
	t.Cleanup(func() { srv.Close() })
	return srv
}

// roundTrip sends h through the unlinked file the real handoff uses.
func roundTrip(t *testing.T, h *Handoff) *Handoff {
	t.Helper()
	f, err := WriteHandoff(h)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := ReadHandoff(f)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestResumeAcceptsCallWithoutInitialize(t *testing.T) {
	stateDir, sessions := t.TempDir(), t.TempDir()
	r1 := startRig(t, newRigServer(t, stateDir, sessions, 999999, NewFakeBackend()))
	r1.handshake()
	r1.quiet()
	want := r1.tr.Conn().State()
	if want.ProtocolVersion == "" || !want.Initialized || !strings.Contains(string(want.SetLevelParams), "warning") {
		t.Fatalf("handshake not captured: %+v", want)
	}
	ho := roundTrip(t, r1.srv.Handoff(want, BinaryIdentity{}))
	r1.stop()

	r2 := startRig(t, newRigServer(t, stateDir, sessions, 999999, NewFakeBackend()), WithHandoff(ho))
	if first := r2.frame(); string(first["method"]) != `"notifications/tools/list_changed"` {
		t.Fatalf("first frame = %v, want notifications/tools/list_changed", first)
	}
	res := r2.rpc(7, "tools/call", `{"name":"list_agents","arguments":{}}`)
	if res["error"] != nil || res["result"] == nil {
		t.Fatalf("tools/call without initialize: %v", res)
	}
	if got := r2.tr.Conn().State(); got.ProtocolVersion != want.ProtocolVersion || string(got.SetLevelParams) != string(want.SetLevelParams) || !got.Initialized {
		t.Fatalf("state after resume %+v, want %+v", got, want)
	}
	var ip *struct{ Name string }
	for ss := range r2.srv.MCP().Sessions() {
		if p := ss.InitializeParams(); p != nil && p.ClientInfo != nil {
			ip = &struct{ Name string }{p.ClientInfo.Name}
		}
	}
	if ip == nil || ip.Name != "rig-client" {
		t.Fatalf("carried client info = %+v", ip)
	}
}

func TestNoHandoffRejectsCallBeforeInitialize(t *testing.T) {
	r := startRig(t, newRigServer(t, t.TempDir(), t.TempDir(), 999999, NewFakeBackend()))
	res := r.rpc(7, "tools/call", `{"name":"list_agents","arguments":{}}`)
	if res["error"] == nil || !strings.Contains(string(res["error"]), "invalid during session initialization") {
		t.Fatalf("call before initialize = %v, want the initialization error", res)
	}
}

func TestResumeKeepsViaCallAndNotices(t *testing.T) {
	stateDir, sessions := t.TempDir(), t.TempDir()
	writeClaudeSession(t, sessions, 4242, "claude-sess", "alpha", "/work/alpha")
	id := claudeID("claude-sess")

	srv1 := newRigServer(t, stateDir, sessions, 4242, NewFakeBackend())
	r1 := startRig(t, srv1)
	r1.handshake()
	if res := r1.rpc(3, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"hi"}}`); res["error"] != nil {
		t.Fatalf("bound call: %v", res)
	}
	srv1.mu.Lock()
	a := srv1.agents[id]
	srv1.mu.Unlock()
	a.holdNotice("held across the refresh")
	r1.quiet()
	ho := roundTrip(t, srv1.Handoff(r1.tr.Conn().State(), BinaryIdentity{}))
	r1.stop()

	srv2 := newRigServer(t, stateDir, sessions, 4242, NewFakeBackend())
	r2 := startRig(t, srv2, WithHandoff(ho))
	r2.frame()
	retired, err := srv2.RetireSession(context.Background(), harnessReq(4242))
	if err != nil || retired {
		t.Fatalf("Claude poll retired a call-bound agent after refresh: retired=%v err=%v", retired, err)
	}
	res := r2.rpc(4, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"again"}}`)
	if res["error"] != nil || !strings.Contains(string(res["result"]), "held across the refresh") {
		t.Fatalf("held notice missing from the next tool result: %s", res["result"])
	}
}

func TestHandoffReadRejectsBadDocuments(t *testing.T) {
	ok, err := json.Marshal(&Handoff{RefreshHandoff: HandoffFormat, Agents: []AgentHandoff{{ID: "a", Harness: "claude", SessionRef: "s"}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"unknown version": `{"refresh_handoff":99}`,
		"missing version": `{"agents":[]}`,
		"truncated":       string(ok[:len(ok)-3]),
		"not json":        `refresh`,
		"agent no id":     `{"refresh_handoff":1,"agents":[{"harness":"claude","session_ref":"s"}]}`,
		"bad session":     `{"refresh_handoff":1,"session":{"initialize_params":"nope"}}`,
	} {
		if _, err := ReadHandoff(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ReadHandoff(strings.NewReader(string(ok))); err != nil {
		t.Errorf("valid document: %v", err)
	}
}

func TestHandoffFromEnvUnsetsAndStartsNormallyWithoutIt(t *testing.T) {
	t.Setenv(HandoffFDEnv, "")
	os.Unsetenv(HandoffFDEnv)
	if h, err := HandoffFromEnv(); h != nil || err != nil {
		t.Fatalf("no variable: %v %v", h, err)
	}
	t.Setenv(HandoffFDEnv, "notanumber")
	if h, err := HandoffFromEnv(); h != nil || err == nil {
		t.Fatalf("bad variable: %v %v", h, err)
	}
	if _, ok := os.LookupEnv(HandoffFDEnv); ok {
		t.Fatal("variable left set for children")
	}
}

func TestWriteHandoffRefusesWhatTheReaderRefuses(t *testing.T) {
	big := &Handoff{RefreshHandoff: HandoffFormat, Agents: []AgentHandoff{{ID: "a", Harness: "claude", SessionRef: strings.Repeat("s", maxHandoff)}}}
	if f, err := WriteHandoff(big); err == nil {
		f.Close()
		t.Fatal("wrote a handoff larger than ReadHandoff accepts")
	}
}

func TestHandoffFromEnvAcceptsOnlyAPrivateUnlinkedFile(t *testing.T) {
	via := func(f *os.File) (*Handoff, error) {
		fd, err := syscall.Dup(int(f.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(HandoffFDEnv, itoa(fd))
		return HandoffFromEnv()
	}
	good, err := WriteHandoff(&Handoff{RefreshHandoff: HandoffFormat})
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	if h, err := via(good); err != nil || h == nil {
		t.Fatalf("unlinked private file: %v %v", h, err)
	}
	linked, err := os.CreateTemp(t.TempDir(), "h")
	if err != nil {
		t.Fatal(err)
	}
	defer linked.Close()
	linked.WriteString(`{"refresh_handoff":1}`)
	linked.Seek(0, io.SeekStart)
	if _, err := via(linked); err == nil {
		t.Fatal("accepted a file that still has a name")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	if _, err := via(pr); err == nil {
		t.Fatal("accepted a pipe")
	}
}

func TestHandoffCarriesCwdAndResumeUsesLiveParent(t *testing.T) {
	stateDir, sessions := t.TempDir(), t.TempDir()
	writeClaudeSession(t, sessions, 4242, "claude-sess", "alpha", "/work/alpha")
	srv1 := newRigServer(t, stateDir, sessions, 4242, NewFakeBackend())
	r1 := startRig(t, srv1)
	r1.handshake()
	r1.rpc(3, "tools/call", `{"name":"send_message","arguments":{"to":"bob","text":"hi"}}`)
	r1.quiet()
	ho := srv1.Handoff(r1.tr.Conn().State(), BinaryIdentity{})
	if len(ho.Agents) != 1 || ho.Agents[0].Cwd != "/work/alpha" {
		t.Fatalf("agents %+v, want the registered cwd carried", ho.Agents)
	}
	r1.stop()
	ho.Agents[0].Req.ParentPID = 1
	srv2 := newRigServer(t, stateDir, sessions, 4242, NewFakeBackend())
	srv2.Resume(context.Background(), ho)
	if srv2.local(ho.Agents[0].ID) == nil {
		t.Fatal("agent not resumed from the live parent when the saved parent pid differs")
	}
}
