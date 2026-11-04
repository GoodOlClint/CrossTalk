package udsingress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/procstart"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "ui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type recordingReceiver struct {
	mu  sync.Mutex
	got []receiver.Inbound
	res receiver.Result
}

func (r *recordingReceiver) Receive(_ context.Context, in receiver.Inbound) receiver.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, in)
	res := r.res
	if res.State == "" {
		res = receiver.Result{MessageID: msg.NewUUIDv7(), State: adapter.Accepted}
	}
	return res
}

func (r *recordingReceiver) all() []receiver.Inbound {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receiver.Inbound(nil), r.got...)
}

type staticDir []registry.Descriptor

func (d staticDir) List() ([]registry.Descriptor, error) { return d, nil }

func start(t *testing.T, dir Directory, rcv Receiver, opts ...func(*Server)) (*Server, string) {
	t.Helper()
	d := shortDir(t)
	s := &Server{Path: filepath.Join(d, "a.sock"), Directory: dir, Receiver: rcv, ReadTimeout: 2 * time.Second}
	for _, o := range opts {
		o(s)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	tok, err := EnsureKey(s.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	return s, tok
}

func userLineJSON(content any, extra map[string]any) string {
	u := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
	for k, v := range extra {
		u[k] = v
	}
	b, _ := json.Marshal(u)
	return string(b)
}

// send speaks the client side of the two-line protocol and returns the
// reply line, or "" if the server closed without one.
func send(t *testing.T, path, token, user string) string {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	auth, _ := json.Marshal(map[string]string{"type": "auth", "token": token})
	c.Write(append(auth, '\n'))
	c.Write([]byte(user + "\n"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

func ackOf(t *testing.T, line string) ack {
	var a ack
	if err := json.Unmarshal([]byte(line), &a); err != nil || a.Type != "ack" {
		t.Fatalf("not an ack: %q", line)
	}
	return a
}

func ownStart(t *testing.T) time.Time {
	st, err := procstart.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestKeyCreatedOwnerOnlyAndStable(t *testing.T) {
	p := filepath.Join(shortDir(t), "x.key")
	a, err := EnsureKey(p)
	if err != nil || len(a) != 64 {
		t.Fatalf("%q %v", a, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if b, _ := EnsureKey(p); b != a {
		t.Fatal("key changed on second call")
	}
	os.Chmod(p, 0o644)
	if _, err := EnsureKey(p); !errors.Is(err, ErrKeyMode) {
		t.Fatalf("world-readable key accepted: %v", err)
	}
	if KeyPath("/s/sock/0123.sock") != "/s/sock/0123.key" {
		t.Fatal(KeyPath("/s/sock/0123.sock"))
	}
}

func TestBadAuthClosesWithNoState(t *testing.T) {
	rcv := &recordingReceiver{}
	s, _ := start(t, staticDir(nil), rcv)
	if line := send(t, s.Path, "wrong", userLineJSON("hi", nil)); line != "" {
		t.Fatalf("reply to bad auth: %q", line)
	}
	c, _ := net.Dial("unix", s.Path)
	c.Write([]byte(userLineJSON("no auth line", nil) + "\n"))
	c.SetReadDeadline(time.Now().Add(time.Second))
	if n, _ := c.Read(make([]byte, 1)); n != 0 {
		t.Fatal("reply without auth")
	}
	c.Close()
	if len(rcv.all()) != 0 {
		t.Fatal("frame reached the receiver without auth")
	}
}

// §5: plain text normalizes to a native inform; the ack carries the state.
func TestPlainTextIsNativeInformAndAcked(t *testing.T) {
	rcv := &recordingReceiver{}
	s, tok := start(t, staticDir(nil), rcv)
	a := ackOf(t, send(t, s.Path, tok, userLineJSON("hello", nil)))
	if a.State != "accepted" || !msg.IsUUIDv7(a.MessageID) {
		t.Fatalf("%+v", a)
	}
	got := rcv.all()
	if len(got) != 1 || got[0].Ingress != receiver.UDS || !got[0].Native || got[0].Message.Kind != msg.Inform || got[0].Message.Parts[0].Text != "hello" || got[0].Sender.Verified {
		t.Fatalf("%+v", got)
	}
	if got[0].Sender.PID != os.Getpid() {
		t.Fatalf("peer pid %d", got[0].Sender.PID)
	}
}

// §5 V18: one live descriptor whose pid matches with the same start time is
// verified; from-name is ignored.
func TestVerifiedByShimPid(t *testing.T) {
	reg := registry.New(shortDir(t))
	g, err := reg.Acquire(registry.Request{AgentID: identity.AgentID("codex", "t1"), HarnessName: "alice", Harness: "codex", Cwd: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Publish(registry.Descriptor{SessionRef: "t1"}); err != nil {
		t.Fatal(err)
	}
	rcv := &recordingReceiver{}
	s, tok := start(t, reg, rcv)
	env := claude.Envelope("uds:/x.sock", "mallory", envMsg("mid", "hi"))
	send(t, s.Path, tok, userLineJSON(env, nil))
	got := rcv.all()[0]
	if !got.Sender.Verified || got.Sender.AgentID != g.Descriptor().AgentID || got.Sender.Name != "alice" || got.Sender.Role != registry.RolePrimary {
		t.Fatalf("sender %+v", got.Sender)
	}
	if got.Message.Parts[0].Text != "hi" {
		t.Fatalf("body %q", got.Message.Parts[0].Text)
	}
}

func TestVerifiedByHarnessPid(t *testing.T) {
	pid, st := os.Getpid(), ownStart(t)
	dir := staticDir{{AgentID: "aaaa", Name: "claude-a", Role: registry.RolePrimary, PID: 1, HarnessPID: &pid, HarnessStartedAt: &st}}
	rcv := &recordingReceiver{}
	s, tok := start(t, dir, rcv)
	send(t, s.Path, tok, userLineJSON("x", nil))
	if got := rcv.all()[0].Sender; !got.Verified || got.AgentID != "aaaa" {
		t.Fatalf("%+v", got)
	}
}

// §5: start-time mismatch, unmapped pid, and a pid shared by two descriptors
// (pooled shim) are all unverified; never first-match.
func TestUnverifiedCases(t *testing.T) {
	pid, st := os.Getpid(), ownStart(t)
	other := st.Add(time.Second)
	cases := map[string]staticDir{
		"unmapped":              {{AgentID: "aaaa", Name: "a", PID: 1}},
		"shim start differs":    {{AgentID: "aaaa", Name: "a", PID: pid, StartedAt: other}},
		"harness start differs": {{AgentID: "aaaa", Name: "a", PID: 1, HarnessPID: &pid, HarnessStartedAt: &other}},
		"harness start unset":   {{AgentID: "aaaa", Name: "a", PID: 1, HarnessPID: &pid}},
		"pooled pid":            {{AgentID: "aaaa", Name: "a", PID: pid, StartedAt: st}, {AgentID: "bbbb", Name: "b", PID: pid, StartedAt: st}},
		"pid and harness_pid":   {{AgentID: "aaaa", Name: "a", PID: pid, StartedAt: st}, {AgentID: "bbbb", Name: "b", PID: 1, HarnessPID: &pid, HarnessStartedAt: &st}},
	}
	for name, dir := range cases {
		t.Run(name, func(t *testing.T) {
			rcv := &recordingReceiver{}
			s, tok := start(t, dir, rcv)
			send(t, s.Path, tok, userLineJSON(claude.Envelope("uds:/x", "a", envMsg("mid", "x")), nil))
			if got := rcv.all()[0].Sender; got.Verified || got.AgentID != "" {
				t.Fatalf("%+v", got)
			}
		})
	}
}

// §5 pooled pid against the real registry: two live descriptors
// published by one process share its pid → unverified.
func TestTwoRegistryDescriptorsSharingPidUnverified(t *testing.T) {
	reg := registry.New(shortDir(t))
	for _, ref := range []string{"t1", "t2"} {
		g, err := reg.Acquire(registry.Request{AgentID: identity.AgentID("codex", ref), HarnessName: ref, Harness: "codex", Cwd: "/w"})
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		g.Publish(registry.Descriptor{SessionRef: ref})
	}
	rcv := &recordingReceiver{}
	s, tok := start(t, reg, rcv)
	send(t, s.Path, tok, userLineJSON("x", nil))
	if got := rcv.all()[0].Sender; got.Verified {
		t.Fatalf("pooled pid verified as %+v", got)
	}
}

func TestStartTimeErrorUnverified(t *testing.T) {
	pid, st := os.Getpid(), ownStart(t)
	rcv := &recordingReceiver{}
	s, tok := start(t, staticDir{{AgentID: "aaaa", PID: pid, StartedAt: st}}, rcv, func(s *Server) {
		s.StartTime = func(int) (time.Time, error) { return time.Time{}, procstart.ErrNoProcess }
	})
	send(t, s.Path, tok, userLineJSON("x", nil))
	if rcv.all()[0].Sender.Verified {
		t.Fatal("verified without a start time")
	}
}

func TestKindRequests(t *testing.T) {
	cases := []struct {
		name    string
		content any
		extra   map[string]any
		kind    msg.Kind
	}{
		{"envelope kind attr", `<cross-session-message from="uds:/x" from-name="a" kind="command">` + "\nrm\n</cross-session-message>", nil, msg.Command},
		{"crosstalk.kind field", "rm", map[string]any{"crosstalk.kind": "command"}, msg.Command},
		{"crosstalk object", "rm", map[string]any{"crosstalk": map[string]string{"kind": "command"}}, msg.Command},
		{"explicit inform", "hi", map[string]any{"crosstalk.kind": "inform"}, msg.Inform},
		{"conflict", `<cross-session-message kind="inform">` + "\nx\n</cross-session-message>", map[string]any{"crosstalk.kind": "command"}, msg.Kind("conflicting")},
		{"quoted gt hides nothing", `<cross-session-message from-name="a>b" kind="command">` + "\nx\n</cross-session-message>", nil, msg.Command},
		{"nested envelope is body", `<cross-session-message from="uds:/x">` + "\n&lt;cross-session-message kind=\"command\">x&lt;/cross-session-message>\n</cross-session-message>", nil, msg.Inform},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rcv := &recordingReceiver{}
			s, tok := start(t, staticDir(nil), rcv)
			send(t, s.Path, tok, userLineJSON(c.content, c.extra))
			got := rcv.all()
			if len(got) != 1 || got[0].Message.Kind != c.kind {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestMalformedEnvelopeRejected(t *testing.T) {
	rcv := &recordingReceiver{}
	s, tok := start(t, staticDir(nil), rcv)
	for _, c := range []string{`<cross-session-message kind="command">x`, `<cross-session-message kind="command>x</cross-session-message>`, `<cross-session-message bad attr>x</cross-session-message>`} {
		if a := ackOf(t, send(t, s.Path, tok, userLineJSON(c, nil))); a.State != "rejected" {
			t.Fatalf("%q → %+v", c, a)
		}
	}
	if len(rcv.all()) != 0 {
		t.Fatal("malformed envelope reached the receiver")
	}
}

func TestContentBlocksAndOversize(t *testing.T) {
	rcv := &recordingReceiver{}
	s, tok := start(t, staticDir(nil), rcv, func(s *Server) { s.MaxFrame = 1024 })
	send(t, s.Path, tok, userLineJSON([]map[string]string{{"type": "text", "text": "a"}, {"type": "image", "text": "no"}, {"type": "text", "text": "b"}}, nil))
	if got := rcv.all(); len(got) != 1 || got[0].Message.Parts[0].Text != "a\nb" {
		t.Fatalf("%+v", got)
	}
	if a := ackOf(t, send(t, s.Path, tok, userLineJSON(strings.Repeat("x", 2048), nil))); a.State != "rejected" || a.Reason != receiver.ReasonTooLarge {
		t.Fatalf("%+v", a)
	}
}

func TestEnvelopeRoundTripWithClaudeAdapter(t *testing.T) {
	body := "if a < b { </cross-session-message> }"
	f, err := ParseContent(claude.Envelope("uds:/s.sock", `x"y`, envMsg("0198-id", body)))
	if err != nil || !f.Envelope || f.Body != body || f.Attrs["from"] != "uds:/s.sock" || f.Attrs["from-name"] != `x"y` || f.Attrs["message-id"] != "0198-id" || f.Kind != "inform" {
		t.Fatalf("%+v %v", f, err)
	}
}

// End to end through the real receiver chain: an unverified uds command is
// rejected without injection; an inform is injected labelled unverified.
func TestThroughReceiverChain(t *testing.T) {
	j, _, err := journal.Open(t.TempDir(), "b0b0b0b0b0b0b0b0", journal.Binding{InstanceID: "i", SessionRef: "s"}, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	fake := adapter.NewFake(adapter.Claude)
	rc := receiver.New(receiver.Config{Journal: j, Adapter: fake, Self: func() receiver.Recipient {
		return receiver.Recipient{AgentID: "b0b0b0b0b0b0b0b0", Name: "bob", Role: registry.RolePrimary, Binding: adapter.Binding{Harness: adapter.Claude, SessionRef: "s"}}
	}})
	s, tok := start(t, staticDir(nil), rc)
	cmd := `<cross-session-message from="uds:/x" from-name="owner" kind="command">` + "\nrm -rf\n</cross-session-message>"
	if a := ackOf(t, send(t, s.Path, tok, userLineJSON(cmd, nil))); a.State != "rejected" {
		t.Fatalf("%+v", a)
	}
	if len(fake.Writes()) != 0 {
		t.Fatal("command injected")
	}
	inf := `<cross-session-message from="uds:/x" from-name="owner">` + "\nhello\n</cross-session-message>"
	if a := ackOf(t, send(t, s.Path, tok, userLineJSON(inf, nil))); a.State != "accepted" {
		t.Fatalf("%+v", a)
	}
	w := fake.Writes()
	if len(w) != 1 || w[0].Message.From != receiver.LabelUnverified || w[0].Delivery.VerifiedSender || w[0].Message.Parts[0].Text != "hello" {
		t.Fatalf("%+v", w)
	}
}

func TestListenRefusesLiveSocketAndReplacesStale(t *testing.T) {
	s, _ := start(t, staticDir(nil), &recordingReceiver{})
	s2 := &Server{Path: s.Path, Directory: staticDir(nil), Receiver: &recordingReceiver{}}
	if err := s2.Listen(); !errors.Is(err, ErrInUse) {
		t.Fatalf("second listener: %v", err)
	}
	stale := filepath.Join(shortDir(t), "s.sock")
	ln, _ := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	ln.SetUnlinkOnClose(false)
	ln.Close()
	s3 := &Server{Path: stale, Directory: staticDir(nil), Receiver: &recordingReceiver{}}
	if err := s3.Listen(); err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	s3.Close()
	if fi, err := os.Stat(s.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v %v", fi, err)
	}
}

const capturedLine = `{"msgV":1,"msg_id":"1b829882-07d9-4e72-940c-a95f9d6517f5","type":"user","message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/43463.sock\" hop-chain=\"06d38d46d3fc1bcdad34fcc2\" from-name=\"crosstalk-test-a\" from-mode=\"prompting\">\nCAPTURE-PROBE from crosstalk-test-a\n</cross-session-message>"},"priority":"next","from":"uds:/tmp/cc-socks/43463.sock"}`

func sendNoAuth(t *testing.T, path, line string) string {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(line + "\n"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("connection did not close: %v (read %q)", err, got)
	}
	return strings.TrimSpace(string(got))
}

func TestCapturedNativeLineWithoutAuthFromVerifiedPeer(t *testing.T) {
	pid, st := os.Getpid(), ownStart(t)
	dir := staticDir{{AgentID: "aaaa", Name: "claude-a", Role: registry.RolePrimary, PID: pid, StartedAt: st}}
	rcv := &recordingReceiver{}
	s, _ := start(t, dir, rcv)
	if got := sendNoAuth(t, s.Path, capturedLine); got != "" {
		t.Fatalf("native shape got reply bytes %q", got)
	}
	got := rcv.all()
	if len(got) != 1 {
		t.Fatalf("%d receives", len(got))
	}
	in := got[0]
	if !in.Sender.Verified || in.Sender.AgentID != "aaaa" || !in.Native {
		t.Fatalf("%+v", in)
	}
	if in.Message.Parts[0].Text != "CAPTURE-PROBE from crosstalk-test-a" {
		t.Fatalf("body %q", in.Message.Parts[0].Text)
	}
	if len(in.Message.HopChain) != 1 || in.Message.HopChain[0] != "06d38d46d3fc1bcdad34fcc2" {
		t.Fatalf("hop chain %v", in.Message.HopChain)
	}
	f, err := ParseUserFrame([]byte(capturedLine))
	if err != nil || f.Attrs["from-name"] != "crosstalk-test-a" || f.Attrs["from-mode"] != "prompting" ||
		f.ClientMsgID != "1b829882-07d9-4e72-940c-a95f9d6517f5" || f.From != "uds:/tmp/cc-socks/43463.sock" {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestCapturedNativeLineWithoutAuthFromUnmappedPeerClosed(t *testing.T) {
	rcv := &recordingReceiver{}
	s, _ := start(t, staticDir(nil), rcv)
	if got := sendNoAuth(t, s.Path, capturedLine); got != "" {
		t.Fatalf("reply %q", got)
	}
	pid := os.Getpid()
	dup := staticDir{{AgentID: "a", PID: pid, StartedAt: ownStart(t)}, {AgentID: "b", PID: pid, StartedAt: ownStart(t)}}
	s2, _ := start(t, dup, rcv)
	if got := sendNoAuth(t, s2.Path, capturedLine); got != "" {
		t.Fatalf("reply %q", got)
	}
	if len(rcv.all()) != 0 {
		t.Fatal("unverified unauthenticated frame reached the receiver")
	}
}

func TestTwoLinePathStillWorksAndBadTokenCloses(t *testing.T) {
	rcv := &recordingReceiver{}
	s, tok := start(t, staticDir(nil), rcv)
	if a := ackOf(t, send(t, s.Path, tok, capturedLine)); a.State != "accepted" {
		t.Fatalf("%+v", a)
	}
	if line := send(t, s.Path, "wrong", capturedLine); line != "" {
		t.Fatalf("reply to bad token: %q", line)
	}
	if len(rcv.all()) != 1 {
		t.Fatalf("%d receives", len(rcv.all()))
	}
}

type gateReceiver struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gateReceiver) Receive(context.Context, receiver.Inbound) receiver.Result {
	close(g.entered)
	<-g.release
	return receiver.Result{MessageID: "m", State: adapter.Accepted}
}

func TestNativeShapeClosesOnlyAfterReceiverReturns(t *testing.T) {
	pid, st := os.Getpid(), ownStart(t)
	dir := staticDir{{AgentID: "aaaa", PID: pid, StartedAt: st}}
	g := &gateReceiver{entered: make(chan struct{}), release: make(chan struct{})}
	s, _ := start(t, dir, g)
	c, err := net.Dial("unix", s.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(capturedLine + "\n"))
	<-g.entered
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var ne net.Error
	if _, err := c.Read(make([]byte, 1)); !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("connection closed or written before receiver returned: %v", err)
	}
	close(g.release)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("want bare EOF, got n=%d err=%v", n, err)
	}
}

func TestNativeShapeRejectionsWriteNothing(t *testing.T) {
	pid, st := os.Getpid(), ownStart(t)
	dir := staticDir{{AgentID: "aaaa", PID: pid, StartedAt: st}}
	rcv := &recordingReceiver{}
	s, _ := start(t, dir, rcv, func(s *Server) { s.MaxFrame = 1024 })
	big := userLineJSON(strings.Repeat("x", 2048), nil)
	if got := sendNoAuth(t, s.Path, big); got != "" {
		t.Fatalf("oversize reply %q", got)
	}
	bad := userLineJSON(`<cross-session-message kind="command">x`, nil)
	if got := sendNoAuth(t, s.Path, bad); got != "" {
		t.Fatalf("invalid frame reply %q", got)
	}
	if len(rcv.all()) != 0 {
		t.Fatal("rejected frame reached the receiver")
	}
}

func envMsg(id, body string) msg.Message {
	return msg.Message{MessageID: id, Kind: msg.Inform, Parts: []msg.Part{{Type: "text", Text: body}}}
}
