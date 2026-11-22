package codex

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
)

const refreshLockDir = "/codex/thread-writer-locks"

// setRefreshLock makes the lock file of thread held and open by openers.
func setRefreshLock(f *fakeSys, thread string, openers ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := lockPath(refreshLockDir, thread)
	f.held[p] = true
	f.open[p] = [][]int{openers}
	f.reads[p] = 0
}

func TestResumeReprovesCodexClaims(t *testing.T) {
	e := newHookEnv(t, true)
	setRefreshLock(e.sys, "T1", parentPID)
	setRefreshLock(e.sys, "T2", parentPID)

	parent, _ := e.sys.Info(parentPID)
	oldClaims := NewClaims(ClaimOptions{Sys: e.sys, Parent: parent, LockDir: refreshLockDir, Registrar: &fakeRegistrar{}, Tries: 3, Sleep: func(time.Duration) {}})
	for _, th := range []string{"T1", "T2"} {
		if err := oldClaims.AcceptDurable(context.Background(), claim(th)); err != nil {
			t.Fatal(err)
		}
	}
	oldH := NewHarness(HarnessOptions{Adapter: threadsFake{adapter.NewFake(adapter.Codex)}})
	oldH.setWakeRoute(&wakeRoute{stateDir: e.state, claims: oldClaims})
	raw := oldH.RefreshState()
	if raw == nil {
		t.Fatal("no refresh state from a harness with two proven claims")
	}

	setRefreshLock(e.sys, "T2", parentPID, 5555)

	h := NewHarness(HarnessOptions{Adapter: threadsFake{adapter.NewFake(adapter.Codex)}, Claims: ClaimsConfig{StateDir: e.state, SockDir: e.sock, Sys: e.sys, ParentPID: parentPID, CodexHome: "/codex", Interval: time.Hour}})
	h.ResumeState(raw)
	reg := &fakeRegistrar{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.ServeClaims(ctx, reg) }()
	defer func() { cancel(); <-done }()
	waitUntil(t, "T2 to be dropped", func() bool {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return slices.Contains(reg.retired, "T2")
	})
	claims := h.wakeRoute().claims
	if !claims.Bound("T1") || claims.Bound("T2") {
		t.Fatalf("after resume: T1 bound=%v T2 bound=%v retired=%v, want true and false", claims.Bound("T1"), claims.Bound("T2"), reg.retired)
	}
	if b, _ := claims.Binding("T1"); b.Claim.Cwd != "/work" || b.Live {
		t.Fatalf("carried binding %+v", b)
	}
	if slices.Contains(reg.retired, "T1") {
		t.Fatalf("T1 retired: %v", reg.retired)
	}

	h.wakeRoute().wakes.Gate = newWakeEnv(t).wakes(t.TempDir()).Gate
	ss, err := h.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var label string
	for _, s := range ss {
		if s.SessionRef == "T1" {
			label = s.DeliveryMode
		}
	}
	if label != DeliveryQueueWake {
		t.Fatalf("T1 delivery_mode %q, want %q", label, DeliveryQueueWake)
	}
}

func TestResumeWithoutClaimSourceRetiresCarriedClaims(t *testing.T) {
	e := newHookEnv(t, false)
	h := NewHarness(HarnessOptions{Adapter: threadsFake{adapter.NewFake(adapter.Codex)}, Claims: ClaimsConfig{StateDir: e.state, SockDir: e.sock, Sys: e.sys, ParentPID: parentPID, CodexHome: "/codex"}})
	h.ResumeState([]byte(`{"claims":[{"claim":{"session_id":"T9"},"live":false}]}`))
	reg := &fakeRegistrar{}
	if err := h.ServeClaims(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reg.retired, []string{"T9"}) {
		t.Fatalf("retired %v, want [T9]", reg.retired)
	}
}

func TestResumeClaimNotBoundWhenRegistrationFails(t *testing.T) {
	e := newHookEnv(t, true)
	setRefreshLock(e.sys, "T1", parentPID)
	parent, _ := e.sys.Info(parentPID)
	reg := &fakeRegistrar{fail: map[string]error{"T1": errors.New("held elsewhere")}}
	c := NewClaims(ClaimOptions{Sys: e.sys, Parent: parent, LockDir: refreshLockDir, Registrar: reg, Tries: 3, Sleep: func(time.Duration) {}})
	dropped := c.Resume(context.Background(), []Binding{{Claim: claim("T1")}})
	if !slices.Equal(dropped, []string{"T1"}) || c.Bound("T1") {
		t.Fatalf("dropped %v, bound %v: a claim whose agent cannot be registered must not stay bound", dropped, c.Bound("T1"))
	}
	reg.fail = nil
	if err := c.AcceptDurable(context.Background(), claim("T1")); err != nil || !c.Bound("T1") {
		t.Fatalf("durable retry after the failure: %v bound=%v", err, c.Bound("T1"))
	}
}
