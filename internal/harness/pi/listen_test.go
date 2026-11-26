package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const testToken = "tok-0123456789abcdef"

// shortDir keeps socket paths under the 104-byte macOS limit.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "pi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type shim struct {
	h    *Harness
	reg  *registry.Registry
	srv  *mcpserver.Server
	cs   *mcp.ClientSession
	stop func()
	done chan error
}

type shimOpt func(*Options)

// startShim runs a real MCP server with the Pi harness and its claim source.
// The test process is the extension's peer, so the parent pid defaults to it.
func startShim(t *testing.T, stateDir, sockDir string, opts ...shimOpt) *shim {
	t.Helper()
	reg := registry.New(stateDir)
	o := Options{PPID: os.Getpid(), Token: testToken, SockDir: sockDir, Host: func(int) bool { return true }, Directory: reg}
	for _, f := range opts {
		f(&o)
	}
	h := NewHarness(o)
	fb := mcpserver.NewFakeBackend()
	fb.Harnesses = []harness.Harness{h}
	srv := mcpserver.New(mcpserver.Options{Backend: fb.AsBackend(), Registry: reg, Harnesses: []harness.Harness{h}, PPID: o.PPID})
	st, ct := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	s := &shim{h: h, reg: reg, srv: srv, done: make(chan error, 1)}
	go func() { s.done <- h.ServeClaims(ctx, srv) }()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cs = cs
	var once bool
	s.stop = func() {
		if once {
			return
		}
		once = true
		cancel()
		if err := <-s.done; err != nil {
			t.Errorf("ServeClaims: %v", err)
		}
		cs.Close()
		srv.Close()
	}
	t.Cleanup(s.stop)
	return s
}

func (s *shim) sock() string { return s.h.sockPath() }

type client struct {
	c net.Conn
	r *bufio.Reader
}

func dial(t *testing.T, path string) *client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			t.Cleanup(func() { c.Close() })
			return &client{c: c, r: bufio.NewReader(c)}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *client) send(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if _, err := c.c.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// read returns the next reply line, or "" with the error at EOF or timeout.
func (c *client) read() (map[string]any, error) {
	c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(line, &m)
}

func helloFor(id, file string) hello {
	return hello{Proto: Proto, Token: testToken, ExtVersion: "0.3.0", PiVersion: "1.6.0", SessionID: id, SessionFile: file, Cwd: "/work", Name: ""}
}

// greet sends h on a new connection and returns the connection and the reply.
func greet(t *testing.T, s *shim, h hello) (*client, map[string]any) {
	t.Helper()
	c := dial(t, s.sock())
	c.send(t, h)
	m, err := c.read()
	if err != nil {
		t.Fatalf("no reply to hello: %v", err)
	}
	return c, m
}

func mustAck(t *testing.T, m map[string]any) {
	t.Helper()
	if m["hello"] != "ok" {
		t.Fatalf("reply %v, want the hello ack", m)
	}
}

func mustRefuse(t *testing.T, m map[string]any, reason string) {
	t.Helper()
	e, _ := m["error"].(string)
	if !strings.Contains(e, reason) || m["hello"] != nil {
		t.Fatalf("reply %v, want an error naming %q", m, reason)
	}
}

func piDescriptors(t *testing.T, reg *registry.Registry) []registry.Descriptor {
	t.Helper()
	ds, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	var out []registry.Descriptor
	for _, d := range ds {
		if d.Harness == string(adapter.Pi) {
			out = append(out, d)
		}
	}
	return out
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func identify(s *shim, ppid int) (harness.Session, bool) {
	ss, ok, _ := s.h.Identify(context.Background(), harness.RequestContext{ParentPID: ppid})
	return ss, ok
}

func TestHelloAcceptedRegistersAndAcks(t *testing.T) {
	s := startShim(t, t.TempDir(), shortDir(t))
	_, m := greet(t, s, helloFor("review-1", "/a/sessions/x.jsonl"))
	mustAck(t, m)
	ss, ok := identify(s, os.Getpid())
	if !ok || !strings.HasPrefix(ss.SessionRef, "review-1@") || len(ss.SessionRef) != len("review-1@")+8 {
		t.Fatalf("identify %+v %v, want the non-UUID id with its file hash", ss, ok)
	}
	ds := piDescriptors(t, s.reg)
	if len(ds) != 1 || ds[0].SessionRef != ss.SessionRef || ds[0].Cwd != "/work" {
		t.Fatalf("descriptors %+v", ds)
	}
}

func TestPeerOtherThanParentClosedUnread(t *testing.T) {
	s := startShim(t, t.TempDir(), shortDir(t), func(o *Options) { o.PPID = os.Getppid() })
	c := dial(t, s.sock())
	c.send(t, helloFor("s1", ""))
	// Linux resets a connection closed with unread data; macOS gives EOF.
	if m, err := c.read(); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read %v %v, want the connection closed with no reply", m, err)
	}
	if _, ok := identify(s, os.Getppid()); ok {
		t.Fatal("session proven by a peer that is not the parent")
	}
	if ds := piDescriptors(t, s.reg); len(ds) != 0 {
		t.Fatalf("descriptors %+v", ds)
	}
}

func TestNonPiParentServesNothing(t *testing.T) {
	sockDir := shortDir(t)
	s := startShim(t, t.TempDir(), sockDir, func(o *Options) { o.Host = func(int) bool { return false } })
	select {
	case err := <-s.done:
		s.done <- err
	case <-time.After(2 * time.Second):
		t.Fatal("ServeClaims kept running for a parent that is not Pi")
	}
	if _, err := os.Stat(s.sock()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket stat: %v, want none", err)
	}
	if _, ok := identify(s, os.Getpid()); ok {
		t.Fatal("identified a call under a non-Pi parent")
	}
}

func TestIsPiHost(t *testing.T) {
	cmd := exec.Command("bash", "-c", "exec -a pi cat")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Wait() })
	pid := cmd.Process.Pid
	t.Setenv("PI_CODING_AGENT", "")
	if IsPiHost(pid) {
		t.Error("Pi host without PI_CODING_AGENT=true")
	}
	t.Setenv("PI_CODING_AGENT", "true")
	eventually(t, "the pi command name", func() bool { return IsPiHost(pid) })
	if IsPiHost(os.Getpid()) {
		t.Error("the test binary counted as a Pi host")
	}
}

