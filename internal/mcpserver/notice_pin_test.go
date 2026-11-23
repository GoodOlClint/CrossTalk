package mcpserver

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

type listCounter struct {
	*registry.Registry
	n atomic.Int64
}

func (l *listCounter) List() ([]registry.Descriptor, error) {
	l.n.Add(1)
	return l.Registry.List()
}

type countingRegistrar struct {
	harness.SessionRegistrar
	n atomic.Int64
}

func (c *countingRegistrar) RegisterSession(ctx context.Context, req harness.RequestContext, cwd string) error {
	c.n.Add(1)
	return c.SessionRegistrar.RegisterSession(ctx, req, cwd)
}

// ServeClaims on a Claude harness with a StateDir, driving the real Server:
// a session renamed with the rename tool is held under its pinned name, so
// the poll does not re-register it every tick (ADR 0027).
func TestServeClaims_PinnedNameHeldAgainstRealServer(t *testing.T) {
	stateDir := t.TempDir()
	dir := &listCounter{Registry: registry.New(stateDir)}
	e := &env{t: t, stateDir: stateDir, sessionDir: t.TempDir(), fb: NewFakeBackend()}
	e.tune = []func(*claude.HarnessOptions){func(o *claude.HarnessOptions) {
		o.Directory = dir
		o.StateDir = stateDir
		o.PPID = 4242
		o.PollInterval = time.Millisecond
	}}
	writeClaudeSession(t, e.sessionDir, 4242, "c-1", "alpha", "/w/a")
	e.start(4242)

	ctx, cancel := context.WithCancel(context.Background())
	reg := &countingRegistrar{SessionRegistrar: e.srv}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.srv.harnesses[len(e.srv.harnesses)-1].(harness.ClaimSource).ServeClaims(ctx, reg)
	}()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(5 * time.Second)
	for reg.n.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("ServeClaims never registered the session")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		isErr, out := e.call(nil, "rename", nameArg("pinned"))
		if !isErr {
			break
		}
		if out["error"] != CodeNotReady || time.Now().After(deadline) {
			t.Fatalf("rename: %v", out)
		}
		time.Sleep(time.Millisecond)
	}
	if d := e.descriptor(claudeID("c-1")); d.Name != "pinned" {
		t.Fatalf("alias after rename %q", d.Name)
	}
	// Directory.List runs once per poll and polls are sequential: a List past
	// l0+1 means the poll in flight at the rename, which may still register once,
	// has finished. Only then is the registration count a baseline.
	l0 := dir.n.Load()
	for dir.n.Load() < l0+2 {
		if time.Now().After(deadline) {
			t.Fatal("ServeClaims stopped polling")
		}
		time.Sleep(time.Millisecond)
	}
	before := reg.n.Load()
	for l1 := dir.n.Load(); dir.n.Load() < l1+30; {
		if time.Now().After(deadline) {
			t.Fatal("ServeClaims stopped polling")
		}
		time.Sleep(time.Millisecond)
	}
	if after := reg.n.Load(); after != before {
		t.Fatalf("session held under its pin was registered %d more times", after-before)
	}
}

// The notice is held before ready closes, so a call that sees the agent ready
// also sees the notice. The backend's Published hook runs on the registering
// goroutine just before the notice is built; the test takes the agent's lock
// there, so registration parks at the hold and ready must stay open.
type publishHook struct {
	Backend
	f func(*Agent)
}

func (p publishHook) Published(a *Agent) { p.f(a) }

func TestAgent_ReadyClosesOnlyAfterNoticeHeld(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(
		Session{Harness: "codex", SessionRef: "owner", Name: "dup", Cwd: "/w"},
		Session{Harness: "codex", SessionRef: "late", Name: "dup", Cwd: "/w"},
	)
	var late atomic.Pointer[Agent]
	published := make(chan struct{})
	e := &env{t: t, stateDir: t.TempDir(), sessionDir: t.TempDir(), fb: fb}
	e.wrap = func(b Backend) Backend {
		return publishHook{b, func(a *Agent) {
			if a.id == codexID("late") {
				a.mu.Lock()
				late.Store(a)
				close(published)
			}
		}}
	}
	e.start(999999)
	e.mustOK(thread("owner"), "send_message", sendArgs("x", "0"))

	errc := make(chan error, 1)
	go func() {
		errc <- e.srv.RegisterSession(context.Background(), harness.RequestContext{Meta: thread("late")}, "/w")
	}()
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("registration never reached Published")
	}
	a := late.Load()
	select {
	case <-a.ready:
		a.mu.Unlock()
		t.Fatal("ready closed before the registration notice was held")
	case <-time.After(300 * time.Millisecond):
	}
	a.mu.Unlock()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	out := e.mustOK(thread("late"), "send_message", sendArgs("x", "1"))
	if n, _ := out["notice"].(string); !strings.Contains(n, "held") {
		t.Fatalf("first call after registration has no held notice: %v", out)
	}
}
