package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
)

type hookEnv struct {
	state, sock string
	sys         *fakeSys
	parent      ownerproof.Proc
}

// newHookEnv puts this test process where the hook runs: under the shell
// under the codex parent, so its real peer pid has that fake ancestry.
func newHookEnv(t *testing.T, on bool) *hookEnv {
	t.Helper()
	sock, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	e := &hookEnv{state: t.TempDir(), sock: sock, sys: newFakeSys()}
	e.sys.proc(os.Getpid(), shellPID, t0.Add(time.Minute), "crosstalk", "hook", "codex", "session-start")
	e.parent, _ = e.sys.Info(parentPID)
	if on {
		if err := os.WriteFile(filepath.Join(e.state, SettingFile), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *hookEnv) run(t *testing.T, stdin string) string {
	t.Helper()
	var out bytes.Buffer
	start := time.Now()
	code := SessionStartHook(strings.NewReader(stdin), &out, HookOptions{StateDir: e.state, SockDir: e.sock, Sys: e.sys, Self: os.Getpid(), Timeout: 500 * time.Millisecond})
	if code != 0 || out.String() != "{}\n" {
		t.Fatalf("hook exit %d stdout %q, want 0 and {}", code, out.String())
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("hook took %v", d)
	}
	return out.String()
}

const startPayload = `{"session_id":"T1","source":"startup","cwd":"/work","transcript_path":"/rollout.jsonl","hook_event_name":"SessionStart"}`

func (e *hookEnv) claimFiles(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(ClaimDir(e.state))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range ents {
		out = append(out, x.Name())
	}
	return out
}

func TestHook_BadStdin_ExpectEmptyObjectExit0NoFile(t *testing.T) {
	e := newHookEnv(t, true)
	for _, in := range []string{"", "not json", `{"session_id":"../x"}`, `{"session_id":""}`} {
		e.run(t, in)
	}
	if fs := e.claimFiles(t); len(fs) != 0 {
		t.Fatalf("files %v", fs)
	}
}

func TestHook_NoCodexAncestor_ExpectEmptyObjectExit0NoFile(t *testing.T) {
	e := newHookEnv(t, true)
	e.sys.proc(parentPID, 1, t0, "node", "--no-daemon")
	e.run(t, startPayload)
	if fs := e.claimFiles(t); len(fs) != 0 {
		t.Fatalf("files %v", fs)
	}
}

func TestHook_NoListener_ExpectDurableFile0600AndEmptyObject(t *testing.T) {
	e := newHookEnv(t, true)
	e.run(t, startPayload)
	path := claimFile(e.state, e.parent, "T1")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("claim file mode %v", fi.Mode().Perm())
	}
	di, err := os.Stat(ClaimDir(e.state))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("claim dir %v %v", di, err)
	}
	b, _ := os.ReadFile(path)
	var cl Claim
	if err := json.Unmarshal(b, &cl); err != nil {
		t.Fatal(err)
	}
	if cl.SessionID != "T1" || cl.Source != "startup" || cl.OwnerPID != parentPID || cl.OwnerStartMicros != t0.UnixMicro() || cl.TranscriptPath != "/rollout.jsonl" {
		t.Fatalf("claim %+v", cl)
	}
	if fs := e.claimFiles(t); len(fs) != 1 {
		t.Fatalf("files %v (temp file left behind?)", fs)
	}
}

func TestHook_SettingOff_ExpectNoFileNoDir(t *testing.T) {
	e := newHookEnv(t, false)
	e.run(t, startPayload)
	if _, err := os.Stat(ClaimDir(e.state)); !os.IsNotExist(err) {
		t.Fatalf("claim dir exists with the setting off: %v", err)
	}
}

func TestHook_ListenerNeverAnswers_ExpectReturnsWithinBound(t *testing.T) {
	e := newHookEnv(t, true)
	ln, err := net.Listen("unix", ClaimSocketPath(e.sock, e.parent))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	e.run(t, startPayload)
	if _, err := os.Stat(claimFile(e.state, e.parent, "T1")); err != nil {
		t.Fatalf("durable file not written before the live attempt: %v", err)
	}
}

func TestClaimSocketPath_LongHome_ExpectFitsSunPath(t *testing.T) {
	t.Setenv("HOME", "/Users/"+strings.Repeat("h", 200))
	p := ClaimSocketPath(DefaultSockDir(), ownerproof.Proc{PID: 99999999, Start: time.Date(2286, 1, 1, 0, 0, 0, 999999000, time.UTC)})
	if len(p) >= 104 {
		t.Fatalf("socket path %q is %d bytes; sun_path is 104 on macOS", p, len(p))
	}
}