func TestHelloRefusals(t *testing.T) {
	old := helloTimeout
	helloTimeout = 300 * time.Millisecond
	t.Cleanup(func() { helloTimeout = old })
	s := startShim(t, t.TempDir(), shortDir(t))

	bad := helloFor("s1", "")
	bad.Token = "wrong"
	_, m := greet(t, s, bad)
	mustRefuse(t, m, "token")

	bad = helloFor("s1", "")
	bad.Proto = 2
	_, m = greet(t, s, bad)
	mustRefuse(t, m, "protocol 2")

	bad = helloFor("s1", "")
	bad.PiVersion = "2.0.0"
	_, m = greet(t, s, bad)
	mustRefuse(t, m, "major 2")

	for _, id := range []string{"../x", "-x", "x-", ".x", "a/b", ""} {
		_, m = greet(t, s, helloFor(id, ""))
		mustRefuse(t, m, "session id")
	}

	b, _ := json.Marshal(helloFor("s1", ""))
	c0 := dial(t, s.sock())
	c0.c.Write(b)
	c0.c.(*net.UnixConn).CloseWrite()
	m, _ = c0.read()
	mustRefuse(t, m, "hello")

	c := dial(t, s.sock())
	c.c.Write([]byte("{not json\n"))
	m, _ = c.read()
	mustRefuse(t, m, "malformed")

	c = dial(t, s.sock())
	c.c.Write([]byte(`{"proto":1,"name":"` + strings.Repeat("x", MaxLine) + "\"}\n"))
	m, _ = c.read()
	mustRefuse(t, m, "hello")

	c = dial(t, s.sock())
	c.c.Write([]byte(`{"proto":1`))
	start := time.Now()
	m, _ = c.read()
	mustRefuse(t, m, "hello")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("slow hello refused after %v", d)
	}

	if _, ok := identify(s, os.Getpid()); ok {
		t.Fatal("a refused hello proved a session")
	}
	if ds := piDescriptors(t, s.reg); len(ds) != 0 {
		t.Fatalf("descriptors %+v", ds)
	}
	_, m = greet(t, s, helloFor("s1", ""))
	mustAck(t, m)
}

func TestUntestedMinorAccepted(t *testing.T) {
	s := startShim(t, t.TempDir(), shortDir(t))
	h := helloFor("s1", "")
	h.PiVersion = "1.9.0"
	_, m := greet(t, s, h)
	mustAck(t, m)
}

func TestCheckVersion(t *testing.T) {
	for v, want := range map[string]string{"1.6.0": "ok", "1.6.3": "ok", "1.9.0": "warn", "2.0.0": "err", "0.9.0": "err", "1.6": "err", "v1.6.0": "err"} {
		warn, err := CheckVersion(v)
		got := "ok"
		if err != nil {
			got = "err"
		} else if warn != "" {
			got = "warn"
		}
		if got != want {
			t.Errorf("%s: %s, want %s", v, got, want)
		}
	}
}

