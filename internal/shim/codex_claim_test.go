package shim

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

// ownerSys is a kernel in which pid 7000 is a --no-daemon codex process that
// alone holds and has open every lock file.
type ownerSys struct{}

var ownerStart = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func (ownerSys) Info(pid int) (ownerproof.Proc, error) {
	if pid != 7000 {
		return ownerproof.Proc{}, ownerproof.ErrNoProcess
	}
	return ownerproof.Proc{PID: 7000, PPID: 1, Start: ownerStart}, nil
}
func (ownerSys) Args(int) (string, []string, error) {
	return "/bin/codex", []string{"codex", "--no-daemon"}, nil
}
func (ownerSys) LockHeld(string) (bool, error) { return true, nil }
func (ownerSys) Openers(string) ([]int, error) { return []int{7000}, nil }

func TestCodexClaim_InformBeforeFirstCall_ExpectHeldForThreadAndPullableAtFirstCall(t *testing.T) {
	w := newWorld(t)
	alice := w.claude(4001, "sess-alice", "alice")
	w.codex.SetBinding(adapter.Binding{Harness: adapter.Codex, SessionRef: "T-bob"})
	w.codex.SetDefaultInject(adapter.Outcome{State: adapter.Unreachable, Err: codex.ErrNoEndpoint, NoWrite: true})
	na := w.node(alice.pid, w.lb, nil)
	nb := w.node(7000, w.lb, nil)

	parent, _ := ownerSys{}.Info(7000)
	claims := codex.NewClaims(codex.ClaimOptions{Sys: ownerSys{}, Parent: parent, LockDir: "/locks", Registrar: nb.srv, Sleep: func(time.Duration) {}})
	if err := claims.AcceptDurable(context.Background(), codex.Claim{SessionID: "T-bob", Source: "startup", Cwd: "/work/bob"}); err != nil {
		t.Fatal(err)
	}
	bob := identity.AgentID("codex", "T-bob")
	if _, ok := w.descriptor(bob); !ok {
		t.Fatal("claim did not register the agent")
	}

	res := na.ok(nil, "send_message", send(bob, "sent before bob's first call"))
	if res["state"] != "unreachable" {
		t.Fatalf("send %v", res)
	}

	msgs := nb.ok(thread("T-bob"), "receive_message", map[string]any{})["messages"].([]any)
	var texts []string
	for _, m := range msgs {
		texts = append(texts, fmt.Sprint(m.(map[string]any)["text"]))
	}
	if len(texts) != 1 || texts[0] != "sent before bob's first call" {
		t.Fatalf("mailbox at the first call %v", texts)
	}
	n := 0
	ds, _ := registry.New(w.state).List()
	for _, d := range ds {
		if d.Harness == "codex" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d codex agent records, want 1: %+v", n, ds)
	}
}