func startServer(t *testing.T, e *hookEnv, reg *fakeRegistrar) (*Claims, context.CancelFunc) {
	t.Helper()
	c := newTestClaims(t, e.sys, reg)
	srv := &ClaimServer{Claims: c, Owner: e.parent, StateDir: e.state, SockDir: e.sock, Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(ctx); close(done) }()
	path := ClaimSocketPath(e.sock, e.parent)
	waitUntil(t, "listener", func() bool { _, err := os.Stat(path); return err == nil })
	t.Cleanup(func() { cancel(); <-done })
	return c, cancel
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClaimServer_LiveClaimFromHookOverRealSocket_ExpectBoundLive(t *testing.T) {
	e := newHookEnv(t, true)
	e.sys.own("T1")
	reg := &fakeRegistrar{}
	c, _ := startServer(t, e, reg)
	e.run(t, startPayload)
	b, ok := c.Binding("T1")
	if !ok || !b.Live || !slices.Equal(reg.got(), []string{"T1"}) {
		t.Fatalf("binding %+v %v, registered %v", b, ok, reg.got())
	}
}

func TestClaimServer_LiveClaimFromOutsideAncestryOverRealSocket_ExpectIgnored(t *testing.T) {
	e := newHookEnv(t, true)
	e.sys.own("T1")
	reg := &fakeRegistrar{}
	c, _ := startServer(t, e, reg)
	e.sys.proc(os.Getpid(), 1, t0.Add(time.Minute), "crosstalk")
	line, _ := json.Marshal(Claim{SessionID: "T1", OwnerPID: parentPID, OwnerStartMicros: t0.UnixMicro()})
	conn, err := net.Dial("unix", ClaimSocketPath(e.sock, e.parent))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write(append(line, '\n'))
	reply, _ := bufio.NewReader(conn).ReadString('\n')
	if reply != "ignored\n" || c.Bound("T1") || len(reg.got()) != 0 {
		t.Fatalf("reply %q, bound %v, registered %v", reply, c.Bound("T1"), reg.got())
	}
}

func TestClaimServer_DurableScanOnStart_ExpectOnlyOwnParentsClaimsBound(t *testing.T) {
	e := newHookEnv(t, true)
	e.sys.own("T1")
	e.sys.own("T9")
	e.run(t, startPayload)
	other := ownerproof.Proc{PID: 4242, Start: t0}
	line, _ := json.Marshal(Claim{SessionID: "T9"})
	if err := writeClaimFile(claimFile(e.state, other, "T9"), line); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistrar{}
	c, _ := startServer(t, e, reg)
	waitUntil(t, "durable claim", func() bool { return c.Bound("T1") })
	if b, _ := c.Binding("T1"); b.Live {
		t.Fatal("durable claim marked live")
	}
	if c.Bound("T9") || slices.Contains(reg.got(), "T9") {
		t.Fatal("another owner's claim file was accepted")
	}
}

func TestServeClaims_SettingOff_ExpectReturnsAtOnceNoSocketNoDir(t *testing.T) {
	e := newHookEnv(t, false)
	h := NewHarness(HarnessOptions{Adapter: threadsFake{}, Claims: ClaimsConfig{StateDir: e.state, SockDir: filepath.Join(e.sock, "s"), Sys: e.sys, ParentPID: parentPID, CodexHome: "/codex"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.ServeClaims(ctx, &fakeRegistrar{}); err != nil || ctx.Err() != nil {
		t.Fatalf("err %v ctx %v", err, ctx.Err())
	}
	for _, p := range []string{ClaimDir(e.state), filepath.Join(e.sock, "s")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s exists with the setting off", p)
		}
	}
}

func TestServeClaims_ParentNotCodex_ExpectReturnsAtOnce(t *testing.T) {
	e := newHookEnv(t, true)
	e.sys.proc(parentPID, 1, t0, "bash")
	h := NewHarness(HarnessOptions{Adapter: threadsFake{}, Claims: ClaimsConfig{StateDir: e.state, SockDir: filepath.Join(e.sock, "s"), Sys: e.sys, ParentPID: parentPID, CodexHome: "/codex"}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.ServeClaims(ctx, &fakeRegistrar{}); err != nil || ctx.Err() != nil {
		t.Fatalf("err %v ctx %v", err, ctx.Err())
	}
	if _, err := os.Stat(filepath.Join(e.sock, "s")); !os.IsNotExist(err) {
		t.Fatal("socket dir created under a non-codex parent")
	}
}

func TestClaimServer_FailedDurableClaimRescanned_ExpectTriedOncePerWrite(t *testing.T) {
	e := newHookEnv(t, true)
	e.sys.set("T1", false)
	e.run(t, startPayload)
	reg := &fakeRegistrar{}
	c := newTestClaims(t, e.sys, reg)
	srv := &ClaimServer{Claims: c, Owner: e.parent, StateDir: e.state, SockDir: e.sock}
	p := lockPath(lockDir, "T1")
	srv.Scan(context.Background())
	srv.Scan(context.Background())
	if c.Bound("T1") || e.sys.lockReads(p) != 3 {
		t.Fatalf("bound %v, lock read %d times, want one debounced attempt (3)", c.Bound("T1"), e.sys.lockReads(p))
	}
	e.sys.own("T1")
	e.run(t, `{"session_id":"T1","source":"resume","cwd":"/work"}`)
	srv.Scan(context.Background())
	if !c.Bound("T1") {
		t.Fatal("a refired claim was not retried")
	}
}

// connCounter accepts on the claim socket and counts connections.
func connCounter(t *testing.T, path string) *atomic.Int64 {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Write([]byte("ok\n"))
			c.Close()
		}
	}()
	return &n
}

func TestHook_SocketDirOpenToOthers_ExpectNoLiveSendDurableWritten(t *testing.T) {
	e := newHookEnv(t, true)
	n := connCounter(t, ClaimSocketPath(e.sock, e.parent))
	if err := os.Chmod(e.sock, 0o755); err != nil {
		t.Fatal(err)
	}
	e.run(t, startPayload)
	if n.Load() != 0 {
		t.Fatal("claim sent into a socket directory others can write")
	}
	if _, err := os.Stat(claimFile(e.state, e.parent, "T1")); err != nil {
		t.Fatalf("durable claim not written: %v", err)
	}
}

func TestHook_SocketDirIsSymlink_ExpectNoLiveSend(t *testing.T) {
	e := newHookEnv(t, true)
	n := connCounter(t, ClaimSocketPath(e.sock, e.parent))
	link := e.sock + "-link"
	if err := os.Symlink(e.sock, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
	var out bytes.Buffer
	SessionStartHook(strings.NewReader(startPayload), &out, HookOptions{StateDir: e.state, SockDir: link, Sys: e.sys, Self: os.Getpid(), Timeout: 500 * time.Millisecond})
	if n.Load() != 0 || out.String() != "{}\n" {
		t.Fatalf("connections %d stdout %q", n.Load(), out.String())
	}
}

func TestHook_SocketDirMissing_ExpectNotCreated(t *testing.T) {
	e := newHookEnv(t, true)
	missing := filepath.Join(e.sock, "absent")
	var out bytes.Buffer
	SessionStartHook(strings.NewReader(startPayload), &out, HookOptions{StateDir: e.state, SockDir: missing, Sys: e.sys, Self: os.Getpid(), Timeout: 500 * time.Millisecond})
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("hook created the socket directory: %v", err)
	}
}

func TestHook_ClaimDirOpenToOthers_ExpectNoFileWritten(t *testing.T) {
	e := newHookEnv(t, true)
	if err := os.MkdirAll(ClaimDir(e.state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ClaimDir(e.state), 0o777); err != nil {
		t.Fatal(err)
	}
	e.run(t, startPayload)
	if fs := e.claimFiles(t); len(fs) != 0 {
		t.Fatalf("claim written into a directory others can write: %v", fs)
	}
}

func TestClaimServer_SocketDirOpenToOthers_ExpectNoListener(t *testing.T) {
	e := newHookEnv(t, true)
	if err := os.Chmod(e.sock, 0o777); err != nil {
		t.Fatal(err)
	}
	srv := &ClaimServer{Claims: newTestClaims(t, e.sys, &fakeRegistrar{}), Owner: e.parent, StateDir: e.state, SockDir: e.sock}
	if ln, err := srv.listen(); err == nil {
		ln.Close()
		t.Fatal("listened in a socket directory others can write")
	}
}

func TestClaimServer_DurableFileNameDisagreesWithBody_ExpectIgnored(t *testing.T) {
	e := newHookEnv(t, true)
	e.sys.own("T1")
	e.sys.own("T2")
	line, _ := json.Marshal(Claim{SessionID: "T1"})
	if err := writeClaimFile(claimFile(e.state, e.parent, "T2"), line); err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistrar{}
	c := newTestClaims(t, e.sys, reg)
	srv := &ClaimServer{Claims: c, Owner: e.parent, StateDir: e.state, SockDir: e.sock}
	srv.Scan(context.Background())
	if c.Bound("T1") || c.Bound("T2") || len(reg.got()) != 0 {
		t.Fatalf("mismatched claim file accepted: registered %v", reg.got())
	}
}
