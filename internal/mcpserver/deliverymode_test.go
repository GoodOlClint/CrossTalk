package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const queueWake = "queue-wake (experimental)"

func rowOf(t *testing.T, s *Server, id string) Row {
	t.Helper()
	rows, _ := s.Listing(context.Background())
	for _, r := range rows {
		if r.AgentID == id {
			return r
		}
	}
	t.Fatalf("no row %s in %v", id, rows)
	return Row{}
}

func TestDeliveryModeReachesAnotherShimAndClears(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(Session{Harness: "codex", SessionRef: "tab", Name: "tab", Cwd: "/w", DeliveryMode: queueWake})
	e := newEnv(t, 999999, fb)
	e.mustOK(thread("tab"), "list_agents", nil)
	id := codexID("tab")

	other := New(Options{Backend: NewFakeBackend().AsBackend(), Registry: registry.New(e.stateDir), PPID: 1})
	if got := rowOf(t, other, id).DeliveryMode; got != "" {
		t.Fatalf("mode before the serving shim ticked: %q", got)
	}
	e.srv.syncDeliveryModes(context.Background())
	if got := rowOf(t, other, id).DeliveryMode; got != queueWake {
		t.Fatalf("second shim's row mode = %q, want %q", got, queueWake)
	}

	fb.SetSessions(Session{Harness: "codex", SessionRef: "tab", Name: "tab", Cwd: "/w"})
	e.srv.syncDeliveryModes(context.Background())
	if got := rowOf(t, other, id).DeliveryMode; got != "" {
		t.Fatalf("mode after the claim was released = %q, want cleared", got)
	}
}

func TestDeliveryModeOfDeadOwnerIsNotShown(t *testing.T) {
	fb := NewFakeBackend()
	fb.SetSessions(Session{Harness: "codex", SessionRef: "tab", Name: "tab", Cwd: "/w", DeliveryMode: queueWake})
	e := newEnv(t, 999999, fb)
	e.mustOK(thread("tab"), "list_agents", nil)
	id := codexID("tab")
	e.srv.syncDeliveryModes(context.Background())

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	path := e.reg.DescriptorPath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m["delivery_mode"] != queueWake {
		t.Fatalf("descriptor %v, %v", m, err)
	}
	m["pid"] = cmd.Process.Pid
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	other := New(Options{Backend: NewFakeBackend().AsBackend(), Registry: registry.New(e.stateDir), PPID: 1})
	rows, _ := other.Listing(context.Background())
	for _, r := range rows {
		if r.AgentID == id {
			t.Fatalf("dead owner's row still listed: %+v", r)
		}
	}
}