// ADR 0036 point 5: a shim serves one session; a hello naming another is
// refused, logged and answered, and the first stays bound and listed.
func TestSecondSessionIDRefused(t *testing.T) {
	s := startShim(t, t.TempDir(), shortDir(t))
	c, m := greet(t, s, helloFor("first", "/a/first.jsonl"))
	mustAck(t, m)
	first, _ := identify(s, os.Getpid())

	c.send(t, helloFor("second", "/a/second.jsonl"))
	m, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, m, "second")
	if m, err := c.read(); !errors.Is(err, io.EOF) {
		t.Fatalf("read %v %v, want the conflicting connection closed", m, err)
	}

	c.c.Close()
	_, m = greet(t, s, helloFor("second", "/a/second.jsonl"))
	mustRefuse(t, m, "second")

	if ss, ok := identify(s, os.Getpid()); !ok || ss.SessionRef != first.SessionRef {
		t.Fatalf("identify %+v %v, want %s", ss, ok, first.SessionRef)
	}
	ds := piDescriptors(t, s.reg)
	if len(ds) != 1 || ds[0].SessionRef != first.SessionRef {
		t.Fatalf("descriptors %+v", ds)
	}
	_, m = greet(t, s, helloFor("first", "/a/first.jsonl"))
	mustAck(t, m)
}

func TestSessionRef(t *testing.T) {
	a := helloFor("review-1", "/p/one/.pi/sessions/review-1.jsonl").ref()
	b := helloFor("review-1", "/p/two/.pi/sessions/review-1.jsonl").ref()
	if a == b || !strings.HasPrefix(a, "review-1@") || !strings.HasPrefix(b, "review-1@") {
		t.Fatalf("refs %q %q, want two distinct refs for one id in two directories", a, b)
	}
	long := strings.Repeat("a", 300)
	if !sessionID.MatchString(long) || !sessionID.MatchString("review-1") || !sessionID.MatchString("x") {
		t.Fatal("an id Pi accepts was refused")
	}
	if r := helloFor("review-1", "").ref(); r != "review-1" {
		t.Fatalf("no session file: ref %q, want the bare id", r)
	}
}

// --no-session, -p and --mode rpc start the extension with no session file
// (spike 24, M4): the bare id registers.
func TestNoSessionFileRegistersBareID(t *testing.T) {
	s := startShim(t, t.TempDir(), shortDir(t))
	_, m := greet(t, s, helloFor("0199a1b2-c3d4", ""))
	mustAck(t, m)
	ds := piDescriptors(t, s.reg)
	if len(ds) != 1 || ds[0].SessionRef != "0199a1b2-c3d4" {
		t.Fatalf("descriptors %+v", ds)
	}
}

func TestIdentifyOnlyProvenParent(t *testing.T) {
	s := startShim(t, t.TempDir(), shortDir(t))
	if _, ok := identify(s, os.Getpid()); ok {
		t.Fatal("identified before any hello")
	}
	_, m := greet(t, s, helloFor("s1", ""))
	mustAck(t, m)
	if _, ok := identify(s, os.Getpid()); !ok {
		t.Fatal("proven parent not identified")
	}
	if _, ok := identify(s, os.Getppid()); ok {
		t.Fatal("identified a call from another parent")
	}
	b, ok, _ := s.h.Bind(context.Background(), harness.RequestContext{ParentPID: os.Getpid()})
	if !ok || b.HarnessVersion != "1.6.0" || b.HarnessEndpoint != s.sock() || b.SessionRef != "s1" {
		t.Fatalf("bind %+v %v", b, ok)
	}
	if _, ok, _ := s.h.Bind(context.Background(), harness.RequestContext{ParentPID: os.Getppid()}); ok {
		t.Fatal("bound a call from another parent")
	}
}

func discover(t *testing.T, h *Harness) []harness.Session {
	t.Helper()
	ss, err := h.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

// D4: another shim lists the session reachable with its status while the
// extension is connected, unreachable once it drops, and not at all after
// the owning shim exits.
func TestSecondShimListsSession(t *testing.T) {
	stateDir, sockDir := t.TempDir(), shortDir(t)
	s := startShim(t, stateDir, sockDir)
	other := NewHarness(Options{PPID: 1, SockDir: sockDir, Directory: registry.New(stateDir), Host: func(int) bool { return false }})

	c, m := greet(t, s, helloFor("s1", ""))
	mustAck(t, m)
	ss := discover(t, other)
	if len(ss) != 1 || !ss[0].Reachable || ss[0].SessionRef != "s1" || ss[0].Harness != adapter.Pi {
		t.Fatalf("discover %+v, want s1 reachable", ss)
	}
	c.send(t, map[string]string{"status": "busy"})
	eventually(t, "busy", func() bool { ss := discover(t, other); return len(ss) == 1 && ss[0].Status == "busy" })
	if st, _ := s.h.Status(context.Background(), "s1"); !st.Busy || !st.Reachable || !st.Loaded {
		t.Fatalf("status %+v", st)
	}
	c.send(t, map[string]string{"status": "idle"})
	eventually(t, "idle", func() bool { ss := discover(t, other); return len(ss) == 1 && ss[0].Status == "idle" })
	fi, err := os.Stat(s.h.statusPath(os.Getpid(), os.Getpid()))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("status file %v %v, want 0600", fi, err)
	}

	c.c.Close()
	eventually(t, "unreachable", func() bool { ss := discover(t, other); return len(ss) == 1 && !ss[0].Reachable })

	s.stop()
	if ss := discover(t, other); len(ss) != 0 {
		t.Fatalf("discover after the owner exited %+v", ss)
	}
	if _, err := os.Stat(s.sock()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left after exit: %v", err)
	}
}

// Two Pi processes on one session file (spike 24, M4) make two shims claim
// one (pi, session ref): the registry keeps the live holder and refuses the
// later claimant, with no takeover.
func TestSecondClaimantRefusedNoTakeover(t *testing.T) {
	stateDir := t.TempDir()
	a := startShim(t, stateDir, shortDir(t))
	b := startShim(t, stateDir, shortDir(t))
	h := helloFor("shared", "/p/.pi/sessions/shared.jsonl")
	_, m := greet(t, a, h)
	mustAck(t, m)
	before := piDescriptors(t, a.reg)

	_, m = greet(t, b, h)
	mustRefuse(t, m, "agent_held")
	if _, ok := identify(b, os.Getpid()); ok {
		t.Fatal("refused claimant holds the session")
	}
	after := piDescriptors(t, a.reg)
	if len(before) != 1 || len(after) != 1 || after[0].InstanceID != before[0].InstanceID {
		t.Fatalf("descriptors before %+v after %+v, want the first holder kept", before, after)
	}
	if ss := discover(t, b.h); len(ss) != 1 || ss[0].SessionRef != before[0].SessionRef {
		t.Fatalf("discover %+v", ss)
	}
}

func TestNameLineRenamesUnlessPinned(t *testing.T) {
	s := startShim(t, t.TempDir(), shortDir(t))
	h := helloFor("s1", "")
	h.Name = "first-name"
	c, m := greet(t, s, h)
	mustAck(t, m)
	name := func() string {
		ds := piDescriptors(t, s.reg)
		if len(ds) != 1 {
			return ""
		}
		return ds[0].Name
	}
	if n := name(); n != "first-name" {
		t.Fatalf("name %q", n)
	}
	c.send(t, map[string]string{"name": "second-name"})
	eventually(t, "the renamed session", func() bool { return name() == "second-name" })

	res, err := s.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "rename", Arguments: map[string]any{"name": "pinned-name"}})
	if err != nil || res.IsError {
		t.Fatalf("rename: %v %+v", err, res)
	}
	c.send(t, map[string]string{"name": "third-name"})
	c.send(t, map[string]string{"status": "busy"})
	eventually(t, "the busy line after the name line", func() bool { st, _ := s.h.Status(context.Background(), "s1"); return st.Busy })
	if n := name(); n != "pinned-name" {
		t.Fatalf("name %q after a harness rename, want the pin kept", n)
	}
	if ss, _ := identify(s, os.Getpid()); ss.Name != "third-name" {
		t.Fatalf("identify name %q, want the harness name recorded", ss.Name)
	}
}

// M5: Pi waits 10 s for a direct server's initialize; the claim source's
// startup work must not hold it.
func TestInitializeNotHeldByClaimSource(t *testing.T) {
	release := make(chan struct{})
	start := time.Now()
	s := startShim(t, t.TempDir(), shortDir(t), func(o *Options) {
		o.Host = func(int) bool { <-release; return false }
	})
	t.Cleanup(func() { close(release) })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("initialize answered after %v", d)
	}
	if _, err := s.cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := identify(s, os.Getpid()); ok {
		t.Fatal("identified before any hello")
	}
}

func TestSocketDirIsPrivate(t *testing.T) {
	sockDir := filepath.Join(shortDir(t), "pi")
	s := startShim(t, t.TempDir(), sockDir)
	dial(t, s.sock())
	fi, err := os.Stat(sockDir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir %v %v, want 0700", fi, err)
	}
}

// A second shim under the same live Pi process waits for the first to stop
// listening rather than displace it, and never removes a file that is not a
// socket.
func TestListenerNotDisplaced(t *testing.T) {
	old := listenRetry
	listenRetry = 20 * time.Millisecond
	t.Cleanup(func() { listenRetry = old })
	sockDir := shortDir(t)
	a := startShim(t, t.TempDir(), sockDir)
	dial(t, a.sock())
	b := startShim(t, t.TempDir(), sockDir)
	time.Sleep(100 * time.Millisecond)
	_, m := greet(t, a, helloFor("s1", ""))
	mustAck(t, m)
	if _, ok := identify(a, os.Getpid()); !ok {
		t.Fatal("the first shim lost its connection")
	}
	if _, ok := identify(b, os.Getpid()); ok {
		t.Fatal("the waiting shim took the hello")
	}
	a.stop()
	_, m = greet(t, b, helloFor("s1", ""))
	mustAck(t, m)

	foreign := shortDir(t)
	path := filepath.Join(foreign, strconv.Itoa(os.Getpid())+".sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(Options{PPID: os.Getpid(), Token: testToken, SockDir: foreign, Host: func(int) bool { return true }})
	if err := h.ServeClaims(context.Background(), nil); err == nil {
		t.Fatal("served over a file that is not a socket")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "keep" {
		t.Fatalf("foreign file %q %v", b, err)
	}
}

// lateRegistrar finishes a registration only after the shim is told to stop.
type lateRegistrar struct{ entered chan struct{} }

func (r lateRegistrar) RegisterSession(ctx context.Context, _ harness.RequestContext, _ string) error {
	close(r.entered)
	<-ctx.Done()
	return nil
}

func (lateRegistrar) RetireSession(context.Context, harness.RequestContext) (bool, error) {
	return false, nil
}

func TestStopDuringRegistrationReturns(t *testing.T) {
	h := NewHarness(Options{PPID: os.Getpid(), Token: testToken, SockDir: shortDir(t), Host: func(int) bool { return true }})
	ctx, cancel := context.WithCancel(context.Background())
	r := lateRegistrar{entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- h.ServeClaims(ctx, r) }()
	c := dial(t, h.sockPath())
	c.send(t, helloFor("s1", ""))
	<-r.entered
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeClaims did not return after a registration finished during shutdown")
	}
}

type failingDir struct{}

func (failingDir) List() ([]registry.Descriptor, error) { return nil, errors.New("unreadable") }

func TestDiscoverReportsRegistryError(t *testing.T) {
	h := NewHarness(Options{PPID: 1, SockDir: shortDir(t), Directory: failingDir{}})
	if _, err := h.Discover(context.Background()); err == nil {
		t.Fatal("registry error hidden")
	}
}

// probeRegistrar records what Identify reports while a registration runs,
// for the registration's own call and for any other call, then refuses.
type probeRegistrar struct {
	h    *Harness
	seen chan [2]bool
}

func (r probeRegistrar) RegisterSession(ctx context.Context, req harness.RequestContext, _ string) error {
	_, own, _ := r.h.Identify(ctx, req)
	_, other, _ := r.h.Identify(ctx, harness.RequestContext{ParentPID: r.h.ppid})
	r.seen <- [2]bool{own, other}
	return errors.New("registry refused")
}

func (probeRegistrar) RetireSession(context.Context, harness.RequestContext) (bool, error) {
	return false, nil
}

func TestRefusedRegistrationNeverIdentified(t *testing.T) {
	h := NewHarness(Options{PPID: os.Getpid(), Token: testToken, SockDir: shortDir(t), Host: func(int) bool { return true }})
	r := probeRegistrar{h: h, seen: make(chan [2]bool, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.ServeClaims(ctx, r) }()
	t.Cleanup(func() { cancel(); <-done })
	c := dial(t, h.sockPath())
	c.send(t, helloFor("s1", ""))
	m, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, m, "not registered")
	seen := <-r.seen
	if !seen[0] {
		t.Fatal("the registration's own call did not see the session it registers")
	}
	if seen[1] {
		t.Fatal("another call saw a session whose registration was then refused")
	}
	if _, ok, _ := h.Identify(context.Background(), harness.RequestContext{ParentPID: os.Getpid()}); ok {
		t.Fatal("refused session identified after the refusal")
	}
}
